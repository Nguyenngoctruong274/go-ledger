package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/lib/pq"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
)

// ErrIdempotencyConflict is returned when a transfer id that has already
// been posted is sent again with a different request (other accounts,
// amounts or currency).
var ErrIdempotencyConflict = errors.New("transfer id already used for a different request")

// errDuplicateTransfer is internal: the insert lost a race with a
// concurrent request carrying the same transfer id.
var errDuplicateTransfer = errors.New("duplicate transfer id")

// PostedEntry is one leg of a posted transfer and the balance it produced.
type PostedEntry struct {
	AccountID    ledger.AccountID
	Amount       ledger.Amount
	BalanceAfter ledger.Amount
}

// Receipt describes a posted transfer.
type Receipt struct {
	TransferID ledger.TransferID
	Currency   ledger.Currency
	Entries    []PostedEntry
	CreatedAt  time.Time
	// Replayed is true when this call did not move money because the same
	// request had already been posted; the receipt is the original one.
	Replayed bool
}

// PostTransfer atomically and idempotently applies a validated transfer.
//
// The transfer id is the idempotency key. Posting the same request twice
// returns the original receipt with Replayed set and moves money only
// once, even if the accounts have changed since or both requests arrive at
// the same moment. Reusing an id for a different request returns
// ErrIdempotencyConflict.
//
// The write itself happens in one database transaction that:
//  0. inserts the transfer row, which claims the id;
//  1. locks every involved account row with SELECT … FOR UPDATE, always in
//     ascending id order, so two transfers touching the same accounts in
//     opposite directions queue behind each other instead of deadlocking;
//  2. re-checks the transfer against the locked balances with ledger.Plan
//     (funds, currency, existence);
//  3. inserts the transfer and its entries, and moves the balances.
func (s *Store) PostTransfer(ctx context.Context, t ledger.Transfer) (Receipt, error) {
	fp := t.Fingerprint()

	// Fast path for retries: answer from what is already stored, without
	// taking any locks and without re-checking funds that may have moved.
	if r, found, err := s.replay(ctx, t.ID(), fp); err != nil || found {
		return r, err
	}

	r, err := s.post(ctx, t, fp)
	if errors.Is(err, errDuplicateTransfer) {
		// A concurrent request with the same id committed first.
		r, found, rerr := s.replay(ctx, t.ID(), fp)
		if rerr != nil {
			return Receipt{}, rerr
		}
		if !found {
			return Receipt{}, fmt.Errorf("post transfer %s: duplicate reported but transfer not found", t.ID())
		}
		return r, nil
	}
	return r, err
}

