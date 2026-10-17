package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Nguyenngoctruong274/go-ledger/internal/metrics"
)

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: %d", rec.Code)
	}
	return rec.Body.String()
}

func TestMetricsUseRoutePatternsNotPaths(t *testing.T) {
	reg := metrics.NewRegistry()
	f := newFakeLedger()
	c := &client{t: t, h: NewHandler(discardLogger(), f, Options{Metrics: reg})}

	c.do("POST", "/v1/accounts", `{"id":"acc-1","currency":"VND"}`)
	c.do("GET", "/v1/accounts/acc-1", "")
	c.do("GET", "/v1/accounts/acc-2", "") // 404
	c.do("GET", "/nope", "")

	out := scrape(t, c.h)
	for _, want := range []string{
		`http_requests_total{method="POST",route="/v1/accounts",status="201"} 1`,
		`http_requests_total{method="GET",route="/v1/accounts/{id}",status="200"} 1`,
		`http_requests_total{method="GET",route="/v1/accounts/{id}",status="404"} 1`,
		`http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		`http_request_duration_seconds_count{method="GET",route="/v1/accounts/{id}"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, "acc-1") || strings.Contains(out, "acc-2") {
		t.Fatal("raw ids leaked into metric labels")
	}
}

func TestTransferOutcomeMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	f := newFakeLedger()
	c := &client{t: t, h: NewHandler(discardLogger(), f, Options{Metrics: reg})}
	for _, body := range []string{
		`{"id":"funding","currency":"VND","allow_negative":true}`,
		`{"id":"alice","currency":"VND"}`,
		`{"id":"bob","currency":"VND"}`,
	} {
		c.do("POST", "/v1/accounts", body)
	}
	pay := func(key, body string) { c.do("POST", "/v1/transfers", body, IdempotencyKeyHeader, key) }

	pay("t1", `{"currency":"VND","from":"funding","to":"alice","amount":100}`) // posted
	pay("t1", `{"currency":"VND","from":"funding","to":"alice","amount":100}`) // replayed
	pay("t1", `{"currency":"VND","from":"funding","to":"alice","amount":5}`)   // conflict
	pay("t2", `{"currency":"VND","from":"alice","to":"bob","amount":999}`)     // rejected (funds)
	pay("t3", `{"currency":"VND","from":"alice","to":"bob","amount":-1}`)      // invalid
	pay("", `{"currency":"VND","from":"alice","to":"bob","amount":1}`)         // invalid (no key)

	out := scrape(t, c.h)
	for outcome, n := range map[string]string{"posted": "1", "replayed": "1", "conflict": "1", "rejected": "1", "invalid": "2"} {
		want := `ledger_transfers_total{outcome="` + outcome + `"} ` + n
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestMetricsEndpointOnlyWhenConfigured(t *testing.T) {
	h := NewHandler(discardLogger(), newFakeLedger(), Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/metrics without a registry: %d, want 404", rec.Code)
	}
}

func TestReadiness(t *testing.T) {
	var readyErr error
	h := NewHandler(discardLogger(), newFakeLedger(), Options{
		Ready: func(context.Context) error { return readyErr },
	})
	probe := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code
	}

	if code := probe(); code != http.StatusOK {
		t.Fatalf("ready: %d", code)
	}
	readyErr = errors.New("pq: connection refused to 10.0.0.5")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not ready: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatal("readiness leaked error details")
	}
	// Liveness is independent of readiness.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz while not ready: %d", rec.Code)
	}
}

func TestRequestIDs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h := NewHandler(logger, newFakeLedger(), Options{})

	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{"keeps a valid id", "trace-abc_123.x", true},
		{"replaces an empty id", "", false},
		{"replaces an id with spaces", "evil id", false},
		{"replaces a log-injection attempt", "a\nlevel=ERROR", false},
		{"replaces an overlong id", strings.Repeat("a", 65), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs.Reset()
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			if tt.incoming != "" {
				req.Header.Set(RequestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get(RequestIDHeader)
			if tt.keep && got != tt.incoming {
				t.Fatalf("id = %q, want incoming %q", got, tt.incoming)
			}
			if !tt.keep && (got == tt.incoming || len(got) != 16) {
				t.Fatalf("id = %q, want a fresh 16-char id", got)
			}
			if !strings.Contains(logs.String(), `"request_id":"`+got+`"`) {
				t.Fatalf("log line lacks request_id %s: %s", got, logs.String())
			}
		})
	}
}
