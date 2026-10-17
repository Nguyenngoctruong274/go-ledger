package config

import (
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name: "defaults",
			env:  map[string]string{"LEDGER_DATABASE_URL": "postgres://localhost/ledger"},
			want: Config{
				HTTPAddr:        ":8080",
				DatabaseURL:     "postgres://localhost/ledger",
				ShutdownTimeout: 15 * time.Second,
				LogLevel:        "info",
			},
		},
		{
			name: "overrides",
			env: map[string]string{
				"LEDGER_HTTP_ADDR":        ":9090",
				"LEDGER_DATABASE_URL":     "postgres://localhost/ledger",
				"LEDGER_SHUTDOWN_TIMEOUT": "5s",
				"LEDGER_LOG_LEVEL":        "debug",
			},
			want: Config{
				HTTPAddr:        ":9090",
				DatabaseURL:     "postgres://localhost/ledger",
				ShutdownTimeout: 5 * time.Second,
				LogLevel:        "debug",
			},
		},
		{
			name: "webhook",
			env: map[string]string{
				"LEDGER_DATABASE_URL":          "x",
				"LEDGER_OUTBOX_WEBHOOK_URL":    "https://hooks.example.com/ledger",
				"LEDGER_OUTBOX_WEBHOOK_SECRET": "0123456789abcdef",
			},
			want: Config{
				HTTPAddr: ":8080", DatabaseURL: "x", ShutdownTimeout: 15 * time.Second, LogLevel: "info",
				OutboxWebhookURL: "https://hooks.example.com/ledger", OutboxWebhookSecret: "0123456789abcdef",
			},
		},
		{name: "webhook without secret", env: map[string]string{"LEDGER_DATABASE_URL": "x", "LEDGER_OUTBOX_WEBHOOK_URL": "https://h.example.com"}, wantErr: true},
		{name: "webhook short secret", env: map[string]string{"LEDGER_DATABASE_URL": "x", "LEDGER_OUTBOX_WEBHOOK_URL": "https://h.example.com", "LEDGER_OUTBOX_WEBHOOK_SECRET": "short"}, wantErr: true},
		{name: "webhook relative url", env: map[string]string{"LEDGER_DATABASE_URL": "x", "LEDGER_OUTBOX_WEBHOOK_URL": "/hook", "LEDGER_OUTBOX_WEBHOOK_SECRET": "0123456789abcdef"}, wantErr: true},
		{name: "webhook ftp url", env: map[string]string{"LEDGER_DATABASE_URL": "x", "LEDGER_OUTBOX_WEBHOOK_URL": "ftp://h.example.com", "LEDGER_OUTBOX_WEBHOOK_SECRET": "0123456789abcdef"}, wantErr: true},
		{
			name: "drain",
			env:  map[string]string{"LEDGER_DATABASE_URL": "x", "LEDGER_SHUTDOWN_DRAIN": "5s"},
			want: Config{HTTPAddr: ":8080", DatabaseURL: "x", ShutdownTimeout: 15 * time.Second, ShutdownDrain: 5 * time.Second, LogLevel: "info"},
		},
		{name: "bad drain", env: map[string]string{"LEDGER_DATABASE_URL": "x", "LEDGER_SHUTDOWN_DRAIN": "-1s"}, wantErr: true},
		{name: "missing database url", env: map[string]string{}, wantErr: true},
		{name: "bad duration", env: map[string]string{"LEDGER_DATABASE_URL": "x", "LEDGER_SHUTDOWN_TIMEOUT": "soon"}, wantErr: true},
		{name: "non-positive duration", env: map[string]string{"LEDGER_DATABASE_URL": "x", "LEDGER_SHUTDOWN_TIMEOUT": "0s"}, wantErr: true},
		{name: "bad log level", env: map[string]string{"LEDGER_DATABASE_URL": "x", "LEDGER_LOG_LEVEL": "loud"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := load(func(k string) string { return tt.env[k] })
			if (err != nil) != tt.wantErr {
				t.Fatalf("load() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
