// Package outbox publishes events that the ledger wrote to the outbox table.
//
// Delivery is at-least-once: a message can be published again if the relay
// crashes after publishing but before recording it. Consumers must dedupe,
// for example on the message id or on the transfer id in the payload.
package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Message is one outbox row ready to publish.
type Message struct {
	ID      int64
	Topic   string
	Key     string
	Payload []byte
}

// ErrPublish reports a delivery the receiver did not confirm.
var ErrPublish = errors.New("publish failed")

// Publisher delivers a message to the outside world (a broker, a webhook,
// a log). It must return an error unless delivery is confirmed.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// relayLockID identifies the outbox relay's advisory lock. It only needs
// to differ from other advisory locks the service takes.
const relayLockID = 7_362_901_119

// Options tune a Relay. Zero values pick the defaults.
type Options struct {
	BatchSize    int           // rows claimed per round (default 100)
	PollInterval time.Duration // sleep when there is nothing to do (default 500ms)
	BaseBackoff  time.Duration // first retry delay, doubled per attempt (default 1s)
	MaxBackoff   time.Duration // cap on the retry delay (default 5m)
}

func (o Options) withDefaults() Options {
	if o.BatchSize <= 0 {
		o.BatchSize = 100
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 5 * time.Minute
	}
	return o
}

// Relay moves messages from the outbox table to a Publisher, strictly in
// id order.
//
// Each round runs in one transaction. It first takes a transaction-scoped
// advisory lock, so when several relays run (one per replica) only one of
// them publishes at a time and the others skip the round. It then reads the
// oldest unpublished rows and publishes them in order, stopping at the first
// row that fails or is still waiting for its retry time, so a message is
// never published before a lower id.
//
// What that guarantees to consumers: events touching the same account are
// published in the order its balance changed. Two transfers on a shared
// account are serialised by its row lock, and the outbox row is inserted
// after that lock is taken, so the later transfer always gets the higher
// id and only becomes visible after the earlier one committed. Unrelated
// transfers may commit in a different order than their ids, which does
// not matter to any single account.
//
// The cost of strict ordering is head-of-line blocking: a message that keeps
// failing holds back the ones behind it. attempts and last_error on the row
// show why, and are what alerting should watch.
type Relay struct {
	db     *sql.DB
	pub    Publisher
	opts   Options
	logger *slog.Logger
}

// NewRelay returns a relay over db that publishes through pub.
func NewRelay(db *sql.DB, pub Publisher, logger *slog.Logger, opts Options) *Relay {
	return &Relay{db: db, pub: pub, opts: opts.withDefaults(), logger: logger}
}

// Run publishes until ctx is cancelled. Database errors are logged and
// retried after the poll interval; they never stop the loop.
func (r *Relay) Run(ctx context.Context) error {
	for {
		n, err := r.ProcessBatch(ctx)
		if err != nil && ctx.Err() == nil {
			r.logger.ErrorContext(ctx, "outbox relay round failed", slog.Any("error", err))
		}
		if n > 0 && err == nil {
			continue // more may be waiting; go again immediately
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.opts.PollInterval):
		}
	}
}

// ProcessBatch runs one round and returns how many messages it published.
func (r *Relay) ProcessBatch(ctx context.Context) (published int, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("outbox: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var leader bool
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, relayLockID).Scan(&leader); err != nil {
		return 0, fmt.Errorf("outbox: lock: %w", err)
	}
	if !leader {
		// Another relay is publishing; committing releases nothing we hold.
		return 0, tx.Commit()
	}

	msgs, attempts, err := claim(ctx, tx, r.opts.BatchSize)
	if err != nil {
		return 0, err
	}

	for i, m := range msgs {
		if pubErr := r.pub.Publish(ctx, m); pubErr != nil {
			delay := r.backoff(attempts[i] + 1)
			r.logger.WarnContext(ctx, "outbox publish failed",
				slog.Int64("id", m.ID), slog.String("topic", m.Topic),
				slog.Int("attempt", attempts[i]+1), slog.Duration("retry_in", delay), slog.Any("error", pubErr))
			if _, err = tx.ExecContext(ctx, `
				UPDATE outbox
				SET attempts = attempts + 1,
				    last_error = $2,
				    next_attempt_at = now() + $3 * interval '1 microsecond'
				WHERE id = $1`, m.ID, truncate(pubErr.Error(), 1000), delay.Microseconds()); err != nil {
				return published, fmt.Errorf("outbox: record failure of %d: %w", m.ID, err)
			}
			break // strict order: later messages wait for this one
		}
		if _, err = tx.ExecContext(ctx,
			`UPDATE outbox SET published_at = now(), last_error = NULL WHERE id = $1`, m.ID); err != nil {
			return published, fmt.Errorf("outbox: mark %d published: %w", m.ID, err)
		}
		published++
	}

	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("outbox: commit: %w", err)
	}
	return published, nil
}

// claim returns the oldest unpublished messages that are due, in id order,
// stopping before the first one that is not due yet so nothing overtakes it.
func claim(ctx context.Context, tx *sql.Tx, limit int) ([]Message, []int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, topic, key, payload, attempts, next_attempt_at <= now() AS due
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY id
		LIMIT $1`, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("outbox: claim: %w", err)
	}
	defer rows.Close()

	var (
		msgs     []Message
		attempts []int
	)
	for rows.Next() {
		var (
			m   Message
			a   int
			due bool
		)
		if err := rows.Scan(&m.ID, &m.Topic, &m.Key, &m.Payload, &a, &due); err != nil {
			return nil, nil, fmt.Errorf("outbox: scan: %w", err)
		}
		if !due {
			break
		}
		msgs = append(msgs, m)
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("outbox: claim: %w", err)
	}
	return msgs, attempts, nil
}

// backoff returns the delay before attempt n+1: base * 2^(n-1), capped.
func (r *Relay) backoff(attempt int) time.Duration {
	d := r.opts.BaseBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= r.opts.MaxBackoff {
			return r.opts.MaxBackoff
		}
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
