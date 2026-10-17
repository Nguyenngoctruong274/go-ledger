//go:build integration

package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestMetricsReflectTheBooks checks that what /metrics reports matches what
// actually happened, after the outbox has drained.
func TestMetricsReflectTheBooks(t *testing.T) {
	s := newStack(t)
	s.mustCall(http.StatusOK, "GET", "/readyz", "")

	s.createAccount("funding", "VND", true)
	s.createAccount("a", "VND", false)
	s.createAccount("b", "VND", false)
	s.pay("t1", "funding", "a", 500)
	s.pay("t1", "funding", "a", 500) // replay
	s.pay("t2", "a", "b", 200)
	s.pay("t3", "a", "b", 10_000) // refused
	s.waitForEvents(2)

	var out string
	deadline := time.Now().Add(5 * time.Second)
	for {
		out = string(s.mustCall(http.StatusOK, "GET", "/metrics", "").Body)
		if strings.Contains(out, "outbox_pending_messages 0\n") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, want := range []string{
		`ledger_transfers_total{outcome="posted"} 2`,
		`ledger_transfers_total{outcome="replayed"} 1`,
		`ledger_transfers_total{outcome="rejected"} 1`,
		`outbox_published_total{topic="transfer.posted"} 2`,
		"outbox_pending_messages 0\n",
		"outbox_oldest_pending_age_seconds 0\n",
		`http_requests_total{method="POST",route="/v1/transfers",status="201"} 2`,
		`http_requests_total{method="POST",route="/v1/transfers",status="402"} 1`,
		"db_pool_max_open_connections 20\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if t.Failed() {
		t.Logf("scrape:\n%s", out)
	}
}

// TestReadinessFollowsTheDatabase closes the database handle under the
// running API: liveness must stay green, readiness must go red.
func TestReadinessFollowsTheDatabase(t *testing.T) {
	s := newStack(t)
	s.mustCall(http.StatusOK, "GET", "/readyz", "")

	_ = s.db.Close()

	s.mustCall(http.StatusOK, "GET", "/healthz", "")
	if r := s.call("GET", "/readyz", ""); r.Status != http.StatusServiceUnavailable {
		t.Fatalf("readyz with database gone: %d %s", r.Status, r.Body)
	}
}
