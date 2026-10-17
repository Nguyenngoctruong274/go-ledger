//go:build integration

// Package integration drives the whole service from the outside: real HTTP
// handlers, the real PostgreSQL store, the real outbox relay and a webhook
// receiver that verifies signatures. Only the network edges are in-process.
//
// Run with: LEDGER_TEST_DATABASE_URL=... go test -tags integration ./test/...
package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Nguyenngoctruong274/go-ledger/internal/httpapi"
	"github.com/Nguyenngoctruong274/go-ledger/internal/outbox"
	"github.com/Nguyenngoctruong274/go-ledger/internal/postgres"
	"github.com/Nguyenngoctruong274/go-ledger/internal/testdb"
)

const webhookSecret = "integration-secret-0123456789"

// receivedEvent is one webhook delivery as seen by the receiver.
type receivedEvent struct {
	MessageID int64
	Event     postgres.TransferPostedEvent
}

// receiver is a webhook endpoint. It verifies every signature, can be told
// to fail the next N requests, and records accepted events in arrival order.
type receiver struct {
	t *testing.T

	mu       sync.Mutex
	events   []receivedEvent
	failNext int
	failures int
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ts := r.Header.Get(outbox.HeaderTimestamp)
	want := "sha256=" + outbox.Sign([]byte(webhookSecret), ts, body)
	if !hmac.Equal([]byte(r.Header.Get(outbox.HeaderSignature)), []byte(want)) {
		rc.t.Errorf("webhook: bad signature for message %s", r.Header.Get(outbox.HeaderMessageID))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.failNext > 0 {
		rc.failNext--
		rc.failures++
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	id, _ := strconv.ParseInt(r.Header.Get(outbox.HeaderMessageID), 10, 64)
	var ev postgres.TransferPostedEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		rc.t.Errorf("webhook: bad payload %s: %v", body, err)
	}
	rc.events = append(rc.events, receivedEvent{MessageID: id, Event: ev})
	w.WriteHeader(http.StatusNoContent)
}

func (rc *receiver) snapshot() []receivedEvent {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]receivedEvent(nil), rc.events...)
}

// stack is one running copy of the service.
type stack struct {
	t    *testing.T
	db   *sql.DB
	api  *httptest.Server
	hook *receiver
}

func newStack(t *testing.T) *stack {
	t.Helper()
	db := testdb.New(t, postgres.Migrate)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	hook := &receiver{t: t}
	hookSrv := httptest.NewServer(hook)
	t.Cleanup(hookSrv.Close)

	api := httptest.NewServer(httpapi.NewHandler(logger, postgres.NewStore(db)))
	t.Cleanup(api.Close)

	relay := outbox.NewRelay(db,
		outbox.WebhookPublisher{URL: hookSrv.URL, Secret: []byte(webhookSecret)},
		logger,
		outbox.Options{PollInterval: 20 * time.Millisecond, BaseBackoff: 30 * time.Millisecond, MaxBackoff: 200 * time.Millisecond},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = relay.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	return &stack{t: t, db: db, api: api, hook: hook}
}

type response struct {
	Status  int
	Header  http.Header
	Body    []byte
	Decoded map[string]any
}

func (s *stack) call(method, path, body string, headers ...string) response {
	s.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, s.api.URL+path, bytes.NewBufferString(body))
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := s.api.Client().Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := response{Status: resp.StatusCode, Header: resp.Header, Body: raw}
	_ = json.Unmarshal(raw, &r.Decoded)
	return r
}

func (s *stack) mustCall(wantStatus int, method, path, body string, headers ...string) response {
	s.t.Helper()
	r := s.call(method, path, body, headers...)
	if r.Status != wantStatus {
		s.t.Fatalf("%s %s: status %d, want %d: %s", method, path, r.Status, wantStatus, r.Body)
	}
	return r
}

func (s *stack) createAccount(id, currency string, allowNegative bool) {
	s.t.Helper()
	s.mustCall(http.StatusCreated, "POST", "/v1/accounts",
		fmt.Sprintf(`{"id":%q,"currency":%q,"allow_negative":%t}`, id, currency, allowNegative))
}

func (s *stack) pay(key, from, to string, amount int64) response {
	s.t.Helper()
	return s.call("POST", "/v1/transfers",
		fmt.Sprintf(`{"currency":"VND","from":%q,"to":%q,"amount":%d}`, from, to, amount),
		httpapi.IdempotencyKeyHeader, key)
}

func (s *stack) balance(id string) int64 {
	s.t.Helper()
	r := s.mustCall(http.StatusOK, "GET", "/v1/accounts/"+id, "")
	return int64(r.Decoded["balance"].(float64))
}

type statementEntry struct {
	EntryID      int64  `json:"entry_id"`
	TransferID   string `json:"transfer_id"`
	Amount       int64  `json:"amount"`
	BalanceAfter int64  `json:"balance_after"`
}

// statement walks every page of an account's entries via the public API
// and returns them oldest first.
func (s *stack) statement(id string) []statementEntry {
	s.t.Helper()
	var all []statementEntry
	path := "/v1/accounts/" + id + "/entries?limit=7" // small pages exercise the cursor
	for {
		r := s.mustCall(http.StatusOK, "GET", path, "")
		var page struct {
			Entries    []statementEntry `json:"entries"`
			NextBefore *int64           `json:"next_before"`
		}
		if err := json.Unmarshal(r.Body, &page); err != nil {
			s.t.Fatal(err)
		}
		all = append(all, page.Entries...)
		if page.NextBefore == nil {
			break
		}
		path = fmt.Sprintf("/v1/accounts/%s/entries?limit=7&before=%d", id, *page.NextBefore)
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all
}

// waitForEvents blocks until the receiver holds n events or fails the test.
func (s *stack) waitForEvents(n int) []receivedEvent {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		evs := s.hook.snapshot()
		if len(evs) >= n {
			return evs
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("received %d events, want %d", len(evs), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
