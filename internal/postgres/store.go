// Package postgres stores the ledger in PostgreSQL.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
)

// Open connects to PostgreSQL and verifies the connection.
func Open(ctx context.Context, url string) (*sql.DB, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return db, nil
}

// Store implements ledger persistence on PostgreSQL.
type Store struct {
	db *sql.DB
}

// NewStore returns a Store using db. The caller owns db and closes it.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// CreateAccount inserts a new account with a zero balance.
func (s *Store) CreateAccount(ctx context.Context, acc ledger.Account) error {
	if acc.ID == "" {
		return fmt.Errorf("create account: %w", ledger.ErrMissingID)
	}
	if _, err := ledger.ParseCurrency(string(acc.Currency)); err != nil {
		return fmt.Errorf("create account %s: %w", acc.ID, err)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts (id, currency, allow_negative) VALUES ($1, $2, $3)`,
		acc.ID, acc.Currency, acc.AllowNegative)
	if isUniqueViolation(err) {
		return fmt.Errorf("create account %s: %w", acc.ID, ledger.ErrAccountExists)
	}
	if err != nil {
		return fmt.Errorf("create account %s: %w", acc.ID, err)
	}
	return nil
}

// GetAccount returns an account and its current balance.
func (s *Store) GetAccount(ctx context.Context, id ledger.AccountID) (ledger.Account, ledger.Amount, error) {
	var (
		acc     ledger.Account
		balance int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, currency, allow_negative, balance FROM accounts WHERE id = $1`, id,
	).Scan(&acc.ID, &acc.Currency, &acc.AllowNegative, &balance)
	if errors.Is(err, sql.ErrNoRows) {
		return ledger.Account{}, 0, fmt.Errorf("get account %s: %w", id, ledger.ErrAccountNotFound)
	}
	if err != nil {
		return ledger.Account{}, 0, fmt.Errorf("get account %s: %w", id, err)
	}
	return acc, ledger.Amount(balance), nil
}

// Statement returns up to limit entries for an account, newest first,
// starting strictly before the entry id beforeID (0 means from the newest).
// Keyset pagination keeps every page an index range scan.
func (s *Store) Statement(ctx context.Context, id ledger.AccountID, beforeID int64, limit int) ([]ledger.StatementLine, error) {
	if limit <= 0 || limit > 500 {
		return nil, fmt.Errorf("statement %s: limit %d out of range 1..500", id, limit)
	}
	if beforeID <= 0 {
		beforeID = 1<<63 - 1
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, transfer_id, amount, balance_after, created_at
		FROM entries
		WHERE account_id = $1 AND id < $2
		ORDER BY id DESC
		LIMIT $3`, id, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("statement %s: %w", id, err)
	}
	defer rows.Close()

	lines := make([]ledger.StatementLine, 0, limit)
	for rows.Next() {
		var l ledger.StatementLine
		if err := rows.Scan(&l.EntryID, &l.TransferID, &l.Amount, &l.BalanceAfter, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("statement %s: scan: %w", id, err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("statement %s: %w", id, err)
	}
	return lines, nil
}

func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}
