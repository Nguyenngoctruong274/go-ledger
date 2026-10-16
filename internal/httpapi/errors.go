package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
)

// apiError is the JSON error body: {"error": {"code": "...", "message": "..."}}.
// code is stable and meant for programs; message is for humans.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorBody struct {
	Error apiError `json:"error"`
}

// errorMapping ties a domain error to an HTTP status and a stable code.
// The first match wins, so more specific errors come first.
var errorMapping = []struct {
	err    error
	status int
	code   string
}{
	{ledger.ErrInsufficientFunds, http.StatusPaymentRequired, "insufficient_funds"},
	{ledger.ErrAccountNotFound, http.StatusNotFound, "account_not_found"},
	{ledger.ErrAccountExists, http.StatusConflict, "account_exists"},
	{ledger.ErrIdempotencyConflict, http.StatusUnprocessableEntity, "idempotency_conflict"},
	{ledger.ErrCurrencyMismatch, http.StatusUnprocessableEntity, "currency_mismatch"},
	{ledger.ErrInvalidCurrency, http.StatusBadRequest, "invalid_currency"},
	{ledger.ErrUnbalanced, http.StatusBadRequest, "unbalanced_transfer"},
	{ledger.ErrTooFewEntries, http.StatusBadRequest, "too_few_entries"},
	{ledger.ErrZeroAmount, http.StatusBadRequest, "zero_amount"},
	{ledger.ErrNonPositiveAmount, http.StatusBadRequest, "non_positive_amount"},
	{ledger.ErrDuplicateAccount, http.StatusBadRequest, "duplicate_account"},
	{ledger.ErrAmountOverflow, http.StatusBadRequest, "amount_overflow"},
	{ledger.ErrMissingID, http.StatusBadRequest, "missing_id"},
	{errInvalidRequest, http.StatusBadRequest, "invalid_request"},
}

// errInvalidRequest marks malformed input detected by the HTTP layer.
var errInvalidRequest = errors.New("invalid request")

// statusFor returns the HTTP status and code for err, or 500 if err is not
// a known domain error.
func statusFor(err error) (int, string) {
	for _, m := range errorMapping {
		if errors.Is(err, m.err) {
			return m.status, m.code
		}
	}
	return http.StatusInternalServerError, "internal"
}

// writeError renders err. Known errors expose their message, which only
// contains caller-supplied identifiers; unknown errors are logged and
// replaced by a generic message so internals never leak to clients.
func (a *api) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := statusFor(err)
	if errors.Is(err, ledger.ErrReplayed) {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	msg := err.Error()
	if status == http.StatusInternalServerError {
		a.logger.ErrorContext(r.Context(), "request failed",
			slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("error", err))
		msg = "internal error"
	}
	writeJSON(w, status, errorBody{Error: apiError{Code: code, Message: msg}})
}
