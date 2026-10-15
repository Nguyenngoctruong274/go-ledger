package outbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// LogPublisher writes messages to a logger. It lets the service run end to
// end without a broker, e.g. in local development.
type LogPublisher struct {
	Logger *slog.Logger
}

// Publish logs m and always succeeds.
func (p LogPublisher) Publish(ctx context.Context, m Message) error {
	p.Logger.InfoContext(ctx, "outbox event",
		slog.Int64("id", m.ID), slog.String("topic", m.Topic), slog.String("key", m.Key),
		slog.String("payload", string(m.Payload)))
	return nil
}

// Webhook header names.
const (
	HeaderSignature = "X-Ledger-Signature"
	HeaderTimestamp = "X-Ledger-Timestamp"
	HeaderTopic     = "X-Ledger-Topic"
	HeaderMessageID = "X-Ledger-Message-Id"
)

// WebhookPublisher POSTs each message as JSON to a URL. The body is signed
// with HMAC-SHA256 over "<timestamp>.<body>" so receivers can verify the
// sender and reject replays of old requests. Any non-2xx answer is a failure
// and the relay retries it with backoff.
type WebhookPublisher struct {
	URL    string
	Secret []byte
	Client *http.Client     // defaults to a client with a 10s timeout
	Now    func() time.Time // defaults to time.Now; overridable in tests
}

// Publish sends m and returns an error unless the receiver answers 2xx.
func (p WebhookPublisher) Publish(ctx context.Context, m Message) error {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	ts := strconv.FormatInt(now().Unix(), 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(m.Payload))
	if err != nil {
		return fmt.Errorf("webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderTopic, m.Topic)
	req.Header.Set(HeaderMessageID, strconv.FormatInt(m.ID, 10))
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderSignature, "sha256="+Sign(p.Secret, ts, m.Payload))

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // allow connection reuse

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook: %w: receiver answered %s", ErrPublish, resp.Status)
	}
	return nil
}

// Sign returns the hex HMAC-SHA256 of "<timestamp>.<body>" under secret.
// Receivers recompute it and compare with hmac.Equal.
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
