package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/lib/pq"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
)

// ErrTransferExists is returned when a transfer id has already been posted.
var ErrTransferExists = errors.New("transfer already exists")

// PostTransfer atomically applies a validated transfer.
//
// Inside one database transaction it:
//  1. locks every involved account row with SELECT … FOR UPDATE, always in
//     ascending id order, so two transfers touching the same accounts in
//     opposite directions queue behind each other instead of deadlocking;
//  2. re-checks the transfer against the locked balances with ledger.Plan
//     (funds, currency, existence);
//  3. inserts the transfer and its entries, and moves the balances.
//
// Either everything is written or nothing is. The schema's deferred
// zero-sum trigger and overdraft CHECK act as a second line of defence.
func (s *Store) PostTransfer(ctx context.Context, t ledger.Transfer) (_ []ledger.Posting, err error) {
	entries := t.Entries()
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = string(e.AccountID)
	}
	sort.Strings(ids)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("post transfer %s: begin: %w", t.ID(), err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	accounts, balances, err := lockAccounts(ctx, tx, ids)
	if err != nil {
		return nil, fmt.Errorf("post transfer %s: %w", t.ID(), err)
	}

	postings, err := ledger.Plan(t, accounts, balances)
	if err != nil {
		return nil, err // already carries the transfer id
	}

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO transfers (id, currency) VALUES ($1, $2)`, t.ID(), t.Currency()); err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("post transfer %s: %w", t.ID(), ErrTransferExists)
		}
		return nil, fmt.Errorf("post transfer %s: insert transfer: %w", t.ID(), err)
	}

	accIDs := make([]string, len(entries))
	amounts := make([]int64, len(entries))
	for i, e := range entries {
		accIDs[i] = string(e.AccountID)
		amounts[i] = int64(e.Amount)
	}

	// One round trip per statement regardless of how many legs the transfer has.
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO entries (transfer_id, account_id, amount)
		SELECT $1, e.account_id, e.amount
		FROM unnest($2::text[], $3::bigint[]) AS e(account_id, amount)`,
		t.ID(), pq.Array(accIDs), pq.Array(amounts)); err != nil {
		return nil, fmt.Errorf("post transfer %s: insert entries: %w", t.ID(), err)
	}

	if _, err = tx.ExecContext(ctx, `
		UPDATE accounts AS a
		SET balance = a.balance + d.amount
		FROM unnest($1::text[], $2::bigint[]) AS d(account_id, amount)
		WHERE a.id = d.account_id`,
		pq.Array(accIDs), pq.Array(amounts)); err != nil {
		return nil, fmt.Errorf("post transfer %s: update balances: %w", t.ID(), err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("post transfer %s: commit: %w", t.ID(), err)
	}
	return postings, nil
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
