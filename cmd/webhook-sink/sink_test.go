package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Nguyenngoctruong274/go-ledger/internal/outbox"
)

func TestSink(t *testing.T) {
	secret := []byte("sink-secret-0123456789")
	now := time.Unix(1_800_000_000, 0)
	s := newSink(secret, 5*time.Minute, func() time.Time { return now }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := []byte(`{"transfer_id":"t1"}`)

	send := func(id string, ts time.Time, sig string, method string) int {
		req := httptest.NewRequest(method, "/", bytes.NewReader(body))
		stamp := strconv.FormatInt(ts.Unix(), 10)
		if sig == "" {
			sig = "sha256=" + outbox.Sign(secret, stamp, body)
		}
		req.Header.Set(outbox.HeaderTimestamp, stamp)
		req.Header.Set(outbox.HeaderSignature, sig)
		req.Header.Set(outbox.HeaderMessageID, id)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Code
	}

	tests := []struct {
		name string
		code int
		call func() int
	}{
		{"valid", http.StatusNoContent, func() int { return send("1", now, "", http.MethodPost) }},
		{"duplicate is acknowledged", http.StatusNoContent, func() int { return send("1", now, "", http.MethodPost) }},
		{"forged signature", http.StatusUnauthorized, func() int { return send("2", now, "sha256=deadbeef", http.MethodPost) }},
		{"replayed old request", http.StatusUnauthorized, func() int { return send("3", now.Add(-10*time.Minute), "", http.MethodPost) }},
		{"timestamp from the future", http.StatusUnauthorized, func() int { return send("4", now.Add(10*time.Minute), "", http.MethodPost) }},
		{"within tolerance", http.StatusNoContent, func() int { return send("5", now.Add(-4*time.Minute), "", http.MethodPost) }},
		{"wrong method", http.StatusMethodNotAllowed, func() int { return send("6", now, "", http.MethodGet) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.call(); got != tt.code {
				t.Fatalf("status = %d, want %d", got, tt.code)
			}
		})
	}
	if len(s.seen) != 2 {
		t.Fatalf("processed ids = %v, want 1 and 5 only", s.seen)
	}
}

// TestSinkAcceptsRealPublisher wires the actual WebhookPublisher to the sink.
func TestSinkAcceptsRealPublisher(t *testing.T) {
	secret := []byte("sink-secret-0123456789")
	s := newSink(secret, 5*time.Minute, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(s)
	defer srv.Close()

	pub := outbox.WebhookPublisher{URL: srv.URL, Secret: secret}
	if err := pub.Publish(t.Context(), outbox.Message{ID: 7, Topic: "transfer.posted", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if !s.seen["7"] {
		t.Fatal("sink did not record the message")
	}
}
