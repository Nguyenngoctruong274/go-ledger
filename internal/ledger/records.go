package ledger

import (
	"errors"
	"time"
)

// Errors reported by stores. They live here, next to the other ledger
// errors, so callers such as the HTTP layer can map them without
// depending on a particular storage package.
var (
	ErrAccountExists       = errors.New("account already exists")
	ErrIdempotencyConflict = errors.New("transfer id already used for a different request")
)

// PostedEntry is one leg of a posted transfer and the balance it produced.
type PostedEntry struct {
	AccountID    AccountID
	Amount       Amount
	BalanceAfter Amount
}

// Receipt describes a posted transfer.
type Receipt struct {
	TransferID TransferID
	Currency   Currency
	Entries    []PostedEntry
	CreatedAt  time.Time
	// Replayed is true when this call did not move money because the same
	// request had already been posted; the receipt is the original one.
	Replayed bool
}

// StatementLine is one entry as seen from an account statement.
type StatementLine struct {
	EntryID      int64
	TransferID   TransferID
	Amount       Amount
	BalanceAfter Amount
	CreatedAt    time.Time
}
