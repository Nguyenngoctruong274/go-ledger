package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadMigrations(t *testing.T) {
	tests := []struct {
		name    string
		files   fstest.MapFS
		want    []int
		wantErr string
	}{
		{
			name: "sorted by version",
			files: fstest.MapFS{
				"migrations/0010_later.sql": {Data: []byte("SELECT 10")},
				"migrations/0002_next.sql":  {Data: []byte("SELECT 2")},
				"migrations/0001_init.sql":  {Data: []byte("SELECT 1")},
			},
			want: []int{1, 2, 10},
		},
		{name: "empty", files: fstest.MapFS{}, wantErr: "no migrations"},
		{
			name:    "missing description",
			files:   fstest.MapFS{"migrations/0001.sql": {Data: []byte("x")}},
			wantErr: "name must look like",
		},
		{
			name:    "non-numeric version",
			files:   fstest.MapFS{"migrations/abc_init.sql": {Data: []byte("x")}},
			wantErr: "invalid version",
		},
		{
			name: "duplicate version",
			files: fstest.MapFS{
				"migrations/0001_a.sql": {Data: []byte("x")},
				"migrations/1_b.sql":    {Data: []byte("y")},
			},
			wantErr: "share version 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadMigrations(tt.files)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d migrations, want %d", len(got), len(tt.want))
			}
			for i, m := range got {
				if m.version != tt.want[i] {
					t.Fatalf("migration %d has version %d, want %d", i, m.version, tt.want[i])
				}
			}
		})
	}
}

func TestEmbeddedMigrationsLoad(t *testing.T) {
	ms, err := loadMigrations(migrationFS)
	if err != nil {
		t.Fatal(err)
	}
	if ms[0].version != 1 || !strings.Contains(ms[0].sql, "CREATE TABLE accounts") {
		t.Fatalf("unexpected first migration: %s", ms[0].name)
	}
}

// TestMigration0002BackfillsRunningBalances applies 0001, writes entries the
// old way (no balance_after), then applies 0002 and checks the backfill.
func TestMigration0002BackfillsRunningBalances(t *testing.T) {
	if os.Getenv("LEDGER_TEST_DATABASE_URL") == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping PostgreSQL tests")
	}
	db := openEmptyTestDB(t)
	ctx := context.Background()

	first, err := fs.ReadFile(migrationFS, "migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, fstest.MapFS{"migrations/0001_init.sql": {Data: first}}); err != nil {
		t.Fatal(err)
	}

	stmts := []string{
		`INSERT INTO accounts (id, currency, allow_negative) VALUES ('f', 'VND', true), ('w', 'VND', false)`,
		`BEGIN`,
		`INSERT INTO transfers (id, currency) VALUES ('t1', 'VND')`,
		`INSERT INTO entries (transfer_id, account_id, amount) VALUES ('t1', 'f', -500), ('t1', 'w', 500)`,
		`INSERT INTO transfers (id, currency) VALUES ('t2', 'VND')`,
		`INSERT INTO entries (transfer_id, account_id, amount) VALUES ('t2', 'w', -200), ('t2', 'f', 200)`,
		`COMMIT`,
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range stmts {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = conn.Close()

	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	rows, err := db.QueryContext(ctx, `SELECT transfer_id, account_id, balance_after FROM entries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []string{"t1/f/-500", "t1/w/500", "t2/w/300", "t2/f/-300"}
	var got []string
	for rows.Next() {
		var tr, acc string
		var after int64
		if err := rows.Scan(&tr, &acc, &after); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s/%s/%d", tr, acc, after))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("balance_after = %v, want %v", got, want)
	}

	// The append-only guard must be active again after the migration.
	if _, err := db.ExecContext(ctx, `UPDATE entries SET amount = 1 WHERE transfer_id = 't1'`); err == nil {
		t.Fatal("entries are writable after migration 0002")
	}
}