// replay loads an already-posted transfer. found is false if the id is new.
func (s *Store) replay(ctx context.Context, id ledger.TransferID, fp [32]byte) (r Receipt, found bool, err error) {
	var hash []byte
	err = s.db.QueryRowContext(ctx,
		`SELECT currency, request_hash, created_at FROM transfers WHERE id = $1`, id,
	).Scan(&r.Currency, &hash, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("replay transfer %s: %w", id, err)
	}
	if !bytes.Equal(hash, fp[:]) {
		return Receipt{}, true, fmt.Errorf("post transfer %s: %w", id, ErrIdempotencyConflict)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, amount, balance_after FROM entries WHERE transfer_id = $1 ORDER BY id`, id)
	if err != nil {
		return Receipt{}, false, fmt.Errorf("replay transfer %s: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var e PostedEntry
		if err := rows.Scan(&e.AccountID, &e.Amount, &e.BalanceAfter); err != nil {
			return Receipt{}, false, fmt.Errorf("replay transfer %s: scan: %w", id, err)
		}
		r.Entries = append(r.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return Receipt{}, false, fmt.Errorf("replay transfer %s: %w", id, err)
	}

	r.TransferID = id
	r.Replayed = true
	return r, true, nil
}

func (s *Store) post(ctx context.Context, t ledger.Transfer, fp [32]byte) (_ Receipt, err error) {
	entries := t.Entries()
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = string(e.AccountID)
	}
	sort.Strings(ids)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Receipt{}, fmt.Errorf("post transfer %s: begin: %w", t.ID(), err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Claim the transfer id first. A concurrent request with the same id
	// blocks here on the primary key until this transaction ends, then gets
	// a unique violation and replays our result. Claiming before checking
	// funds matters: otherwise the second request could see the balance the
	// first one just spent and report insufficient funds instead of the
	// original success.
	receipt := Receipt{TransferID: t.ID(), Currency: t.Currency()}
	if err = tx.QueryRowContext(ctx,
		`INSERT INTO transfers (id, currency, request_hash) VALUES ($1, $2, $3) RETURNING created_at`,
		t.ID(), t.Currency(), fp[:],
	).Scan(&receipt.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return Receipt{}, errDuplicateTransfer
		}
		return Receipt{}, fmt.Errorf("post transfer %s: insert transfer: %w", t.ID(), err)
	}

	accounts, balances, err := lockAccounts(ctx, tx, ids)
	if err != nil {
		return Receipt{}, fmt.Errorf("post transfer %s: %w", t.ID(), err)
	}

	postings, err := ledger.Plan(t, accounts, balances)
	if err != nil {
		return Receipt{}, err // already carries the transfer id
	}

	receipt.Entries = make([]PostedEntry, len(postings))
	accIDs := make([]string, len(postings))
	amounts := make([]int64, len(postings))
	after := make([]int64, len(postings))
	for i, p := range postings {
		accIDs[i] = string(p.Account.ID)
		amounts[i] = int64(p.Amount)
		after[i] = int64(p.NewBalance)
		receipt.Entries[i] = PostedEntry{AccountID: p.Account.ID, Amount: p.Amount, BalanceAfter: p.NewBalance}
	}

	// One round trip per statement regardless of how many legs the transfer
	// has. WITH ORDINALITY keeps entry ids in the order the legs were given.
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO entries (transfer_id, account_id, amount, balance_after)
		SELECT $1, e.account_id, e.amount, e.balance_after
		FROM unnest($2::text[], $3::bigint[], $4::bigint[]) WITH ORDINALITY
		     AS e(account_id, amount, balance_after, ord)
		ORDER BY e.ord`,
		t.ID(), pq.Array(accIDs), pq.Array(amounts), pq.Array(after)); err != nil {
		return Receipt{}, fmt.Errorf("post transfer %s: insert entries: %w", t.ID(), err)
	}

	if _, err = tx.ExecContext(ctx, `
		UPDATE accounts AS a
		SET balance = a.balance + d.amount
		FROM unnest($1::text[], $2::bigint[]) AS d(account_id, amount)
		WHERE a.id = d.account_id`,
		pq.Array(accIDs), pq.Array(amounts)); err != nil {
		return Receipt{}, fmt.Errorf("post transfer %s: update balances: %w", t.ID(), err)
	}

	if err = tx.Commit(); err != nil {
		return Receipt{}, fmt.Errorf("post transfer %s: commit: %w", t.ID(), err)
	}
	return receipt, nil
}

// lockAccounts loads and row-locks the given accounts. ids must be sorted:
// FOR UPDATE takes the locks in the order rows leave the ORDER BY, which
// gives every transaction the same global lock order.
func lockAccounts(ctx context.Context, tx *sql.Tx, ids []string) (map[ledger.AccountID]ledger.Account, map[ledger.AccountID]ledger.Amount, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, currency, allow_negative, balance
		FROM accounts
		WHERE id = ANY($1::text[])
		ORDER BY id
		FOR UPDATE`, pq.Array(ids))
	if err != nil {
		return nil, nil, fmt.Errorf("lock accounts: %w", err)
	}
	defer rows.Close()

	accounts := make(map[ledger.AccountID]ledger.Account, len(ids))
	balances := make(map[ledger.AccountID]ledger.Amount, len(ids))
	for rows.Next() {
		var (
			acc     ledger.Account
			balance int64
		)
		if err := rows.Scan(&acc.ID, &acc.Currency, &acc.AllowNegative, &balance); err != nil {
			return nil, nil, fmt.Errorf("lock accounts: scan: %w", err)
		}
		accounts[acc.ID] = acc
		balances[acc.ID] = ledger.Amount(balance)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("lock accounts: %w", err)
	}
	return accounts, balances, nil
}
