// Command webhook-sink is a reference receiver for ledgerd's outbox
// webhooks. It verifies the HMAC signature and the timestamp, ignores
// duplicate deliveries, and logs each event. It is what the Docker Compose
// stack points ledgerd at, and a starting point for a real consumer.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	secret := os.Getenv("SINK_SECRET")
	if len(secret) < 16 {
		fmt.Fprintln(os.Stderr, "webhook-sink: SINK_SECRET must be at least 16 bytes")
		os.Exit(1)
	}
	addr := os.Getenv("SINK_ADDR")
	if addr == "" {
		addr = ":9000"
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           newSink([]byte(secret), 5*time.Minute, time.Now, logger),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("webhook-sink listening", slog.String("addr", addr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("webhook-sink", slog.Any("error", err))
		os.Exit(1)
	}
}
