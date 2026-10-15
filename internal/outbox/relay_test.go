package outbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/Nguyenngoctruong274/go-ledger/internal/postgres"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// openTestDB returns a migrated database scoped to a fresh schema, or skips
// the test when LEDGER_TEST_DATABASE_URL is not set.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping PostgreSQL tests")
	}
	ctx := context.Background()
	admin, err := postgres.Open(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	schema := "test_" + hex.EncodeToString(buf)
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+pq.QuoteIdentifier(schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+pq.QuoteIdentifier(schema)+" CASCADE")
	})

	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := postgres.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := postgres.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func enqueue(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := db.ExecContext(context.Background(), `INSERT INTO outbox (topic, key, payload) VALUES ('test', $1, $2)`,
			fmt.Sprintf("k%d", i), fmt.Sprintf(`{"n":%d}`, i)); err != nil {
			t.Fatal(err)
		}
	}
}

// recorder is a Publisher that records what it receives and can be told to
// fail for specific message keys.
type recorder struct {
	mu     sync.Mutex
	got    []Message
	failOn map[string]int // key -> remaining failures
}

func (r *recorder) Publish(_ context.Context, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failOn[m.Key] > 0 {
		r.failOn[m.Key]--
		return errors.New("broker unavailable")
	}
	r.got = append(r.got, m)
	return nil
}

func (r *recorder) keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.got))
	for i, m := range r.got {
		out[i] = m.Key
	}
	return out
}

func pendingCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRelayPublishesInOrder(t *testing.T) {
	db := openTestDB(t)
	enqueue(t, db, 5)
	rec := &recorder{}
	relay := NewRelay(db, rec, discard(), Options{BatchSize: 2})

	total := 0
	for {
		n, err := relay.ProcessBatch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		total += n
	}
	if total != 5 || fmt.Sprint(rec.keys()) != "[k1 k2 k3 k4 k5]" {
		t.Fatalf("published %d: %v", total, rec.keys())
	}
	if p := pendingCount(t, db); p != 0 {
		t.Fatalf("%d messages still pending", p)
	}
	if string(rec.got[0].Payload) != `{"n": 1}` && string(rec.got[0].Payload) != `{"n":1}` {
		t.Fatalf("payload = %s", rec.got[0].Payload)
	}
}

func TestRelayRetriesWithBackoffAndKeepsOrder(t *testing.T) {
	db := openTestDB(t)
	enqueue(t, db, 3)
	rec := &recorder{failOn: map[string]int{"k2": 2}}
	relay := NewRelay(db, rec, discard(), Options{BaseBackoff: 100 * time.Millisecond, MaxBackoff: time.Second})
	ctx := context.Background()

	// Round 1: k1 goes out, k2 fails, k3 waits behind k2.
	if n, err := relay.ProcessBatch(ctx); err != nil || n != 1 {
		t.Fatalf("round 1: n=%d err=%v", n, err)
	}
	var attempts int
	var lastErr string
	if err := db.QueryRowContext(context.Background(), `SELECT attempts, last_error FROM outbox WHERE key = 'k2'`).Scan(&attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastErr != "broker unavailable" {
		t.Fatalf("k2 attempts=%d last_error=%q", attempts, lastErr)
	}

	// Inside k2's backoff window nothing may be published: k3 is due, but
	// it must not overtake k2.
	if n, err := relay.ProcessBatch(ctx); err != nil || n != 0 {
		t.Fatalf("during backoff: n=%d err=%v, want nothing published", n, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for pendingCount(t, db) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("messages still pending; published %v", rec.keys())
		}
		if _, err := relay.ProcessBatch(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := fmt.Sprint(rec.keys()); got != "[k1 k2 k3]" {
		t.Fatalf("published %s, want [k1 k2 k3]", got)
	}
	if err := db.QueryRowContext(context.Background(), `SELECT attempts FROM outbox WHERE key = 'k2'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("k2 attempts = %d, want 2 failures recorded", attempts)
	}
}

// TestConcurrentRelaysPublishOnceInOrder runs several relays (as if on
// several replicas) over the same table. The advisory lock must let only
// one publish at a time: every message exactly once, in id order.
func TestConcurrentRelaysPublishOnceInOrder(t *testing.T) {
	db := openTestDB(t)
	const n = 300
	enqueue(t, db, n)
	rec := &recorder{}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		relay := NewRelay(db, rec, discard(), Options{BatchSize: 10})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pendingCount(t, db) > 0 {
				if _, err := relay.ProcessBatch(context.Background()); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	keys := rec.keys()
	if len(keys) != n {
		t.Fatalf("published %d messages, want %d (duplicates or losses)", len(keys), n)
	}
	for i, k := range keys {
		if want := fmt.Sprintf("k%d", i+1); k != want {
			t.Fatalf("position %d: got %s, want %s", i, k, want)
		}
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	db := openTestDB(t)
	enqueue(t, db, 3)
	rec := &recorder{}
	relay := NewRelay(db, rec, discard(), Options{PollInterval: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(rec.keys()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if len(rec.keys()) != 3 {
		t.Fatalf("published %v", rec.keys())
	}
}

func TestBackoff(t *testing.T) {
	r := NewRelay(nil, nil, discard(), Options{BaseBackoff: time.Second, MaxBackoff: 10 * time.Second})
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, w := range want {
		if got := r.backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}
