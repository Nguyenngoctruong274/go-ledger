package postgres

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
	"github.com/Nguyenngoctruong274/go-ledger/internal/testdb"
)

// benchStore prepares a funding account and n wallets with plenty of money.
func benchStore(b *testing.B, wallets int) *Store {
	b.Helper()
	store := NewStore(testdb.New(b, Migrate))
	ctx := context.Background()
	if err := store.CreateAccount(ctx, ledger.Account{ID: "funding", Currency: "VND", AllowNegative: true}); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < wallets; i++ {
		id := ledger.AccountID(fmt.Sprintf("w%d", i))
		if err := store.CreateAccount(ctx, ledger.Account{ID: id, Currency: "VND"}); err != nil {
			b.Fatal(err)
		}
		tr, _ := ledger.NewPayment(ledger.TransferID("seed-"+string(id)), "VND", "funding", id, 1<<40)
		if _, err := store.PostTransfer(ctx, tr); err != nil {
			b.Fatal(err)
		}
	}
	return store
}

var benchSeq atomic.Int64

func nextID(prefix string) ledger.TransferID {
	return ledger.TransferID(fmt.Sprintf("%s-%d", prefix, benchSeq.Add(1)))
}

// BenchmarkPostTransfer measures the full transactional path (claim id,
// lock rows, plan, insert entries, update balances, write outbox, commit).
func BenchmarkPostTransfer(b *testing.B) {
	ctx := context.Background()

	b.Run("serial", func(b *testing.B) {
		store := benchStore(b, 2)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tr, _ := ledger.NewPayment(nextID("s"), "VND", "w0", "w1", 1)
			if _, err := store.PostTransfer(ctx, tr); err != nil {
				b.Fatal(err)
			}
		}
	})

	// Many disjoint pairs: transactions rarely wait on each other's locks.
	b.Run("parallel/disjoint-accounts", func(b *testing.B) {
		const wallets = 64
		store := benchStore(b, wallets)
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				n := benchSeq.Add(1)
				from := ledger.AccountID(fmt.Sprintf("w%d", (n*2)%wallets))
				to := ledger.AccountID(fmt.Sprintf("w%d", (n*2+1)%wallets))
				tr, _ := ledger.NewPayment(nextID("d"), "VND", from, to, 1)
				if _, err := store.PostTransfer(ctx, tr); err != nil {
					b.Error(err)
					return
				}
			}
		})
	})

	// Every transfer touches the same account: the row lock serialises them.
	// This is the ceiling for a single "hot" wallet, e.g. a platform fee account.
	b.Run("parallel/hot-account", func(b *testing.B) {
		const wallets = 16
		store := benchStore(b, wallets)
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				n := benchSeq.Add(1)
				from := ledger.AccountID(fmt.Sprintf("w%d", 1+n%(wallets-1)))
				tr, _ := ledger.NewPayment(nextID("h"), "VND", from, "w0", 1)
				if _, err := store.PostTransfer(ctx, tr); err != nil {
					b.Error(err)
					return
				}
			}
		})
	})

	// A retry is answered from the stored result without taking locks.
	b.Run("replay", func(b *testing.B) {
		store := benchStore(b, 2)
		tr, _ := ledger.NewPayment("replay-me", "VND", "w0", "w1", 1)
		if _, err := store.PostTransfer(ctx, tr); err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := store.PostTransfer(ctx, tr); err != nil {
				b.Fatal(err)
			}
		}
	})
}
