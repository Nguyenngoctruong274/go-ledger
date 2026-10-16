package httpapi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
)

// fakeLedger is an in-memory Ledger with the same observable rules as the
// PostgreSQL store (funds check, idempotent replay, statements), which is
// enough to test request handling and error mapping without a database.
type fakeLedger struct {
	mu        sync.Mutex
	accounts  map[ledger.AccountID]ledger.Account
	balances  map[ledger.AccountID]ledger.Amount
	transfers map[ledger.TransferID]fakeTransfer
	lines     map[ledger.AccountID][]ledger.StatementLine
	nextEntry int64
	failWith  error // when set, every call returns it
}

type fakeTransfer struct {
	fp      [32]byte
	receipt ledger.Receipt
	refusal error // set when the id's outcome was a refusal
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{
		accounts:  map[ledger.AccountID]ledger.Account{},
		balances:  map[ledger.AccountID]ledger.Amount{},
		transfers: map[ledger.TransferID]fakeTransfer{},
		lines:     map[ledger.AccountID][]ledger.StatementLine{},
	}
}

func (f *fakeLedger) CreateAccount(_ context.Context, acc ledger.Account) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	if _, ok := f.accounts[acc.ID]; ok {
		return fmt.Errorf("create account %s: %w", acc.ID, ledger.ErrAccountExists)
	}
	f.accounts[acc.ID] = acc
	return nil
}

func (f *fakeLedger) GetAccount(_ context.Context, id ledger.AccountID) (ledger.Account, ledger.Amount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return ledger.Account{}, 0, f.failWith
	}
	acc, ok := f.accounts[id]
	if !ok {
		return ledger.Account{}, 0, fmt.Errorf("get account %s: %w", id, ledger.ErrAccountNotFound)
	}
	return acc, f.balances[id], nil
}

func (f *fakeLedger) PostTransfer(_ context.Context, t ledger.Transfer) (ledger.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return ledger.Receipt{}, f.failWith
	}
	if prev, ok := f.transfers[t.ID()]; ok {
		if prev.fp != t.Fingerprint() {
			return ledger.Receipt{}, fmt.Errorf("post transfer %s: %w", t.ID(), ledger.ErrIdempotencyConflict)
		}
		if prev.refusal != nil {
			return ledger.Receipt{}, fmt.Errorf("post transfer %s: %w: %w", t.ID(), ledger.ErrReplayed, prev.refusal)
		}
		r := prev.receipt
		r.Replayed = true
		return r, nil
	}

	postings, err := ledger.Plan(t, f.accounts, f.balances)
	if err != nil {
		// Like the store, a refusal is the final outcome of the id.
		f.transfers[t.ID()] = fakeTransfer{fp: t.Fingerprint(), refusal: err}
		return ledger.Receipt{}, err
	}
	r := ledger.Receipt{TransferID: t.ID(), Currency: t.Currency(), CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	for _, p := range postings {
		f.balances[p.Account.ID] = p.NewBalance
		f.nextEntry++
		f.lines[p.Account.ID] = append(f.lines[p.Account.ID], ledger.StatementLine{
			EntryID: f.nextEntry, TransferID: t.ID(), Amount: p.Amount, BalanceAfter: p.NewBalance, CreatedAt: r.CreatedAt,
		})
		r.Entries = append(r.Entries, ledger.PostedEntry{AccountID: p.Account.ID, Amount: p.Amount, BalanceAfter: p.NewBalance})
	}
	f.transfers[t.ID()] = fakeTransfer{fp: t.Fingerprint(), receipt: r}
	return r, nil
}

func (f *fakeLedger) Statement(_ context.Context, id ledger.AccountID, beforeID int64, limit int) ([]ledger.StatementLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	if limit <= 0 {
		return nil, errors.New("bad limit")
	}
	all := f.lines[id]
	var out []ledger.StatementLine
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		if beforeID == 0 || all[i].EntryID < beforeID {
			out = append(out, all[i])
		}
	}
	return out, nil
}
