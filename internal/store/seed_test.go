package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/store"
)

// openMigratedWithV1Fixture opens a fresh DB, lays down the v1 schema
// + a couple of v1 rows (mirroring the migrate_test pattern), then
// runs Migrate so the v2 rebind happens with real v1 data. The
// returned DB has two `accounts` rows with user_id IS NULL, ready for
// SeedOperator + BackfillOperatorAccounts.
func openMigratedWithV1Fixture(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := applyV1Schema(ctx, db); err != nil {
		t.Fatalf("applyV1Schema: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, created_at) VALUES (?, ?), (?, ?)`,
		"phall@example.com", "2026-04-01T10:00:00.000Z",
		"james@example.com", "2026-04-02T11:00:00.000Z",
	); err != nil {
		t.Fatalf("seed v1 users: %v", err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func TestSeedOperatorInsertsAdminRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openMigratedWithV1Fixture(t)

	uid, err := store.SeedOperator(ctx, db, store.SeedOpts{
		Email:        "phall@example.com",
		PasswordHash: []byte("argon2id$placeholder"),
	})
	if err != nil {
		t.Fatalf("SeedOperator: %v", err)
	}
	if uid == "" {
		t.Fatal("SeedOperator returned empty UserID")
	}

	// Exactly one row, role=admin, invited_by IS NULL.
	var (
		count      int
		id, email  string
		role       string
		invitedBy  sql.NullString
		gotHashLen int
	)
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("users row count = %d, want 1", count)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT id, email, role, invited_by, length(password_hash)
		FROM users LIMIT 1`).
		Scan(&id, &email, &role, &invitedBy, &gotHashLen); err != nil {
		t.Fatal(err)
	}
	if domain.UserID(id) != uid {
		t.Errorf("row id = %q, want %q", id, uid)
	}
	if email != "phall@example.com" {
		t.Errorf("email = %q", email)
	}
	if role != "admin" {
		t.Errorf("role = %q, want admin", role)
	}
	if invitedBy.Valid {
		t.Errorf("invited_by = %q, want NULL", invitedBy.String)
	}
	if gotHashLen != len("argon2id$placeholder") {
		t.Errorf("password_hash length = %d, want %d",
			gotHashLen, len("argon2id$placeholder"))
	}
}

func TestSeedOperatorIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openMigratedWithV1Fixture(t)

	uid1, err := store.SeedOperator(ctx, db, store.SeedOpts{
		Email: "op@example.com",
	})
	if err != nil {
		t.Fatalf("SeedOperator #1: %v", err)
	}
	uid2, err := store.SeedOperator(ctx, db, store.SeedOpts{
		Email:        "op@example.com",
		PasswordHash: []byte("different-hash-should-be-ignored"),
	})
	if err != nil {
		t.Fatalf("SeedOperator #2: %v", err)
	}
	if uid1 != uid2 {
		t.Errorf("idempotent SeedOperator returned different ids: %q vs %q", uid1, uid2)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("users count after double-seed = %d, want 1", n)
	}

	// The second call must not overwrite an existing password_hash —
	// INSERT OR IGNORE preserves the original row.
	var hash []byte
	if err := db.QueryRowContext(ctx, `SELECT password_hash FROM users`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if len(hash) != 0 {
		t.Errorf("password_hash length = %d, want 0 (no-op should preserve original)",
			len(hash))
	}
}

func TestSeedOperatorExplicitUserID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openMigratedWithV1Fixture(t)

	want := "usr_pinned123"
	uid, err := store.SeedOperator(ctx, db, store.SeedOpts{
		Email:  "op@example.com",
		UserID: want,
	})
	if err != nil {
		t.Fatalf("SeedOperator: %v", err)
	}
	if string(uid) != want {
		t.Fatalf("uid = %q, want %q", uid, want)
	}

	var id string
	if err := db.QueryRowContext(ctx, `SELECT id FROM users`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != want {
		t.Errorf("persisted id = %q, want %q", id, want)
	}
}

func TestSeedOperatorRejectsSecondOperator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openMigratedWithV1Fixture(t)

	if _, err := store.SeedOperator(ctx, db, store.SeedOpts{
		Email: "first@example.com",
	}); err != nil {
		t.Fatalf("seed first: %v", err)
	}

	// A second non-invited (invited_by=NULL) user must collide on the
	// partial unique index idx_users_one_operator. We try the direct
	// INSERT path (Service layer code) — SeedOperator with a
	// different email would be a different call site, but the schema
	// guarantee is what we're testing.
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES ('usr_smuggled', 'evil@example.com', x'00', 'admin', NULL, 0)`)
	if err == nil {
		t.Fatal("expected partial unique index to reject second NULL-inviter user")
	}
}

func TestSeedOperatorRequiresEmail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openMigratedWithV1Fixture(t)

	_, err := store.SeedOperator(ctx, db, store.SeedOpts{})
	if err == nil {
		t.Fatal("expected error for empty email")
	}
	if !errors.Is(err, store.ErrSeedFailed) {
		t.Errorf("err = %v, want wrap of ErrSeedFailed", err)
	}
}

func TestBackfillOperatorAccounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openMigratedWithV1Fixture(t)

	uid, err := store.SeedOperator(ctx, db, store.SeedOpts{Email: "op@example.com"})
	if err != nil {
		t.Fatalf("SeedOperator: %v", err)
	}

	// Sanity: two v1-imported accounts, both with user_id NULL.
	var nNull int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM accounts WHERE user_id IS NULL`).Scan(&nNull); err != nil {
		t.Fatal(err)
	}
	if nNull != 2 {
		t.Fatalf("pre-backfill NULL accounts = %d, want 2", nNull)
	}

	n, err := store.BackfillOperatorAccounts(ctx, db, uid)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if n != 2 {
		t.Fatalf("Backfill rows = %d, want 2", n)
	}

	// All accounts now bound to the operator.
	rows, err := db.QueryContext(ctx, `SELECT user_id FROM accounts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var u sql.NullString
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		if !u.Valid || u.String != string(uid) {
			t.Errorf("account user_id = %v, want %q", u, uid)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// Second call is a no-op.
	n2, err := store.BackfillOperatorAccounts(ctx, db, uid)
	if err != nil {
		t.Fatalf("Backfill #2: %v", err)
	}
	if n2 != 0 {
		t.Errorf("Backfill #2 rows = %d, want 0 (idempotent)", n2)
	}
}

func TestBackfillRequiresOperatorID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openMigratedWithV1Fixture(t)

	_, err := store.BackfillOperatorAccounts(ctx, db, "")
	if err == nil {
		t.Fatal("expected error for empty operatorID")
	}
	if !errors.Is(err, store.ErrSeedFailed) {
		t.Errorf("err = %v, want wrap of ErrSeedFailed", err)
	}
}
