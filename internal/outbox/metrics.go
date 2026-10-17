package outbox

import (
	"context"
	"database/sql"
	"math"
	"time"

	"github.com/Nguyenngoctruong274/go-ledger/internal/metrics"
)

// Metrics are the relay's operational signals. The two gauges answer the
// questions an on-call engineer asks first: is the outbox draining, and how
// long has the oldest event been waiting? A rising oldest-age with a
// non-zero failure rate is the head-of-line blocking the relay accepts in
// exchange for strict ordering.
type Metrics struct {
	published *metrics.CounterVec
	failures  *metrics.CounterVec
}

// NewMetrics registers outbox metrics on r. The pending gauges are read
// from db at scrape time, so they stay correct across relay restarts and
// replicas.
func NewMetrics(r *metrics.Registry, db *sql.DB) *Metrics {
	m := &Metrics{
		published: r.NewCounterVec("outbox_published_total", "Outbox messages delivered, by topic.", "topic"),
		failures:  r.NewCounterVec("outbox_publish_failures_total", "Failed outbox delivery attempts, by topic.", "topic"),
	}
	r.NewGaugeFunc("outbox_pending_messages", "Outbox messages not yet delivered.", func() float64 {
		return scrapeQuery(db, `SELECT count(*) FROM outbox WHERE published_at IS NULL`)
	})
	r.NewGaugeFunc("outbox_oldest_pending_age_seconds", "Age of the oldest undelivered outbox message (0 when empty).", func() float64 {
		return scrapeQuery(db, `SELECT COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)::float8 FROM outbox WHERE published_at IS NULL`)
	})
	return m
}

// scrapeQuery runs a one-number query with a short deadline. On failure it
// reports NaN, which Prometheus stores as "no sensible value" rather than a
// misleading zero.
func scrapeQuery(db *sql.DB, q string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var v float64
	if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
		return math.NaN()
	}
	return v
}

func (m *Metrics) onPublished(topic string) {
	if m != nil {
		m.published.Inc(topic)
	}
}

func (m *Metrics) onFailure(topic string) {
	if m != nil {
		m.failures.Inc(topic)
	}
}
