package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/lib/pq"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
)

func createAccounts(t *testing.T, s *Store, accs ...ledger.Account) {
	t.Helper()
	for _, a := range accs {
		if err := s.CreateAccount(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
}

func mustPayment(t *testing.T, id string, from, to ledger.AccountID, amount ledger.Amount) ledger.Transfer {
	t.Helper()
	tr, err := ledger.NewPayment(ledger.TransferID(id), "VND", from, to, amount)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func balanceOf(t *testing.T, s *Store, id ledger.AccountID) ledger.Amount {
	t.Helper()
	_, b, err := s.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPostTransfer(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := context.Background()
	createAccounts(t, store,
		ledger.Account{ID: "funding", Currency: "VND", AllowNegative: true},
		ledger.Account{ID: "rider", Currency: "VND"},
		ledger.Account{ID: "driver", Currency: "VND"},
		ledger.Account{ID: "fee", Currency: "VND"},
	)

	if _, err := store.PostTransfer(ctx, mustPayment(t, "topup-1", "funding", "rider", 50_000)); err != nil {
		t.Fatal(err)
	}

	ride, err := ledger.NewTransfer("ride-1", "VND",
		ledger.Entry{AccountID: "rider", Amount: -30_000},
		ledger.Entry{AccountID: "driver", Amount: 24_000},
		ledger.Entry{AccountID: "fee", Amount: 6_000},
	)
	if err != nil {
		t.Fatal(err)
	}
	postings, err := store.PostTransfer(ctx, ride)
	if err != nil {
		t.Fatal(err)
	}
	if len(postings) != 3 || postings[0].NewBalance != 20_000 {
		t.Fatalf("postings = %+v", postings)
	}

	for id, want := range map[ledger.AccountID]ledger.Amount{
		"funding": -50_000, "rider": 20_000, "driver": 24_000, "fee": 6_000,
	} {
		if got := balanceOf(t, store, id); got != want {
			t.Errorf("balance(%s) = %d, want %d", id, got, want)
		}
	}

	lines, err := store.Statement(ctx, "rider", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0].TransferID != "ride-1" || lines[0].Amount != -30_000 {
		t.Fatalf("rider statement = %+v", lines)
	}
}

func TestPostTransferRejectsAndWritesNothing(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()
	createAccounts(t, store,
		ledger.Account{ID: "funding", Currency: "VND", AllowNegative: true},
		ledger.Account{ID: "alice", Currency: "VND"},
		ledger.Account{ID: "bob", Currency: "VND"},
		ledger.Account{ID: "usd-wallet", Currency: "USD"},
	)
	if _, err := store.PostTransfer(ctx, mustPayment(t, "seed", "funding", "alice", 100)); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		tr      ledger.Transfer
		wantErr error
	}{
		{"insufficient funds", mustPayment(t, "t1", "alice", "bob", 101), ledger.ErrInsufficientFunds},
		{"unknown account", mustPayment(t, "t2", "alice", "carol", 1), ledger.ErrAccountNotFound},
		{"currency mismatch", mustPayment(t, "t3", "alice", "usd-wallet", 1), ledger.ErrCurrencyMismatch},
		{"duplicate transfer id", mustPayment(t, "seed", "alice", "bob", 1), ErrTransferExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := store.PostTransfer(ctx, tt.tr); !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}

	// Nothing from the rejected transfers may have been written.
	if got := balanceOf(t, store, "alice"); got != 100 {
		t.Fatalf("alice balance = %d, want 100", got)
	}
	if got := balanceOf(t, store, "bob"); got != 0 {
		t.Fatalf("bob balance = %d, want 0", got)
	}
	var transfers, entries int
	if err := db.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM transfers), (SELECT count(*) FROM entries)`).Scan(&transfers, &entries); err != nil {
		t.Fatal(err)
	}
	if transfers != 1 || entries != 2 {
		t.Fatalf("found %d transfers / %d entries, want only the seed (1 / 2)", transfers, entries)
	}
}

// TestPostTransferOppositeDirectionsDoNotDeadlock runs many A→B and B→A
// transfers at the same time. Without a global lock order this is the
// textbook deadlock; with ordered FOR UPDATE every transfer must succeed.
func TestPostTransferOppositeDirectionsDoNotDeadlock(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := context.Background()
	createAccounts(t, store,
		ledger.Account{ID: "funding", Currency: "VND", AllowNegative: true},
		ledger.Account{ID: "a", Currency: "VND"},
		ledger.Account{ID: "b", Currency: "VND"},
	)
	for _, id := range []ledger.AccountID{"a", "b"} {
		if _, err := store.PostTransfer(ctx, mustPayment(t, "seed-"+string(id), "funding", id, 1_000_000)); err != nil {
			t.Fatal(err)
		}
	}

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		from, to := ledger.AccountID("a"), ledger.AccountID("b")
		if i%2 == 1 {
			from, to = to, from
		}
		tr := mustPayment(t, fmt.Sprintf("swap-%d", i), from, to, 10)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.PostTransfer(ctx, tr); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "40P01" {
			t.Fatalf("deadlock detected: %v", err)
		}
		t.Fatalf("unexpected error: %v", err)
	}
	// 50 transfers each way of the same amount cancel out.
	if a, b := balanceOf(t, store, "a"), balanceOf(t, store, "b"); a != 1_000_000 || b != 1_000_000 {
		t.Fatalf("balances a=%d b=%d, want 1,000,000 each", a, b)
	}
}

// TestPostTransferConcurrentInvariants hammers a small set of wallets with
// random transfers, then checks the ledger is still consistent: money is
// conserved, no customer wallet is negative, and every stored balance equals
// the sum of its entries.
func TestPostTransferConcurrentInvariants(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	wallets := []ledger.AccountID{"w1", "w2", "w3", "w4", "w5"}
	createAccounts(t, store, ledger.Account{ID: "funding", Currency: "VND", AllowNegative: true})
	for _, w := range wallets {
		createAccounts(t, store, ledger.Account{ID: w, Currency: "VND"})
		if _, err := store.PostTransfer(ctx, mustPayment(t, "seed-"+string(w), "funding", w, 1_000)); err != nil {
			t.Fatal(err)
		}
	}

	const workers, perWorker = 8, 40
	rng := rand.New(rand.NewSource(42))
	type job struct{ tr ledger.Transfer }
	jobs := make([][]job, workers)
	for w := range jobs {
		for i := 0; i < perWorker; i++ {
			from := wallets[rng.Intn(len(wallets))]
			to := wallets[rng.Intn(len(wallets))]
			for to == from {
				to = wallets[rng.Intn(len(wallets))]
			}
			amount := ledger.Amount(rng.Intn(400) + 1) // often more than the balance
			jobs[w] = append(jobs[w], job{mustPayment(t, fmt.Sprintf("r-%d-%d", w, i), from, to, amount)})
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for _, js := range jobs {
		wg.Add(1)
		go func(js []job) {
			defer wg.Done()
			for _, j := range js {
				_, err := store.PostTransfer(ctx, j.tr)
				if err != nil && !errors.Is(err, ledger.ErrInsufficientFunds) {
					errs <- err
				}
			}
		}(js)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("unexpected error: %v", err)
	}

	var total ledger.Amount
	for _, w := range wallets {
		b := balanceOf(t, store, w)
		if b < 0 {
			t.Errorf("wallet %s went negative: %d", w, b)
		}
		total += b
	}
	if total != 5*1_000 {
		t.Errorf("money not conserved: wallets hold %d, want %d", total, 5*1_000)
	}

	var mismatched int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM accounts a
		WHERE a.balance <> COALESCE((SELECT SUM(amount) FROM entries e WHERE e.account_id = a.id), 0)`,
	).Scan(&mismatched); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Errorf("%d accounts have a balance that differs from the sum of their entries", mismatched)
	}
}
