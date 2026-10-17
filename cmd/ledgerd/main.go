// Command ledgerd runs the go-ledger HTTP service.
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

	"github.com/Nguyenngoctruong274/go-ledger/internal/config"
	"github.com/Nguyenngoctruong274/go-ledger/internal/httpapi"
	"github.com/Nguyenngoctruong274/go-ledger/internal/outbox"
	"github.com/Nguyenngoctruong274/go-ledger/internal/postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ledgerd:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStart()

	db, err := postgres.Open(startCtx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := postgres.Migrate(startCtx, db); err != nil {
		return err
	}
	logger.Info("database ready")

	// The relay runs until shutdown. It is stopped and awaited before the
	// database handle is closed (defers run in reverse order).
	relayCtx, stopRelay := context.WithCancel(context.Background())
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		_ = outbox.NewRelay(db, newPublisher(cfg, logger), logger, outbox.Options{}).Run(relayCtx)
	}()
	defer func() {
		stopRelay()
		<-relayDone
		logger.Info("outbox relay stopped")
	}()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewHandler(logger, postgres.NewStore(db)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("ledgerd listening", slog.String("addr", cfg.HTTPAddr))
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down", slog.Duration("timeout", cfg.ShutdownTimeout))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	logger.Info("stopped")
	return nil
}

// newPublisher picks where outbox events go: a signed webhook when one is
// configured, otherwise the log (useful locally, never loses an event).
func newPublisher(cfg config.Config, logger *slog.Logger) outbox.Publisher {
	if cfg.OutboxWebhookURL == "" {
		logger.Info("outbox: no webhook configured, events go to the log")
		return outbox.LogPublisher{Logger: logger}
	}
	logger.Info("outbox: publishing to webhook", slog.String("url", cfg.OutboxWebhookURL))
	return outbox.WebhookPublisher{URL: cfg.OutboxWebhookURL, Secret: []byte(cfg.OutboxWebhookSecret)}
}
