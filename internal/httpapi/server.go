// Package httpapi exposes the ledger over HTTP.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
	"github.com/Nguyenngoctruong274/go-ledger/internal/metrics"
)

// Ledger is what the HTTP layer needs from storage. It is defined here, by
// its consumer, so handlers can be tested with an in-memory fake and the
// storage package stays free of HTTP concerns.
type Ledger interface {
	CreateAccount(ctx context.Context, acc ledger.Account) error
	GetAccount(ctx context.Context, id ledger.AccountID) (ledger.Account, ledger.Amount, error)
	PostTransfer(ctx context.Context, t ledger.Transfer) (ledger.Receipt, error)
	Statement(ctx context.Context, id ledger.AccountID, beforeID int64, limit int) ([]ledger.StatementLine, error)
}

// Options configure optional parts of the handler. The zero value is valid.
type Options struct {
	// Metrics, when set, receives HTTP and transfer metrics and is served at
	// GET /metrics.
	Metrics *metrics.Registry
	// Ready reports whether the service can take traffic. GET /readyz
	// answers 503 while it returns an error. nil means always ready.
	Ready func(context.Context) error
}

// NewHandler builds the root HTTP handler with all routes and middleware.
func NewHandler(logger *slog.Logger, l Ledger, opts Options) http.Handler {
	reg := opts.Metrics
	if reg == nil {
		reg = metrics.NewRegistry() // instrument anyway; nothing serves it
	}
	a := &api{
		ledger: l,
		logger: logger,
		ready:  opts.Ready,
		requests: reg.NewCounterVec("http_requests_total",
			"HTTP requests by method, route pattern and status code.", "method", "route", "status"),
		latency: reg.NewHistogramVec("http_request_duration_seconds",
			"HTTP request latency by method and route pattern.", metrics.DefaultLatencyBuckets, "method", "route"),
		transfers: reg.NewCounterVec("ledger_transfers_total",
			"Transfer requests by outcome: posted, replayed, rejected, conflict, invalid, error.", "outcome"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("GET /readyz", a.handleReady)
	if opts.Metrics != nil {
		mux.Handle("GET /metrics", opts.Metrics.Handler())
	}
	mux.HandleFunc("POST /v1/accounts", a.createAccount)
	mux.HandleFunc("GET /v1/accounts/{id}", a.getAccount)
	mux.HandleFunc("GET /v1/accounts/{id}/entries", a.listEntries)
	mux.HandleFunc("POST /v1/transfers", a.postTransfer)
	return withRequestID(a.instrument(mux))
}

type api struct {
	ledger Ledger
	logger *slog.Logger
	ready  func(context.Context) error

	requests  *metrics.CounterVec
	latency   *metrics.HistogramVec
	transfers *metrics.CounterVec
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady is the readiness probe. Unlike /healthz (is the process
// alive?) it answers "should a load balancer send traffic here?", which is
// no while the database is unreachable or the server is draining.
func (a *api) handleReady(w http.ResponseWriter, r *http.Request) {
	if a.ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := a.ready(ctx); err != nil {
			a.logger.WarnContext(r.Context(), "not ready", slog.Any("error", err), slog.String("request_id", RequestID(r.Context())))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ---- request ids --------------------------------------------------------------

// RequestIDHeader carries the request id in both directions.
const RequestIDHeader = "X-Request-Id"

type requestIDKey struct{}

// RequestID returns the id of the request ctx belongs to, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// withRequestID keeps a well-formed incoming X-Request-Id (so one id can
// follow a call through several services) or generates one, stores it in
// the context and echoes it in the response.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// validRequestID accepts 1–64 characters from a conservative set, so a
// client cannot inject log lines or huge values through the header.
func validRequestID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		ok := c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}

// ---- logging and metrics ------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// instrument logs every request and records its metrics. Metrics are
// labelled with the route pattern ("/v1/accounts/{id}"), never the raw
// path, so account ids cannot explode the number of time series.
func (a *api) instrument(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		route := "unmatched"
		if _, pattern := mux.Handler(r); pattern != "" {
			if _, path, ok := strings.Cut(pattern, " "); ok {
				route = path
			} else {
				route = pattern
			}
		}
		a.requests.Inc(r.Method, route, strconv.Itoa(rec.status))
		a.latency.Observe(elapsed.Seconds(), r.Method, route)

		a.logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("request_id", RequestID(r.Context())),
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("duration", elapsed),
		)
	})
}
