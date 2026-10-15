// Package config loads runtime configuration for ledgerd from the environment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Config is the complete runtime configuration of the service.
type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	ShutdownTimeout time.Duration
	LogLevel        string

	// OutboxWebhookURL receives outbox events when set; otherwise they are logged.
	OutboxWebhookURL    string
	OutboxWebhookSecret string
}

// Load reads configuration from environment variables, applying defaults
// for optional values. It reports every invalid value at once so that a
// misconfigured deployment fails fast with a complete error message.
func Load() (Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (Config, error) {
	cfg := Config{
		HTTPAddr:        valueOr(getenv("LEDGER_HTTP_ADDR"), ":8080"),
		DatabaseURL:     getenv("LEDGER_DATABASE_URL"),
		ShutdownTimeout: 15 * time.Second,
		LogLevel:        valueOr(getenv("LEDGER_LOG_LEVEL"), "info"),

		OutboxWebhookURL:    getenv("LEDGER_OUTBOX_WEBHOOK_URL"),
		OutboxWebhookSecret: getenv("LEDGER_OUTBOX_WEBHOOK_SECRET"),
	}

	var errs []error

	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("LEDGER_DATABASE_URL: required"))
	}

	if raw := getenv("LEDGER_SHUTDOWN_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("LEDGER_SHUTDOWN_TIMEOUT: %w", err))
		case d <= 0:
			errs = append(errs, errors.New("LEDGER_SHUTDOWN_TIMEOUT: must be positive"))
		default:
			cfg.ShutdownTimeout = d
		}
	}

	if cfg.OutboxWebhookURL != "" {
		u, err := url.Parse(cfg.OutboxWebhookURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("LEDGER_OUTBOX_WEBHOOK_URL: must be an absolute http(s) URL"))
		}
		if len(cfg.OutboxWebhookSecret) < 16 {
			errs = append(errs, errors.New("LEDGER_OUTBOX_WEBHOOK_SECRET: required with a webhook URL, at least 16 bytes"))
		}
	}

	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("LEDGER_LOG_LEVEL: unsupported level %s", strconv.Quote(cfg.LogLevel)))
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
