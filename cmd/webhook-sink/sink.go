package main

import (
	"crypto/hmac"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Nguyenngoctruong274/go-ledger/internal/outbox"
)

// sink verifies and records webhook deliveries.
type sink struct {
	secret    []byte
	tolerance time.Duration
	now       func() time.Time
	logger    *slog.Logger

	mu   sync.Mutex
	seen map[string]bool // message ids already processed (a real consumer would persist this)
}

func newSink(secret []byte, tolerance time.Duration, now func() time.Time, logger *slog.Logger) *sink {
	return &sink{secret: secret, tolerance: tolerance, now: now, logger: logger, seen: map[string]bool{}}
}

func (s *sink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// 1. Authenticity: the signature covers the timestamp and the body.
	ts := r.Header.Get(outbox.HeaderTimestamp)
	want := "sha256=" + outbox.Sign(s.secret, ts, body)
	if !hmac.Equal([]byte(r.Header.Get(outbox.HeaderSignature)), []byte(want)) {
		s.logger.Warn("rejected: bad signature")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// 2. Freshness: a captured request cannot be replayed later.
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || s.now().Sub(time.Unix(sec, 0)).Abs() > s.tolerance {
		s.logger.Warn("rejected: stale or invalid timestamp", slog.String("timestamp", ts))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// 3. Idempotency: delivery is at-least-once, so acknowledge duplicates
	//    without processing them twice.
	id := r.Header.Get(outbox.HeaderMessageID)
	s.mu.Lock()
	dup := s.seen[id]
	s.seen[id] = true
	s.mu.Unlock()
	if dup {
		s.logger.Info("duplicate delivery ignored", slog.String("message_id", id))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.logger.Info("event",
		slog.String("message_id", id),
		slog.String("topic", r.Header.Get(outbox.HeaderTopic)),
		slog.String("payload", string(body)))
	w.WriteHeader(http.StatusNoContent)
}
