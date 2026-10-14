package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
)

const (
	maxBodyBytes     = 1 << 20 // 1 MiB is far more than any valid request
	maxIDLength      = 128
	defaultPageLimit = 50
	maxPageLimit     = 500
)

// decodeJSON reads exactly one JSON object into dst, rejecting unknown
// fields, trailing data and oversized bodies.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: body: %w", errInvalidRequest, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: body must contain a single JSON object", errInvalidRequest)
	}
	return nil
}

func checkID(field, v string) error {
	if v == "" {
		return fmt.Errorf("%w: %s is required", errInvalidRequest, field)
	}
	if len(v) > maxIDLength {
		return fmt.Errorf("%w: %s is longer than %d bytes", errInvalidRequest, field, maxIDLength)
	}
	return nil
}

// ---- accounts ---------------------------------------------------------------

type createAccountRequest struct {
	ID            string `json:"id"`
	Currency      string `json:"currency"`
	AllowNegative bool   `json:"allow_negative"`
}

type accountResponse struct {
	ID            string `json:"id"`
	Currency      string `json:"currency"`
	AllowNegative bool   `json:"allow_negative"`
	Balance       int64  `json:"balance"`
}

func toAccountResponse(acc ledger.Account, balance ledger.Amount) accountResponse {
	return accountResponse{
		ID:            string(acc.ID),
		Currency:      string(acc.Currency),
		AllowNegative: acc.AllowNegative,
		Balance:       int64(balance),
	}
}

func (a *api) createAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := checkID("id", req.ID); err != nil {
		a.writeError(w, r, err)
		return
	}
	cur, err := ledger.ParseCurrency(req.Currency)
	if err != nil {
		a.writeError(w, r, err)
		return
	}

	acc := ledger.Account{ID: ledger.AccountID(req.ID), Currency: cur, AllowNegative: req.AllowNegative}
	if err := a.ledger.CreateAccount(r.Context(), acc); err != nil {
		a.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/accounts/"+req.ID)
	writeJSON(w, http.StatusCreated, toAccountResponse(acc, 0))
}

func (a *api) getAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := checkID("account id", id); err != nil {
		a.writeError(w, r, err)
		return
	}
	acc, balance, err := a.ledger.GetAccount(r.Context(), ledger.AccountID(id))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAccountResponse(acc, balance))
}

type entryResponse struct {
	EntryID      int64     `json:"entry_id"`
	TransferID   string    `json:"transfer_id"`
	Amount       int64     `json:"amount"`
	BalanceAfter int64     `json:"balance_after"`
	CreatedAt    time.Time `json:"created_at"`
}

type entriesResponse struct {
	Entries []entryResponse `json:"entries"`
	// NextBefore is the cursor for the next (older) page, or null at the end.
	NextBefore *int64 `json:"next_before"`
}

func (a *api) listEntries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := checkID("account id", id); err != nil {
		a.writeError(w, r, err)
		return
	}
	limit, err := intParam(r, "limit", defaultPageLimit, 1, maxPageLimit)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	before, err := intParam(r, "before", 0, 1, 1<<62)
	if err != nil {
		a.writeError(w, r, err)
		return
	}

	// Distinguish "no entries yet" from "no such account".
	if _, _, err := a.ledger.GetAccount(r.Context(), ledger.AccountID(id)); err != nil {
		a.writeError(w, r, err)
		return
	}
	lines, err := a.ledger.Statement(r.Context(), ledger.AccountID(id), int64(before), limit)
	if err != nil {
		a.writeError(w, r, err)
		return
	}

	resp := entriesResponse{Entries: make([]entryResponse, len(lines))}
	for i, l := range lines {
		resp.Entries[i] = entryResponse{
			EntryID: l.EntryID, TransferID: string(l.TransferID), Amount: int64(l.Amount),
			BalanceAfter: int64(l.BalanceAfter), CreatedAt: l.CreatedAt.UTC(),
		}
	}
	if len(lines) == limit {
		next := lines[len(lines)-1].EntryID
		resp.NextBefore = &next
	}
	writeJSON(w, http.StatusOK, resp)
}

