package secrets_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"resy-snipe/internal/store"
)

const (
	testUserID    = "usr_test01"
	testAccountID = "acct_test01"
	testEmail     = "test@example.com"
)

// openTestDB returns a migrated *sql.DB with one users row and one
// accounts row seeded so secrets-table INSERTs satisfy the FK
// constraints. Cleanup is registered with t.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "secrets.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES (?, ?, x'', 'admin', NULL, strftime('%s','now')*1000)`,
		testUserID, testEmail,
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
		VALUES (?, ?, ?, 'test', strftime('%s','now')*1000)`,
		testAccountID, testUserID, testEmail,
	); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return db
}
