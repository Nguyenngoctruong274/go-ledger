package ledger

import (
	"errors"
	"math"
	"testing"
)

const vnd Currency = "VND"

func TestParseCurrency(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"VND", false},
		{"USD", false},
		{"vnd", true},
		{"VN", true},
		{"VNDX", true},
		{"V1D", true},
		{"", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			_, err := ParseCurrency(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseCurrency(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidCurrency) {
				t.Fatalf("error %v is not ErrInvalidCurrency", err)
			}
		})
	}
}

func TestNewTransfer(t *testing.T) {
	tests := []struct {
		name     string
		id       TransferID
		currency Currency
		entries  []Entry
		wantErr  error
	}{
		{
			name: "two-leg payment", id: "t1", currency: vnd,
			entries: []Entry{{"alice", -500}, {"bob", 500}},
		},
		{
			name: "split payment with fee", id: "t2", currency: vnd,
			entries: []Entry{{"rider", -10_000}, {"driver", 8_000}, {"platform-fee", 2_000}},
		},
		{name: "missing id", currency: vnd, entries: []Entry{{"a", -1}, {"b", 1}}, wantErr: ErrMissingID},
		{name: "bad currency", id: "t", currency: "dong", entries: []Entry{{"a", -1}, {"b", 1}}, wantErr: ErrInvalidCurrency},
		{name: "single entry", id: "t", currency: vnd, entries: []Entry{{"a", 0}}, wantErr: ErrTooFewEntries},
		{name: "no entries", id: "t", currency: vnd, wantErr: ErrTooFewEntries},
		{name: "zero amount", id: "t", currency: vnd, entries: []Entry{{"a", 0}, {"b", 0}}, wantErr: ErrZeroAmount},
		{name: "missing account", id: "t", currency: vnd, entries: []Entry{{"", -1}, {"b", 1}}, wantErr: ErrMissingID},
		{name: "duplicate account", id: "t", currency: vnd, entries: []Entry{{"a", -1}, {"a", 1}}, wantErr: ErrDuplicateAccount},
		{name: "unbalanced", id: "t", currency: vnd, entries: []Entry{{"a", -100}, {"b", 99}}, wantErr: ErrUnbalanced},
		{
			name: "overflow", id: "t", currency: vnd,
			entries: []Entry{{"a", math.MaxInt64}, {"b", 1}, {"c", math.MinInt64}},
			wantErr: ErrAmountOverflow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, err := NewTransfer(tt.id, tt.currency, tt.entries...)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tr.ID() != tt.id || tr.Currency() != tt.currency || len(tr.Entries()) != len(tt.entries) {
				t.Fatalf("got %+v", tr)
			}
		})
	}
}

func TestTransferEntriesIsACopy(t *testing.T) {
	in := []Entry{{"a", -1}, {"b", 1}}
	tr, err := NewTransfer("t", vnd, in...)
	if err != nil {
		t.Fatal(err)
	}

	in[0].Amount = -1000 // caller mutates its input
	out := tr.Entries()
	out[1].Amount = 1000 // caller mutates the returned slice

	if got := tr.Entries(); got[0].Amount != -1 || got[1].Amount != 1 {
		t.Fatalf("transfer was mutated through a shared slice: %+v", got)
	}
}

func TestNewPayment(t *testing.T) {
	tr, err := NewPayment("p1", vnd, "alice", "bob", 25_000)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{"alice", -25_000}, {"bob", 25_000}}
	got := tr.Entries()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	for _, amount := range []Amount{0, -1} {
		if _, err := NewPayment("p", vnd, "a", "b", amount); !errors.Is(err, ErrNonPositiveAmount) {
			t.Fatalf("amount %d: error = %v, want ErrNonPositiveAmount", amount, err)
		}
	}
	if _, err := NewPayment("p", vnd, "a", "a", 1); !errors.Is(err, ErrDuplicateAccount) {
		t.Fatalf("self-payment: error = %v, want ErrDuplicateAccount", err)
	}
}

// FuzzNewTransfer checks the core invariant: whatever the input, a transfer
// that NewTransfer accepts has non-zero entries that sum to exactly zero.
func FuzzNewTransfer(f *testing.F) {
	f.Add(int64(-100), int64(60), int64(40))
	f.Add(int64(math.MaxInt64), int64(math.MinInt64), int64(1))
	f.Add(int64(0), int64(5), int64(-5))

	f.Fuzz(func(t *testing.T, a, b, c int64) {
		tr, err := NewTransfer("fuzz", vnd,
			Entry{"a", Amount(a)}, Entry{"b", Amount(b)}, Entry{"c", Amount(c)})
		if err != nil {
			return
		}
		var sum Amount
		for _, e := range tr.Entries() {
			if e.Amount == 0 {
				t.Fatalf("accepted zero entry: %+v", tr.Entries())
			}
			sum += e.Amount
		}
		if sum != 0 {
			t.Fatalf("accepted unbalanced transfer: %+v", tr.Entries())
		}
	})
}
