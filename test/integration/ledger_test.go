//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Nguyenngoctruong274/go-ledger/internal/httpapi"
)

// TestRidePaymentEndToEnd follows one ride through every layer: HTTP in,
// balances and statement out, and the signed event at the webhook.
func TestRidePaymentEndToEnd(t *testing.T) {
	s := newStack(t)
	s.createAccount("funding", "VND", true)
	s.createAccount("rider", "VND", false)
	s.createAccount("driver", "VND", false)
	s.createAccount("platform-fee", "VND", false)

	s.mustCall(http.StatusCreated, "POST", "/v1/transfers",
		`{"currency":"VND","from":"funding","to":"rider","amount":50000}`, httpapi.IdempotencyKeyHeader, "topup-1")
	s.mustCall(http.StatusCreated, "POST", "/v1/transfers",
		`{"currency":"VND","entries":[
			{"account_id":"rider","amount":-30000},
			{"account_id":"driver","amount":24000},
			{"account_id":"platform-fee","amount":6000}]}`,
		httpapi.IdempotencyKeyHeader, "ride-42")

	for id, want := range map[string]int64{"funding": -50000, "rider": 20000, "driver": 24000, "platform-fee": 6000} {
		if got := s.balance(id); got != want {
			t.Errorf("balance(%s) = %d, want %d", id, got, want)
		}
	}

	st := s.statement("rider")
	if len(st) != 2 || st[0].TransferID != "topup-1" || st[1].TransferID != "ride-42" || st[1].BalanceAfter != 20000 {
		t.Fatalf("rider statement = %+v", st)
	}

	evs := s.waitForEvents(2)
	if evs[0].Event.TransferID != "topup-1" || evs[1].Event.TransferID != "ride-42" {
		t.Fatalf("events out of order: %+v", evs)
	}
	ride := evs[1].Event
	if ride.Currency != "VND" || len(ride.Entries) != 3 || ride.Entries[0].BalanceAfter != 20000 || ride.PostedAt.IsZero() {
		t.Fatalf("ride event = %+v", ride)
	}
}

// TestRetriesAndConflictsEndToEnd checks idempotency as a client sees it:
// headers, bodies, balances and the number of events.
func TestRetriesAndConflictsEndToEnd(t *testing.T) {
	s := newStack(t)
	s.createAccount("funding", "VND", true)
	s.createAccount("alice", "VND", false)
	s.createAccount("bob", "VND", false)
	s.mustCall(http.StatusCreated, "POST", "/v1/transfers",
		`{"currency":"VND","from":"funding","to":"alice","amount":100}`, httpapi.IdempotencyKeyHeader, "seed")

	first := s.pay("pay-1", "alice", "bob", 100)
	if first.Status != http.StatusCreated {
		t.Fatalf("first: %d %s", first.Status, first.Body)
	}
	// Alice is now at 0; a real second charge would be refused for funds.
	retry := s.pay("pay-1", "alice", "bob", 100)
	if retry.Status != http.StatusOK || retry.Header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(retry.Body, first.Body) {
		t.Fatalf("retry: %d %v %s", retry.Status, retry.Header, retry.Body)
	}

	conflict := s.pay("pay-1", "alice", "bob", 99)
	if conflict.Status != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: %d %s", conflict.Status, conflict.Body)
	}
	broke := s.pay("pay-2", "alice", "bob", 1)
	if broke.Status != http.StatusPaymentRequired {
		t.Fatalf("insufficient funds: %d %s", broke.Status, broke.Body)
	}

	if a, b := s.balance("alice"), s.balance("bob"); a != 0 || b != 100 {
		t.Fatalf("balances alice=%d bob=%d, want 0 and 100", a, b)
	}

	// seed + pay-1 only: no event for the replay, the conflict or the refusal.
	s.waitForEvents(2)
	time.Sleep(300 * time.Millisecond) // give a wrong extra event time to show up
	if evs := s.hook.snapshot(); len(evs) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(evs), evs)
	}
}

