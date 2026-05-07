package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"resy-snipe/internal/store"
)

func openMigrated(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "migrate.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func TestMigrateCreatesAllTables(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	want := []string{
		"events",
		"observed_release_times",
		"schema_migrations",
		"sessions",
		"snipes",
		"users",
		"venues",
	}
	got := listTables(t, db)
	for _, name := range want {
		if !contains(got, name) {
			t.Errorf("missing table %q (got %v)", name, got)
		}
	}
}

func TestMigrateRecordsAppliedVersions(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	rows, err := db.QueryContext(context.Background(),
		`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()

	type row struct {
		version int
		name    string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.version, &r.name); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 1 || got[0].version != 1 || got[0].name != "initial" {
		t.Fatalf("schema_migrations rows: %+v", got)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	// Already migrated once via openMigrated. Run twice more.
	for i := 0; i < 2; i++ {
		if err := store.Migrate(context.Background(), db); err != nil {
			t.Fatalf("Migrate run %d: %v", i+2, err)
		}
	}
	rows, err := db.QueryContext(context.Background(), `SELECT COUNT(*) FROM schema_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("count missing")
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("schema_migrations row count = %d, want 1 after triple-Migrate", n)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `
		INSERT INTO events (snipe_id, type, at)
		VALUES ('does_not_exist', 'submitted', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Fatal("expected FK violation when inserting event for missing snipe")
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (user_id, provider, jwt, exp)
		VALUES ('phantom_user', 'resy', 'jwt', '2026-12-31T00:00:00Z')`)
	if err == nil {
		t.Fatal("expected FK violation when inserting session for missing user")
	}
}

func TestSnipesAndEventsCanBeWritten(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO snipes (id, intent_hash, intent_json, status) VALUES (?, ?, ?, ?)`,
		"snp_1", "ihash", `{"user":"u"}`, "submitted",
	); err != nil {
		t.Fatalf("insert snipe: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO events (snipe_id, type, at, fields_json) VALUES (?, ?, ?, ?)`,
		"snp_1", "submitted", "2026-01-01T00:00:00Z", `{}`,
	); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM snipes WHERE id = ?`, "snp_1").Scan(&status); err != nil {
		t.Fatalf("select snipe: %v", err)
	}
	if status != "submitted" {
		t.Fatalf("status = %q, want submitted", status)
	}
}

// listTables / contains -------------------------------------------

func listTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
