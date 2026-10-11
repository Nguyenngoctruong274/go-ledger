package ledger

import (
	"errors"
	"fmt"
)

// Account is a ledger account. Its balance is not stored here: it is the
// sum of all entries posted to the account, which the store maintains.
type Account struct {
	ID       AccountID
	Currency Currency
	// AllowNegative is true for system accounts (e.g. external funding or
	// settlement accounts) that may go below zero. Customer wallets keep it false.
	AllowNegative bool
}

// Errors returned by Plan.
var (
	ErrAccountNotFound   = errors.New("account not found")
	ErrCurrencyMismatch  = errors.New("currency mismatch")
	ErrInsufficientFunds = errors.New("insufficient funds")
)

// Posting is the effect of a transfer on one account.
type Posting struct {
	Account    Account
	Amount     Amount
	NewBalance Amount
}

// Plan checks that a transfer can be applied to the given accounts and
// current balances, and returns the resulting postings in entry order.
// It does not mutate anything; the store applies the postings inside the
// same database transaction that read the balances.
func Plan(t Transfer, accounts map[AccountID]Account, balances map[AccountID]Amount) ([]Posting, error) {
	postings := make([]Posting, 0, len(t.entries))
	for _, e := range t.entries {
		acc, ok := accounts[e.AccountID]
		if !ok {
			return nil, fmt.Errorf("transfer %s: %w: %s", t.id, ErrAccountNotFound, e.AccountID)
		}
		if acc.Currency != t.currency {
			return nil, fmt.Errorf("transfer %s: %w: account %s is %s, transfer is %s",
				t.id, ErrCurrencyMismatch, acc.ID, acc.Currency, t.currency)
		}
		next, err := add(balances[e.AccountID], e.Amount)
		if err != nil {
			return nil, fmt.Errorf("transfer %s: account %s: %w", t.id, acc.ID, err)
		}
		if next < 0 && !acc.AllowNegative {
			return nil, fmt.Errorf("transfer %s: %w: account %s has %d, needs %d",
				t.id, ErrInsufficientFunds, acc.ID, balances[e.AccountID], -e.Amount)
		}
		postings = append(postings, Posting{Account: acc, Amount: e.Amount, NewBalance: next})
	}
	return postings, nil
}
