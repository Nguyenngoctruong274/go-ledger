// Package httpapi exposes the ledger over HTTP.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
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

// NewHandler builds the root HTTP handler with all routes and middleware.
func NewHandler(logger *slog.Logger, l Ledger) http.Handler {
	a := &api{ledger: l, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("POST /v1/accounts", a.createAccount)
	mux.HandleFunc("GET /v1/accounts/{id}", a.getAccount)
	mux.HandleFunc("GET /v1/accounts/{id}/entries", a.listEntries)
	mux.HandleFunc("POST /v1/transfers", a.postTransfer)
	return logRequests(logger, mux)
}

type api struct {
	ledger Ledger
	logger *slog.Logger
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}
