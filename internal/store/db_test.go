package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"resy-snipe/internal/store"
)

func TestOpenSetsPragmas(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if got := readStringPragma(t, db, "journal_mode"); got != "wal" {
		t.Errorf("journal_mode = %q, want wal", got)
	}
	if got := readIntPragma(t, db, "foreign_keys"); got != 1 {
		t.Errorf("foreign_keys = %d, want 1", got)
	}
	if got := readIntPragma(t, db, "busy_timeout"); got != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", got)
	}
	// synchronous: 0=OFF, 1=NORMAL, 2=FULL, 3=EXTRA
	if got := readIntPragma(t, db, "synchronous"); got != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", got)
	}
}

func TestOpenCreatesParentDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c", "test.db")

	db, err := store.Open(context.Background(), nested)
	if err != nil {
		t.Fatalf("Open with nested path: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("db file not created at %s: %v", nested, err)
	}
}

func TestOpenWritesAndReads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "rw.db")

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO t(v) VALUES (?)`, "hello"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	var got string
	if err := db.QueryRowContext(ctx, `SELECT v FROM t LIMIT 1`).Scan(&got); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if got != "hello" {
		t.Fatalf("read back %q, want hello", got)
	}
}

// Canceled contexts must propagate through the helper rather than
// silently producing a connection.
func TestOpenRespectsContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ctx.db")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	db, err := store.Open(ctx, path)
	if err == nil {
		_ = db.Close()
		t.Fatal("Open with canceled ctx should error")
	}
}

func TestDefaultDBPathHonorsXDG(t *testing.T) {
	// t.Setenv requires no Parallel.
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	path, err := store.DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	want := filepath.Join(xdg, "resy-snipe", "db.sqlite")
	if path != want {
		t.Fatalf("got %s want %s", path, want)
	}
}

func TestDefaultDBPathFallsBackToHome(t *testing.T) {
	// t.Setenv requires no Parallel.
	t.Setenv("XDG_DATA_HOME", "")

	path, err := store.DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir unavailable: %v", err)
	}
	want := filepath.Join(home, ".local", "share", "resy-snipe", "db.sqlite")
	if path != want {
		t.Fatalf("got %s want %s", path, want)
	}
}

// Helpers ---------------------------------------------------------

func readStringPragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRowContext(context.Background(), `PRAGMA `+name).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return v
}

func readIntPragma(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var v int
	if err := db.QueryRowContext(context.Background(), `PRAGMA `+name).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return v
}