// intParam reads an optional integer query parameter within [lo, hi].
func intParam(r *http.Request, name string, def, lo, hi int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("%w: %s must be an integer between %d and %d", errInvalidRequest, name, lo, hi)
	}
	return v, nil
}

// ---- transfers --------------------------------------------------------------

// IdempotencyKeyHeader carries the client-chosen transfer id. It is
// required: a transfer without one cannot be retried safely.
const IdempotencyKeyHeader = "Idempotency-Key"

type entryRequest struct {
	AccountID string `json:"account_id"`
	Amount    int64  `json:"amount"`
}

// postTransferRequest accepts either a list of entries (any number of legs)
// or the from/to/amount shorthand for a simple payment.
type postTransferRequest struct {
	Currency string         `json:"currency"`
	Entries  []entryRequest `json:"entries,omitempty"`
	From     string         `json:"from,omitempty"`
	To       string         `json:"to,omitempty"`
	Amount   int64          `json:"amount,omitempty"`
}

type postedEntryResponse struct {
	AccountID    string `json:"account_id"`
	Amount       int64  `json:"amount"`
	BalanceAfter int64  `json:"balance_after"`
}

type transferResponse struct {
	ID        string                `json:"id"`
	Currency  string                `json:"currency"`
	Entries   []postedEntryResponse `json:"entries"`
	CreatedAt time.Time             `json:"created_at"`
}

func (req postTransferRequest) toTransfer(id ledger.TransferID) (ledger.Transfer, error) {
	cur, err := ledger.ParseCurrency(req.Currency)
	if err != nil {
		return ledger.Transfer{}, err
	}
	shorthand := req.From != "" || req.To != "" || req.Amount != 0
	switch {
	case shorthand && len(req.Entries) > 0:
		return ledger.Transfer{}, fmt.Errorf("%w: use either entries or from/to/amount, not both", errInvalidRequest)
	case shorthand:
		if err := checkID("from", req.From); err != nil {
			return ledger.Transfer{}, err
		}
		if err := checkID("to", req.To); err != nil {
			return ledger.Transfer{}, err
		}
		return ledger.NewPayment(id, cur, ledger.AccountID(req.From), ledger.AccountID(req.To), ledger.Amount(req.Amount))
	default:
		entries := make([]ledger.Entry, len(req.Entries))
		for i, e := range req.Entries {
			if err := checkID(fmt.Sprintf("entries[%d].account_id", i), e.AccountID); err != nil {
				return ledger.Transfer{}, err
			}
			entries[i] = ledger.Entry{AccountID: ledger.AccountID(e.AccountID), Amount: ledger.Amount(e.Amount)}
		}
		return ledger.NewTransfer(id, cur, entries...)
	}
}

func (a *api) postTransfer(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get(IdempotencyKeyHeader)
	if err := checkID(IdempotencyKeyHeader+" header", key); err != nil {
		a.writeError(w, r, err)
		return
	}
	var req postTransferRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeError(w, r, err)
		return
	}
	tr, err := req.toTransfer(ledger.TransferID(key))
	if err != nil {
		a.writeError(w, r, err)
		return
	}

	receipt, err := a.ledger.PostTransfer(r.Context(), tr)
	if err != nil {
		a.writeError(w, r, err)
		return
	}

	resp := transferResponse{
		ID: string(receipt.TransferID), Currency: string(receipt.Currency),
		Entries: make([]postedEntryResponse, len(receipt.Entries)), CreatedAt: receipt.CreatedAt.UTC(),
	}
	for i, e := range receipt.Entries {
		resp.Entries[i] = postedEntryResponse{
			AccountID: string(e.AccountID), Amount: int64(e.Amount), BalanceAfter: int64(e.BalanceAfter),
		}
	}

	status := http.StatusCreated
	if receipt.Replayed {
		// Same body as the first response, flagged so clients can tell.
		w.Header().Set("Idempotent-Replayed", "true")
		status = http.StatusOK
	}
	writeJSON(w, status, resp)
}
