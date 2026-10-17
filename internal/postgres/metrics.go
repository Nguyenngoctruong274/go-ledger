package postgres

import (
	"database/sql"

	"github.com/Nguyenngoctruong274/go-ledger/internal/metrics"
)

// RegisterPoolMetrics exposes database/sql pool statistics. Wait count and
// wait time growing while in-use sits at the pool cap mean requests are
// queueing for connections: raise the cap or find the slow queries.
func RegisterPoolMetrics(r *metrics.Registry, db *sql.DB) {
	r.NewGaugeFunc("db_pool_max_open_connections", "Configured connection cap.", func() float64 {
		return float64(db.Stats().MaxOpenConnections)
	})
	r.NewGaugeFunc("db_pool_open_connections", "Open connections, in use or idle.", func() float64 {
		return float64(db.Stats().OpenConnections)
	})
	r.NewGaugeFunc("db_pool_in_use_connections", "Connections currently in use.", func() float64 {
		return float64(db.Stats().InUse)
	})
	r.NewCounterFunc("db_pool_wait_total", "Times a caller waited for a free connection.", func() float64 {
		return float64(db.Stats().WaitCount)
	})
	r.NewCounterFunc("db_pool_wait_seconds_total", "Total time spent waiting for a free connection.", func() float64 {
		return db.Stats().WaitDuration.Seconds()
	})
}
