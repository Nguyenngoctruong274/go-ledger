package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
)

// openTestDB returns a database handle scoped to a fresh schema with all
// migrations applied. Tests that need it are skipped unless
// LEDGER_TEST_DATABASE_URL points at a disposable PostgreSQL database.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := Open(ctx, base)
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

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	db, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("schema_migrations has %d rows, want 1", n)
	}
}

func TestCreateAndGetAccount(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := context.Background()

	want := ledger.Account{ID: "wallet-1", Currency: "VND"}
	if err := store.CreateAccount(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, balance, err := store.GetAccount(ctx, "wallet-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != want || balance != 0 {
		t.Fatalf("got %+v balance %d, want %+v balance 0", got, balance, want)
	}

	if err := store.CreateAccount(ctx, want); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("duplicate create: error = %v, want ErrAccountExists", err)
	}
	if _, _, err := store.GetAccount(ctx, "missing"); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatalf("missing account: error = %v, want ErrAccountNotFound", err)
	}
	if err := store.CreateAccount(ctx, ledger.Account{ID: "x", Currency: "vnd"}); !errors.Is(err, ledger.ErrInvalidCurrency) {
		t.Fatalf("bad currency: error = %v, want ErrInvalidCurrency", err)
	}
}

// insertTransfer writes a transfer with raw SQL so the schema's own
// safeguards can be tested before the Go posting path exists.
func insertTransfer(ctx context.Context, db *sql.DB, id string, entries map[string]int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `INSERT INTO transfers (id, currency) VALUES ($1, 'VND')`, id); err != nil {
		return err
	}
	for acc, amount := range entries {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entries (transfer_id, account_id, amount) VALUES ($1, $2, $3)`, id, acc, amount); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func TestSchemaRejectsUnbalancedTransfer(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()
	for _, id := range []ledger.AccountID{"a", "b"} {
		if err := store.CreateAccount(ctx, ledger.Account{ID: id, Currency: "VND", AllowNegative: true}); err != nil {
			t.Fatal(err)
		}
	}

	err := insertTransfer(ctx, db, "bad", map[string]int64{"a": -100, "b": 99})
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || pqErr.Code != "23514" {
		t.Fatalf("unbalanced transfer: error = %v, want check_violation", err)
	}

	if err := insertTransfer(ctx, db, "good", map[string]int64{"a": -100, "b": 100}); err != nil {
		t.Fatalf("balanced transfer: %v", err)
	}
}

func TestSchemaEntriesAreAppendOnly(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()
	for _, id := range []ledger.AccountID{"a", "b"} {
		if err := store.CreateAccount(ctx, ledger.Account{ID: id, Currency: "VND", AllowNegative: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := insertTransfer(ctx, db, "t1", map[string]int64{"a": -5, "b": 5}); err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []string{
		`UPDATE entries SET amount = 6 WHERE account_id = 'b'`,
		`DELETE FROM entries WHERE transfer_id = 't1'`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Fatalf("%q succeeded, want append-only error", stmt)
		}
	}
}

func TestSchemaRejectsCustomerOverdraft(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()
	if err := store.CreateAccount(ctx, ledger.Account{ID: "wallet", Currency: "VND"}); err != nil {
		t.Fatal(err)
	}

	_, err := db.ExecContext(ctx, `UPDATE accounts SET balance = -1 WHERE id = 'wallet'`)
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || pqErr.Constraint != "accounts_balance_non_negative" {
		t.Fatalf("overdraft: error = %v, want accounts_balance_non_negative violation", err)
	}
}

func TestStatementPagination(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()
	for _, acc := range []ledger.Account{
		{ID: "funding", Currency: "VND", AllowNegative: true},
		{ID: "wallet", Currency: "VND"},
	} {
		if err := store.CreateAccount(ctx, acc); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range []string{"t1", "t2", "t3"} {
		amount := int64(1000 * (i + 1))
		if err := insertTransfer(ctx, db, id, map[string]int64{"funding": -amount, "wallet": amount}); err != nil {
			t.Fatal(err)
		}
	}

	page1, err := store.Statement(ctx, "wallet", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || page1[0].TransferID != "t3" || page1[1].TransferID != "t2" {
		t.Fatalf("page 1 = %+v, want t3, t2", page1)
	}

	page2, err := store.Statement(ctx, "wallet", page1[1].EntryID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].TransferID != "t1" || page2[0].Amount != 1000 {
		t.Fatalf("page 2 = %+v, want t1 (+1000)", page2)
	}

	if _, err := store.Statement(ctx, "wallet", 0, 0); err == nil {
		t.Fatal("limit 0 accepted")
	}
}
