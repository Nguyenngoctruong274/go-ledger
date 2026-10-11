// Package ledger holds the double-entry bookkeeping rules of go-ledger.
//
// Money is always an integer number of minor units (cents, đồng, …) so that
// arithmetic is exact. Every Transfer is a set of entries whose amounts sum
// to zero; that invariant is enforced when a Transfer is constructed, so any
// Transfer value that exists is balanced.
package ledger

import (
	"errors"
	"fmt"
	"math"
)

// Amount is a signed quantity of money in minor units.
// Negative amounts debit an account (money leaves it); positive amounts
// credit an account (money arrives).
type Amount int64

// Currency is an ISO 4217 alphabetic code such as "VND" or "USD".
type Currency string

// ErrInvalidCurrency reports a currency code that is not three upper-case letters.
var ErrInvalidCurrency = errors.New("invalid currency")

// ErrAmountOverflow reports arithmetic that would not fit in an Amount.
var ErrAmountOverflow = errors.New("amount overflow")

// ParseCurrency validates and returns a currency code.
func ParseCurrency(code string) (Currency, error) {
	if len(code) != 3 {
		return "", fmt.Errorf("%w: %q must have 3 letters", ErrInvalidCurrency, code)
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return "", fmt.Errorf("%w: %q must be upper-case ASCII letters", ErrInvalidCurrency, code)
		}
	}
	return Currency(code), nil
}

// add returns a+b, or ErrAmountOverflow if the result does not fit.
func add(a, b Amount) (Amount, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, ErrAmountOverflow
	}
	return a + b, nil
}
