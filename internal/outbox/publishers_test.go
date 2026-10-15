package outbox

import (
	"context"
	"crypto/hmac"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhookPublisherSignsAndSends(t *testing.T) {
	secret := []byte("s3cret")
	var gotBody []byte
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	pub := WebhookPublisher{
		URL: srv.URL, Secret: secret,
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	msg := Message{ID: 42, Topic: "transfer.posted", Key: "t1", Payload: []byte(`{"transfer_id":"t1"}`)}
	if err := pub.Publish(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	if string(gotBody) != string(msg.Payload) {
		t.Fatalf("body = %s", gotBody)
	}
	if gotHeaders.Get(HeaderTopic) != "transfer.posted" || gotHeaders.Get(HeaderMessageID) != "42" ||
		gotHeaders.Get(HeaderTimestamp) != "1700000000" {
		t.Fatalf("headers = %v", gotHeaders)
	}

	// What a receiver does: recompute and compare in constant time.
	want := "sha256=" + Sign(secret, gotHeaders.Get(HeaderTimestamp), gotBody)
	if !hmac.Equal([]byte(gotHeaders.Get(HeaderSignature)), []byte(want)) {
		t.Fatalf("signature %q, want %q", gotHeaders.Get(HeaderSignature), want)
	}
	if Sign([]byte("other"), "1700000000", gotBody) == strings.TrimPrefix(want, "sha256=") {
		t.Fatal("signature does not depend on the secret")
	}
}

func TestWebhookPublisherFailsOnNon2xx(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusMovedPermanently} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		err := WebhookPublisher{URL: srv.URL}.Publish(context.Background(), Message{Payload: []byte(`{}`)})
		srv.Close()
		if !errors.Is(err, ErrPublish) {
			t.Fatalf("status %d: error = %v, want ErrPublish", status, err)
		}
	}
}

func TestWebhookPublisherTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	pub := WebhookPublisher{URL: srv.URL, Client: &http.Client{Timeout: 50 * time.Millisecond}}
	if err := pub.Publish(context.Background(), Message{Payload: []byte(`{}`)}); err == nil {
		t.Fatal("expected timeout error")
	}
}
