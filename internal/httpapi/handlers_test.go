package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Nguyenngoctruong274/go-ledger/internal/ledger"
)

type client struct {
	t *testing.T
	h http.Handler
}

func newClient(t *testing.T, l Ledger) *client {
	return &client{t: t, h: NewHandler(discardLogger(), l)}
}

func (c *client) do(method, path, body string, headers ...string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q is not JSON: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

// seeded returns a client over a fake ledger with a funding account and two
// wallets; alice holds 1,000 VND.
func seeded(t *testing.T) (*client, *fakeLedger) {
	t.Helper()
	f := newFakeLedger()
	c := newClient(t, f)
	for _, body := range []string{
		`{"id":"funding","currency":"VND","allow_negative":true}`,
		`{"id":"alice","currency":"VND"}`,
		`{"id":"bob","currency":"VND"}`,
		`{"id":"usd","currency":"USD"}`,
	} {
		if rec := c.do("POST", "/v1/accounts", body); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	rec := c.do("POST", "/v1/transfers", `{"currency":"VND","from":"funding","to":"alice","amount":1000}`,
		IdempotencyKeyHeader, "seed")
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed transfer: %d %s", rec.Code, rec.Body)
	}
	return c, f
}

func TestCreateAndGetAccount(t *testing.T) {
	c := newClient(t, newFakeLedger())

	rec := c.do("POST", "/v1/accounts", `{"id":"wallet-1","currency":"VND"}`)
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") != "/v1/accounts/wallet-1" {
		t.Fatalf("create: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}

	rec = c.do("GET", "/v1/accounts/wallet-1", "")
	var got accountResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	if got != (accountResponse{ID: "wallet-1", Currency: "VND", Balance: 0}) {
		t.Fatalf("got %+v", got)
	}
}

func TestPostTransferAndReplay(t *testing.T) {
	c, _ := seeded(t)
	body := `{"currency":"VND","entries":[{"account_id":"alice","amount":-300},{"account_id":"bob","amount":300}]}`

	first := c.do("POST", "/v1/transfers", body, IdempotencyKeyHeader, "pay-1")
	if first.Code != http.StatusCreated || first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("first: %d %v %s", first.Code, first.Header(), first.Body)
	}
	var tr transferResponse
	if err := json.Unmarshal(first.Body.Bytes(), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.ID != "pay-1" || len(tr.Entries) != 2 || tr.Entries[0].BalanceAfter != 700 || tr.Entries[1].BalanceAfter != 300 {
		t.Fatalf("receipt = %+v", tr)
	}

	again := c.do("POST", "/v1/transfers", body, IdempotencyKeyHeader, "pay-1")
	if again.Code != http.StatusOK || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v", again.Code, again.Header())
	}
	if again.Body.String() != first.Body.String() {
		t.Fatalf("replay body differs:\n%s\n%s", again.Body, first.Body)
	}

	rec := c.do("GET", "/v1/accounts/alice", "")
	if !strings.Contains(rec.Body.String(), `"balance":700`) {
		t.Fatalf("alice after replay: %s (charged twice?)", rec.Body)
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		key        string
		wantStatus int
		wantCode   string
	}{
		{"insufficient funds", "POST", "/v1/transfers", `{"currency":"VND","from":"alice","to":"bob","amount":1001}`, "k1", 402, "insufficient_funds"},
		{"unknown account", "POST", "/v1/transfers", `{"currency":"VND","from":"alice","to":"carol","amount":1}`, "k2", 404, "account_not_found"},
		{"currency mismatch", "POST", "/v1/transfers", `{"currency":"VND","from":"alice","to":"usd","amount":1}`, "k3", 422, "currency_mismatch"},
		{"idempotency conflict", "POST", "/v1/transfers", `{"currency":"VND","from":"funding","to":"bob","amount":5}`, "seed", 422, "idempotency_conflict"},
		{"unbalanced", "POST", "/v1/transfers", `{"currency":"VND","entries":[{"account_id":"alice","amount":-5},{"account_id":"bob","amount":4}]}`, "k4", 400, "unbalanced_transfer"},
		{"one entry", "POST", "/v1/transfers", `{"currency":"VND","entries":[{"account_id":"alice","amount":-5}]}`, "k5", 400, "too_few_entries"},
		{"zero amount", "POST", "/v1/transfers", `{"currency":"VND","from":"alice","to":"bob","amount":0}`, "k6", 400, "non_positive_amount"},
		{"negative amount", "POST", "/v1/transfers", `{"currency":"VND","from":"alice","to":"bob","amount":-5}`, "k7", 400, "non_positive_amount"},
		{"self payment", "POST", "/v1/transfers", `{"currency":"VND","from":"alice","to":"alice","amount":5}`, "k8", 400, "duplicate_account"},
		{"bad currency", "POST", "/v1/transfers", `{"currency":"vnd","from":"alice","to":"bob","amount":5}`, "k9", 400, "invalid_currency"},
		{"missing key", "POST", "/v1/transfers", `{"currency":"VND","from":"alice","to":"bob","amount":5}`, "", 400, "invalid_request"},
		{"both forms", "POST", "/v1/transfers", `{"currency":"VND","from":"alice","to":"bob","amount":5,"entries":[{"account_id":"a","amount":1}]}`, "k10", 400, "invalid_request"},
		{"unknown field", "POST", "/v1/transfers", `{"currency":"VND","from":"alice","to":"bob","amount":5,"memo":"x"}`, "k11", 400, "invalid_request"},
		{"trailing data", "POST", "/v1/transfers", `{"currency":"VND","from":"alice","to":"bob","amount":5}{}`, "k12", 400, "invalid_request"},
		{"not json", "POST", "/v1/transfers", `amount=5`, "k13", 400, "invalid_request"},
		{"amount is a string", "POST", "/v1/transfers", `{"currency":"VND","from":"alice","to":"bob","amount":"5"}`, "k14", 400, "invalid_request"},
		{"account exists", "POST", "/v1/accounts", `{"id":"alice","currency":"VND"}`, "", 409, "account_exists"},
		{"account without id", "POST", "/v1/accounts", `{"currency":"VND"}`, "", 400, "invalid_request"},
		{"account bad currency", "POST", "/v1/accounts", `{"id":"x","currency":"dong"}`, "", 400, "invalid_currency"},
		{"get missing account", "GET", "/v1/accounts/nobody", "", "", 404, "account_not_found"},
		{"entries of missing account", "GET", "/v1/accounts/nobody/entries", "", "", 404, "account_not_found"},
		{"bad limit", "GET", "/v1/accounts/alice/entries?limit=0", "", "", 400, "invalid_request"},
		{"limit too big", "GET", "/v1/accounts/alice/entries?limit=501", "", "", 400, "invalid_request"},
		{"bad cursor", "GET", "/v1/accounts/alice/entries?before=abc", "", "", 400, "invalid_request"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := seeded(t)
			var headers []string
			if tt.key != "" {
				headers = []string{IdempotencyKeyHeader, tt.key}
			}
			rec := c.do(tt.method, tt.path, tt.body, headers...)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := errorCode(t, rec); got != tt.wantCode {
				t.Fatalf("code = %q, want %q", got, tt.wantCode)
			}
		})
	}
}

