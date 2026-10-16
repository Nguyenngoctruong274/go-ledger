package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/lib/pq"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
)

// errDuplicateTransfer is internal: the insert lost a race with a
// concurrent request carrying the same transfer id.
var errDuplicateTransfer = errors.New("duplicate transfer id")

// PostTransfer atomically and idempotently applies a validated transfer.
//
// The transfer id is the idempotency key and gets exactly one outcome.
// Posting the same request twice returns the original receipt with
// Replayed set and moves money only once, even if the accounts have changed
// since or both requests arrive at the same moment. If the first attempt was
// refused (insufficient funds, unknown account, currency mismatch), every
// later attempt with that id gets the same refusal wrapped with
// ledger.ErrReplayed. Reusing an id for a different request returns
// ledger.ErrIdempotencyConflict.
//
// The write itself happens in one database transaction that:
//  0. inserts the transfer row, which claims the id;
//  1. locks every involved account row with SELECT … FOR UPDATE, always in
//     ascending id order, so two transfers touching the same accounts in
//     opposite directions queue behind each other instead of deadlocking;
//  2. re-checks the transfer against the locked balances with ledger.Plan
//     (funds, currency, existence);
//  3. inserts the entries, moves the balances and writes a
//     transfer.posted event to the outbox.
func (s *Store) PostTransfer(ctx context.Context, t ledger.Transfer) (ledger.Receipt, error) {
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
			return ledger.Receipt{}, rerr
		}
		if !found {
			return ledger.Receipt{}, fmt.Errorf("post transfer %s: duplicate reported but transfer not found", t.ID())
		}
		return r, nil
	}
	return r, err
}

