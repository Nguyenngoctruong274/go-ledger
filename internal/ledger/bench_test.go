package ledger

import (
	"fmt"
	"testing"
)

// The domain layer runs inside every PostTransfer transaction while row
// locks are held, so it must stay negligible next to the database work.

func BenchmarkNewTransferThreeLegs(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = NewTransfer("ride-42", "VND",
			Entry{"rider", -30_000}, Entry{"driver", 24_000}, Entry{"platform-fee", 6_000})
	}
}

func BenchmarkFingerprint(b *testing.B) {
	tr, _ := NewTransfer("ride-42", "VND",
		Entry{"rider", -30_000}, Entry{"driver", 24_000}, Entry{"platform-fee", 6_000})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tr.Fingerprint()
	}
}

func BenchmarkPlan(b *testing.B) {
	for _, legs := range []int{2, 10} {
		b.Run(fmt.Sprintf("legs=%d", legs), func(b *testing.B) {
			accounts := map[AccountID]Account{}
			balances := map[AccountID]Amount{}
			entries := make([]Entry, legs)
			for i := range entries {
				id := AccountID(fmt.Sprintf("acc-%d", i))
				accounts[id] = Account{ID: id, Currency: "VND"}
				balances[id] = 1_000_000
				entries[i] = Entry{id, 100}
			}
			entries[0].Amount = -100 * Amount(legs-1)
			tr, err := NewTransfer("t", "VND", entries...)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = Plan(tr, accounts, balances)
			}
		})
	}
}
