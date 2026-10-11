package ledger

import (
	"errors"
	"math"
	"testing"
)

func TestPlan(t *testing.T) {
	accounts := map[AccountID]Account{
		"alice":   {ID: "alice", Currency: vnd},
		"bob":     {ID: "bob", Currency: vnd},
		"funding": {ID: "funding", Currency: vnd, AllowNegative: true},
		"usd":     {ID: "usd", Currency: "USD"},
	}

	tests := []struct {
		name     string
		from, to AccountID
		amount   Amount
		balances map[AccountID]Amount
		want     []Amount // new balances in entry order
		wantErr  error
	}{
		{
			name: "enough funds", from: "alice", to: "bob", amount: 300,
			balances: map[AccountID]Amount{"alice": 1000, "bob": 50},
			want:     []Amount{700, 350},
		},
		{
			name: "spend exact balance", from: "alice", to: "bob", amount: 1000,
			balances: map[AccountID]Amount{"alice": 1000},
			want:     []Amount{0, 1000},
		},
		{
			name: "insufficient funds", from: "alice", to: "bob", amount: 1001,
			balances: map[AccountID]Amount{"alice": 1000},
			wantErr:  ErrInsufficientFunds,
		},
		{
			name: "system account may go negative", from: "funding", to: "alice", amount: 5000,
			balances: map[AccountID]Amount{},
			want:     []Amount{-5000, 5000},
		},
		{
			name: "unknown account", from: "alice", to: "carol", amount: 1,
			balances: map[AccountID]Amount{"alice": 10},
			wantErr:  ErrAccountNotFound,
		},
		{
			name: "currency mismatch", from: "alice", to: "usd", amount: 1,
			balances: map[AccountID]Amount{"alice": 10},
			wantErr:  ErrCurrencyMismatch,
		},
		{
			name: "balance overflow", from: "funding", to: "bob", amount: 1,
			balances: map[AccountID]Amount{"bob": math.MaxInt64},
			wantErr:  ErrAmountOverflow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, err := NewPayment("t", vnd, tt.from, tt.to, tt.amount)
			if err != nil {
				t.Fatal(err)
			}
			postings, err := Plan(tr, accounts, tt.balances)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(postings) != len(tt.want) {
				t.Fatalf("got %d postings, want %d", len(postings), len(tt.want))
			}
			for i, p := range postings {
				if p.NewBalance != tt.want[i] {
					t.Fatalf("posting %d new balance = %d, want %d", i, p.NewBalance, tt.want[i])
				}
			}
		})
	}
}

func TestPlanDoesNotMutateBalances(t *testing.T) {
	accounts := map[AccountID]Account{"a": {ID: "a", Currency: vnd}, "b": {ID: "b", Currency: vnd}}
	balances := map[AccountID]Amount{"a": 10}
	tr, _ := NewPayment("t", vnd, "a", "b", 10)

	if _, err := Plan(tr, accounts, balances); err != nil {
		t.Fatal(err)
	}
	if balances["a"] != 10 || len(balances) != 1 {
		t.Fatalf("Plan mutated balances: %v", balances)
	}
}

// TestPlanConservesMoney checks that applying a valid plan never creates or
// destroys money: the total across all accounts is unchanged.
func TestPlanConservesMoney(t *testing.T) {
	accounts := map[AccountID]Account{
		"rider": {ID: "rider", Currency: vnd}, "driver": {ID: "driver", Currency: vnd},
		"fee": {ID: "fee", Currency: vnd},
	}
	balances := map[AccountID]Amount{"rider": 50_000, "driver": 1_000, "fee": 0}
	tr, err := NewTransfer("ride-42", vnd,
		Entry{"rider", -30_000}, Entry{"driver", 24_000}, Entry{"fee", 6_000})
	if err != nil {
		t.Fatal(err)
	}

	postings, err := Plan(tr, accounts, balances)
	if err != nil {
		t.Fatal(err)
	}
	var before, after Amount
	for _, b := range balances {
		before += b
	}
	for _, p := range postings {
		after += p.NewBalance
	}
	if before != after {
		t.Fatalf("money not conserved: before %d, after %d", before, after)
	}
}