// replay loads an already-posted transfer. found is false if the id is new.
func (s *Store) replay(ctx context.Context, id ledger.TransferID, fp [32]byte) (r ledger.Receipt, found bool, err error) {
	var (
		hash       []byte
		status     string
		rejectCode sql.NullString
	)
	err = s.db.QueryRowContext(ctx,
		`SELECT currency, request_hash, created_at, status, reject_code FROM transfers WHERE id = $1`, id,
	).Scan(&r.Currency, &hash, &r.CreatedAt, &status, &rejectCode)
	if errors.Is(err, sql.ErrNoRows) {
		return ledger.Receipt{}, false, nil
	}
	if err != nil {
		return ledger.Receipt{}, false, fmt.Errorf("replay transfer %s: %w", id, err)
	}
	if !bytes.Equal(hash, fp[:]) {
		return ledger.Receipt{}, true, fmt.Errorf("post transfer %s: %w", id, ledger.ErrIdempotencyConflict)
	}
	if status == "rejected" {
		return ledger.Receipt{}, true, fmt.Errorf("post transfer %s: %w: %w", id, ledger.ErrReplayed, errorForRejectCode(rejectCode.String))
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, amount, balance_after FROM entries WHERE transfer_id = $1 ORDER BY id`, id)
	if err != nil {
		return ledger.Receipt{}, false, fmt.Errorf("replay transfer %s: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var e ledger.PostedEntry
		if err := rows.Scan(&e.AccountID, &e.Amount, &e.BalanceAfter); err != nil {
			return ledger.Receipt{}, false, fmt.Errorf("replay transfer %s: scan: %w", id, err)
		}
		r.Entries = append(r.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return ledger.Receipt{}, false, fmt.Errorf("replay transfer %s: %w", id, err)
	}

	r.TransferID = id
	r.Replayed = true
	return r, true, nil
}

func (s *Store) post(ctx context.Context, t ledger.Transfer, fp [32]byte) (_ ledger.Receipt, err error) {
	entries := t.Entries()
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = string(e.AccountID)
	}
	sort.Strings(ids)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ledger.Receipt{}, fmt.Errorf("post transfer %s: begin: %w", t.ID(), err)
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
	receipt := ledger.Receipt{TransferID: t.ID(), Currency: t.Currency()}
	if err = tx.QueryRowContext(ctx,
		`INSERT INTO transfers (id, currency, request_hash) VALUES ($1, $2, $3) RETURNING created_at`,
		t.ID(), t.Currency(), fp[:],
	).Scan(&receipt.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return ledger.Receipt{}, errDuplicateTransfer
		}
		return ledger.Receipt{}, fmt.Errorf("post transfer %s: insert transfer: %w", t.ID(), err)
	}

	accounts, balances, err := lockAccounts(ctx, tx, ids)
	if err != nil {
		return ledger.Receipt{}, fmt.Errorf("post transfer %s: %w", t.ID(), err)
	}

	postings, planErr := ledger.Plan(t, accounts, balances)
	if planErr != nil {
		code, final := rejectCodeFor(planErr)
		if !final {
			return ledger.Receipt{}, planErr // already carries the transfer id
		}
		// Record the refusal under this id and commit it: the outcome of a
		// transfer id is decided once. A duplicate that was waiting on our
		// primary key will now replay this refusal instead of re-checking
		// funds that may have arrived since.
		if _, err = tx.ExecContext(ctx,
			`UPDATE transfers SET status = 'rejected', reject_code = $2 WHERE id = $1`, t.ID(), code); err != nil {
			return ledger.Receipt{}, fmt.Errorf("post transfer %s: record rejection: %w", t.ID(), err)
		}
		if err = tx.Commit(); err != nil {
			return ledger.Receipt{}, fmt.Errorf("post transfer %s: commit rejection: %w", t.ID(), err)
		}
		return ledger.Receipt{}, planErr
	}

	receipt.Entries = make([]ledger.PostedEntry, len(postings))
	accIDs := make([]string, len(postings))
	amounts := make([]int64, len(postings))
	after := make([]int64, len(postings))
	for i, p := range postings {
		accIDs[i] = string(p.Account.ID)
		amounts[i] = int64(p.Amount)
		after[i] = int64(p.NewBalance)
		receipt.Entries[i] = ledger.PostedEntry{AccountID: p.Account.ID, Amount: p.Amount, BalanceAfter: p.NewBalance}
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
		return ledger.Receipt{}, fmt.Errorf("post transfer %s: insert entries: %w", t.ID(), err)
	}

	if _, err = tx.ExecContext(ctx, `
		UPDATE accounts AS a
		SET balance = a.balance + d.amount
		FROM unnest($1::text[], $2::bigint[]) AS d(account_id, amount)
		WHERE a.id = d.account_id`,
		pq.Array(accIDs), pq.Array(amounts)); err != nil {
		return ledger.Receipt{}, fmt.Errorf("post transfer %s: update balances: %w", t.ID(), err)
	}

	// The event is part of the same transaction: it exists if and only if
	// the money moved. The outbox relay publishes it afterwards.
	payload, err := json.Marshal(newTransferPostedEvent(receipt))
	if err != nil {
		return ledger.Receipt{}, fmt.Errorf("post transfer %s: encode event: %w", t.ID(), err)
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO outbox (topic, key, payload) VALUES ($1, $2, $3)`,
		TopicTransferPosted, t.ID(), payload); err != nil {
		return ledger.Receipt{}, fmt.Errorf("post transfer %s: write outbox: %w", t.ID(), err)
	}

	if err = tx.Commit(); err != nil {
		return ledger.Receipt{}, fmt.Errorf("post transfer %s: commit: %w", t.ID(), err)
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

// rejectCodes lists the refusals that are recorded as the final outcome of
// a transfer id. Anything else (a database error, a cancelled context) is
// not an answer about the transfer and is rolled back so it can be retried.
var rejectCodes = []struct {
	code string
	err  error
}{
	{"insufficient_funds", ledger.ErrInsufficientFunds},
	{"account_not_found", ledger.ErrAccountNotFound},
	{"currency_mismatch", ledger.ErrCurrencyMismatch},
	{"amount_overflow", ledger.ErrAmountOverflow},
}

func rejectCodeFor(err error) (string, bool) {
	for _, rc := range rejectCodes {
		if errors.Is(err, rc.err) {
			return rc.code, true
		}
	}
	return "", false
}

func errorForRejectCode(code string) error {
	for _, rc := range rejectCodes {
		if rc.code == code {
			return rc.err
		}
	}
	return fmt.Errorf("rejected (%s)", code)
}