func TestUnknownErrorsDoNotLeak(t *testing.T) {
	f := newFakeLedger()
	f.failWith = errors.New("pq: password authentication failed for user ledger at 10.0.0.5")
	c := newClient(t, f)

	rec := c.do("GET", "/v1/accounts/alice", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "pq:") || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("internal error leaked to client: %s", rec.Body)
	}
	if errorCode(t, rec) != "internal" {
		t.Fatalf("code = %q", errorCode(t, rec))
	}
}

func TestRequestBodyIsLimited(t *testing.T) {
	c, _ := seeded(t)
	huge := `{"currency":"VND","from":"alice","to":"bob","amount":5,"x":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	rec := c.do("POST", "/v1/transfers", huge, IdempotencyKeyHeader, "big")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestListEntriesPagination(t *testing.T) {
	c, _ := seeded(t)
	for i := 1; i <= 3; i++ {
		rec := c.do("POST", "/v1/transfers", fmt.Sprintf(`{"currency":"VND","from":"alice","to":"bob","amount":%d}`, i*10),
			IdempotencyKeyHeader, fmt.Sprintf("p%d", i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("transfer %d: %d %s", i, rec.Code, rec.Body)
		}
	}

	var page entriesResponse
	rec := c.do("GET", "/v1/accounts/alice/entries?limit=2", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 || page.Entries[0].TransferID != "p3" || page.NextBefore == nil {
		t.Fatalf("page 1 = %+v", page)
	}

	rec = c.do("GET", fmt.Sprintf("/v1/accounts/alice/entries?limit=2&before=%d", *page.NextBefore), "")
	page = entriesResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	// p1 and the seed top-up remain; a full page still offers a cursor.
	if len(page.Entries) != 2 || page.Entries[0].TransferID != "p1" || page.Entries[1].TransferID != "seed" {
		t.Fatalf("page 2 = %+v", page)
	}
	if page.Entries[1].BalanceAfter != 1000 || page.Entries[0].BalanceAfter != 990 {
		t.Fatalf("running balances = %+v", page.Entries)
	}

	rec = c.do("GET", "/v1/accounts/bob/entries", "")
	if !strings.Contains(rec.Body.String(), `"next_before":null`) {
		t.Fatalf("short page should end the cursor: %s", rec.Body)
	}
}

func TestStatusForUnknownErrorIs500(t *testing.T) {
	if status, code := statusFor(errors.New("boom")); status != 500 || code != "internal" {
		t.Fatalf("got %d %q", status, code)
	}
	if status, _ := statusFor(fmt.Errorf("wrapped: %w", ledger.ErrInsufficientFunds)); status != 402 {
		t.Fatalf("wrapped domain error not matched: %d", status)
	}
}

func TestReplayedRefusalIsFlagged(t *testing.T) {
	c, _ := seeded(t)
	body := `{"currency":"VND","from":"alice","to":"bob","amount":5000}`

	first := c.do("POST", "/v1/transfers", body, IdempotencyKeyHeader, "big")
	if first.Code != http.StatusPaymentRequired || first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("first: %d %v", first.Code, first.Header())
	}
	// Funds arrive, then the client retries the same request.
	if rec := c.do("POST", "/v1/transfers", `{"currency":"VND","from":"funding","to":"alice","amount":10000}`,
		IdempotencyKeyHeader, "topup"); rec.Code != http.StatusCreated {
		t.Fatalf("topup: %d", rec.Code)
	}
	retry := c.do("POST", "/v1/transfers", body, IdempotencyKeyHeader, "big")
	if retry.Code != http.StatusPaymentRequired || retry.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("retry: %d %v %s", retry.Code, retry.Header(), retry.Body)
	}
	if errorCode(t, retry) != "insufficient_funds" {
		t.Fatalf("code = %s", errorCode(t, retry))
	}
}
