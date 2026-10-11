package ledger

import (
	"errors"
	"fmt"
)

// AccountID identifies an account.
type AccountID string

// TransferID identifies a transfer. It doubles as the idempotency handle
// for clients, so it is chosen by the caller rather than generated here.
type TransferID string

// Entry is one leg of a transfer: a signed amount posted to one account.
type Entry struct {
	AccountID AccountID
	Amount    Amount
}

// Transfer is a balanced, validated set of entries in a single currency.
// The zero value is not a valid transfer; use NewTransfer or NewPayment.
type Transfer struct {
	id       TransferID
	currency Currency
	entries  []Entry
}

// Validation errors returned by NewTransfer.
var (
	ErrMissingID        = errors.New("missing id")
	ErrTooFewEntries    = errors.New("transfer needs at least two entries")
	ErrZeroAmount       = errors.New("entry amount must be non-zero")
	ErrDuplicateAccount = errors.New("account appears in more than one entry")
	ErrUnbalanced       = errors.New("entries do not sum to zero")
)

// NewTransfer validates entries and returns a balanced Transfer.
//
// The rules are: a non-empty id, a valid currency, at least two entries,
// every entry has a non-empty account and a non-zero amount, each account
// appears at most once, and the amounts sum to exactly zero.
func NewTransfer(id TransferID, currency Currency, entries ...Entry) (Transfer, error) {
	if id == "" {
		return Transfer{}, fmt.Errorf("transfer: %w", ErrMissingID)
	}
	if _, err := ParseCurrency(string(currency)); err != nil {
		return Transfer{}, fmt.Errorf("transfer %s: %w", id, err)
	}
	if len(entries) < 2 {
		return Transfer{}, fmt.Errorf("transfer %s: %w", id, ErrTooFewEntries)
	}

	seen := make(map[AccountID]struct{}, len(entries))
	var sum Amount
	for i, e := range entries {
		if e.AccountID == "" {
			return Transfer{}, fmt.Errorf("transfer %s: entry %d: account %w", id, i, ErrMissingID)
		}
		if e.Amount == 0 {
			return Transfer{}, fmt.Errorf("transfer %s: entry %d: %w", id, i, ErrZeroAmount)
		}
		if _, dup := seen[e.AccountID]; dup {
			return Transfer{}, fmt.Errorf("transfer %s: %w: %s", id, ErrDuplicateAccount, e.AccountID)
		}
		seen[e.AccountID] = struct{}{}

		var err error
		if sum, err = add(sum, e.Amount); err != nil {
			return Transfer{}, fmt.Errorf("transfer %s: %w", id, err)
		}
	}
	if sum != 0 {
		return Transfer{}, fmt.Errorf("transfer %s: %w (off by %d)", id, ErrUnbalanced, sum)
	}

	return Transfer{id: id, currency: currency, entries: append([]Entry(nil), entries...)}, nil
}

// ErrNonPositiveAmount reports a payment amount that is zero or negative.
var ErrNonPositiveAmount = errors.New("amount must be positive")

// NewPayment is the common two-entry case: move amount from one account to another.
func NewPayment(id TransferID, currency Currency, from, to AccountID, amount Amount) (Transfer, error) {
	if amount <= 0 {
		return Transfer{}, fmt.Errorf("transfer %s: %w", id, ErrNonPositiveAmount)
	}
	return NewTransfer(id, currency,
		Entry{AccountID: from, Amount: -amount},
		Entry{AccountID: to, Amount: amount},
	)
}

// ID returns the transfer id.
func (t Transfer) ID() TransferID { return t.id }

// Currency returns the transfer currency.
func (t Transfer) Currency() Currency { return t.currency }

// Entries returns a copy of the transfer entries, so callers cannot
// unbalance a validated transfer by mutating the slice.
func (t Transfer) Entries() []Entry { return append([]Entry(nil), t.entries...) }