// TestWebhookOutageEndToEnd makes the receiver fail for a while. Every event
// must still arrive exactly once and in order once it recovers, and the
// failures must be visible on the outbox rows.
func TestWebhookOutageEndToEnd(t *testing.T) {
	s := newStack(t)
	s.hook.mu.Lock()
	s.hook.failNext = 5
	s.hook.mu.Unlock()

	s.createAccount("funding", "VND", true)
	s.createAccount("w", "VND", false)
	for i := 1; i <= 4; i++ {
		if r := s.pay(fmt.Sprintf("t%d", i), "funding", "w", int64(i)); r.Status != http.StatusCreated {
			t.Fatalf("t%d: %d %s", i, r.Status, r.Body)
		}
	}

	evs := s.waitForEvents(4)
	for i, e := range evs {
		if want := fmt.Sprintf("t%d", i+1); e.Event.TransferID != want {
			t.Fatalf("event %d = %s, want %s (order broken by retries)", i, e.Event.TransferID, want)
		}
	}
	if s.hook.failures != 5 {
		t.Fatalf("receiver refused %d requests, want 5", s.hook.failures)
	}
	var retried int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM outbox WHERE attempts > 0 AND published_at IS NOT NULL`).Scan(&retried); err != nil {
		t.Fatal(err)
	}
	if retried == 0 {
		t.Fatal("no outbox row records a failed attempt")
	}
}

// TestConcurrentClientsEndToEnd is the load-shaped check: many clients send
// random payments over HTTP, and every request is sent twice at the same
// moment, as a client with an aggressive retry would. Afterwards the books
// must balance through every public view: balances, statements and events.
func TestConcurrentClientsEndToEnd(t *testing.T) {
	s := newStack(t)
	wallets := []string{"w1", "w2", "w3", "w4", "w5"}
	s.createAccount("funding", "VND", true)
	for _, w := range wallets {
		s.createAccount(w, "VND", false)
		if r := s.pay("seed-"+w, "funding", w, 10_000); r.Status != http.StatusCreated {
			t.Fatalf("seed %s: %d", w, r.Status)
		}
	}

	const clients, perClient = 8, 25
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		posted  = map[string]bool{}
		created = 0
	)
	for c := 0; c < clients; c++ {
		rng := rand.New(rand.NewSource(int64(c)))
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for i := 0; i < perClient; i++ {
				from := wallets[rng.Intn(len(wallets))]
				to := wallets[rng.Intn(len(wallets))]
				for to == from {
					to = wallets[rng.Intn(len(wallets))]
				}
				key := fmt.Sprintf("c%d-%d", c, i)
				amount := int64(rng.Intn(4000) + 1)

				// The same request twice, concurrently.
				var inner sync.WaitGroup
				results := make([]response, 2)
				for k := range results {
					inner.Add(1)
					go func(k int) { defer inner.Done(); results[k] = s.pay(key, from, to, amount) }(k)
				}
				inner.Wait()

				mu.Lock()
				switch {
				case results[0].Status == http.StatusPaymentRequired && results[1].Status == http.StatusPaymentRequired:
					// refused for funds; both copies agree
				case (results[0].Status == http.StatusCreated) != (results[1].Status == http.StatusCreated):
					// exactly one copy posted, the other must be its replay
					replay := results[0]
					if replay.Status == http.StatusCreated {
						replay = results[1]
					}
					if replay.Status != http.StatusOK || replay.Header.Get("Idempotent-Replayed") != "true" {
						t.Errorf("%s: unexpected pair %d / %d", key, results[0].Status, results[1].Status)
					}
					posted[key] = true
					created++
				default:
					t.Errorf("%s: unexpected pair %d / %d: %s | %s",
						key, results[0].Status, results[1].Status, results[0].Body, results[1].Body)
				}
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	t.Logf("%d of %d payments posted, the rest refused for funds", created, clients*perClient)

	// 1. Money is conserved and no wallet went negative.
	var total int64
	for _, w := range wallets {
		b := s.balance(w)
		if b < 0 {
			t.Errorf("wallet %s is negative: %d", w, b)
		}
		total += b
	}
	if total != int64(len(wallets))*10_000 {
		t.Fatalf("wallets hold %d, want %d", total, len(wallets)*10_000)
	}

	// 2. Each balance equals the last running balance in its statement, and
	//    the running balances add up entry by entry.
	for _, w := range wallets {
		st := s.statement(w)
		var running int64
		for _, e := range st {
			running += e.Amount
			if e.BalanceAfter != running {
				t.Fatalf("%s entry %d: balance_after %d, running sum %d", w, e.EntryID, e.BalanceAfter, running)
			}
		}
		if running != s.balance(w) {
			t.Fatalf("%s: statement sums to %d, balance is %d", w, running, s.balance(w))
		}
	}

	// 3. Exactly one event per posted transfer (seeds included), and for
	//    every wallet the events report its balances in statement order.
	want := created + len(wallets)
	s.waitForEvents(want)
	time.Sleep(300 * time.Millisecond)
	evs := s.hook.snapshot()
	if len(evs) != want {
		t.Fatalf("got %d events, want %d", len(evs), want)
	}
	seen := map[string]bool{}
	perAccount := map[string][]int64{}
	for _, e := range evs {
		if seen[e.Event.TransferID] {
			t.Fatalf("duplicate event for %s", e.Event.TransferID)
		}
		seen[e.Event.TransferID] = true
		for _, en := range e.Event.Entries {
			perAccount[en.AccountID] = append(perAccount[en.AccountID], en.BalanceAfter)
		}
	}
	for key := range posted {
		if !seen[key] {
			t.Fatalf("no event for posted transfer %s", key)
		}
	}
	for _, w := range wallets {
		st := s.statement(w)
		got := perAccount[w]
		if len(got) != len(st) {
			t.Fatalf("%s: %d events vs %d statement entries", w, len(got), len(st))
		}
		for i := range st {
			if got[i] != st[i].BalanceAfter {
				t.Fatalf("%s: event #%d says balance %d, statement says %d (events out of order)",
					w, i, got[i], st[i].BalanceAfter)
			}
		}
	}
}
