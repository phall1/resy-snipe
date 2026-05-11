package secrets_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/secrets"
)

// migrateFixture wires the per-test scaffolding: a migrated DB with two
// (user, account) pairs and a Sealer ready to encrypt under a stable
// passphrase. Returns the sealer, the second account id, and the
// in-process clock so callers can pin timestamps.
type migrateFixture struct {
	db     *sql.DB
	sealer *secrets.Sealer
	user2  string
	acct2  string
	logBuf *bytes.Buffer
	log    *slog.Logger
}

func newMigrateFixture(t *testing.T) *migrateFixture {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	// Add a second (user, account) pair so we can exercise the
	// "two sessions migrate in one tx" path. Email differs so the
	// users.email UNIQUE doesn't reject the insert.
	const user2 = "usr_test02"
	const acct2 = "acct_test02"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES (?, ?, x'', 'user', ?, strftime('%s','now')*1000)`,
		user2, "test2@example.com", testUserID,
	); err != nil {
		t.Fatalf("seed user2: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
		VALUES (?, ?, ?, 'test2', strftime('%s','now')*1000)`,
		acct2, user2, "test2@example.com",
	); err != nil {
		t.Fatalf("seed account2: %v", err)
	}

	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	meta, err := secrets.LoadOrInitMeta(ctx, db, c)
	if err != nil {
		t.Fatalf("LoadOrInitMeta: %v", err)
	}
	key, err := secrets.DeriveKeyFromPassphrase([]byte("migrate-pass"), meta.KDFSalt)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	s, err := secrets.NewSealer(db, key, meta, c)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	buf := &bytes.Buffer{}
	return &migrateFixture{
		db:     db,
		sealer: s,
		user2:  user2,
		acct2:  acct2,
		logBuf: buf,
		log:    slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

// insertSession seeds a sessions row with the given plaintext jwt. The
// real session shape uses ISO8601 text for exp/created/updated — see
// migrations/0002 — but for the migration tool only `jwt` and
// `account_id` matter.
func insertSession(t *testing.T, db *sql.DB, accountID, jwt string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO sessions (account_id, provider, jwt, exp)
		VALUES (?, 'resy', ?, '2099-01-01T00:00:00.000Z')`,
		accountID, jwt,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

func readJWT(t *testing.T, db *sql.DB, accountID string) string {
	t.Helper()
	var jwt string
	if err := db.QueryRowContext(context.Background(),
		`SELECT jwt FROM sessions WHERE account_id = ?`, accountID,
	).Scan(&jwt); err != nil {
		t.Fatalf("read jwt: %v", err)
	}
	return jwt
}

func TestMigratePlaintextSessionsHappyPath(t *testing.T) {
	t.Parallel()
	f := newMigrateFixture(t)
	insertSession(t, f.db, testAccountID, "jwt-one")
	insertSession(t, f.db, f.acct2, "jwt-two")

	report, err := secrets.MigratePlaintextSessions(context.Background(), f.db, f.sealer, f.log, false)
	if err != nil {
		t.Fatalf("MigratePlaintextSessions: %v", err)
	}
	if report.Migrated != 2 {
		t.Fatalf("Migrated=%d, want 2", report.Migrated)
	}
	if report.Skipped != 0 {
		t.Fatalf("Skipped=%d, want 0", report.Skipped)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("Errors=%v, want none", report.Errors)
	}

	// Both plaintext JWTs are zeroed.
	if got := readJWT(t, f.db, testAccountID); got != "" {
		t.Errorf("jwt for %s = %q, want empty", testAccountID, got)
	}
	if got := readJWT(t, f.db, f.acct2); got != "" {
		t.Errorf("jwt for %s = %q, want empty", f.acct2, got)
	}

	// Both sealed rows Open back to the original plaintext.
	ctx := context.Background()
	plain, err := f.sealer.Open(ctx, testUserID, testAccountID, secrets.KindResySessionJWT)
	if err != nil {
		t.Fatalf("Open testAccount: %v", err)
	}
	if string(plain) != "jwt-one" {
		t.Errorf("plaintext=%q want %q", plain, "jwt-one")
	}
	plain, err = f.sealer.Open(ctx, "usr_test02", "acct_test02", secrets.KindResySessionJWT)
	if err != nil {
		t.Fatalf("Open acct2: %v", err)
	}
	if string(plain) != "jwt-two" {
		t.Errorf("plaintext=%q want %q", plain, "jwt-two")
	}
}

func TestMigratePlaintextSessionsIdempotent(t *testing.T) {
	t.Parallel()
	f := newMigrateFixture(t)
	insertSession(t, f.db, testAccountID, "jwt-one")

	first, err := secrets.MigratePlaintextSessions(context.Background(), f.db, f.sealer, f.log, false)
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if first.Migrated != 1 {
		t.Fatalf("first Migrated=%d want 1", first.Migrated)
	}

	// Second run finds zero work — jwt is already empty so the
	// SELECT returns no rows. Even if we forcibly re-insert plaintext
	// for the same (user, account), the row in `secrets` already
	// exists and the migrate path should skip it.
	second, err := secrets.MigratePlaintextSessions(context.Background(), f.db, f.sealer, f.log, false)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if second.Migrated != 0 || second.Skipped != 0 {
		t.Fatalf("second run Migrated=%d Skipped=%d want both 0", second.Migrated, second.Skipped)
	}

	// Re-insert a plaintext jwt for the same account (simulating an
	// operator manually editing the DB between runs). The third
	// migrate run finds the existing secrets row and skips, but still
	// re-zeros the plaintext.
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE sessions SET jwt = ? WHERE account_id = ?`,
		"jwt-resurrected", testAccountID,
	); err != nil {
		t.Fatalf("resurrect plaintext: %v", err)
	}
	third, err := secrets.MigratePlaintextSessions(context.Background(), f.db, f.sealer, f.log, false)
	if err != nil {
		t.Fatalf("third migrate: %v", err)
	}
	if third.Migrated != 0 {
		t.Fatalf("third Migrated=%d want 0", third.Migrated)
	}
	if third.Skipped != 1 {
		t.Fatalf("third Skipped=%d want 1", third.Skipped)
	}
	if got := readJWT(t, f.db, testAccountID); got != "" {
		t.Errorf("jwt not re-zeroed: %q", got)
	}
}

func TestMigratePlaintextSessionsDryRun(t *testing.T) {
	t.Parallel()
	f := newMigrateFixture(t)
	insertSession(t, f.db, testAccountID, "jwt-one")
	insertSession(t, f.db, f.acct2, "jwt-two")

	report, err := secrets.MigratePlaintextSessions(context.Background(), f.db, f.sealer, f.log, true)
	if err != nil {
		t.Fatalf("MigratePlaintextSessions dry-run: %v", err)
	}
	if report.Migrated != 2 {
		t.Fatalf("dry-run Migrated=%d want 2", report.Migrated)
	}

	// DB is untouched: plaintext still present, no secrets rows.
	if got := readJWT(t, f.db, testAccountID); got != "jwt-one" {
		t.Errorf("dry-run mutated jwt: %q", got)
	}
	var n int
	if err := f.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM secrets`,
	).Scan(&n); err != nil {
		t.Fatalf("count secrets: %v", err)
	}
	if n != 0 {
		t.Errorf("dry-run wrote %d secrets rows, want 0", n)
	}
}

func TestMigratePlaintextSessionsUnboundAccountSkips(t *testing.T) {
	t.Parallel()
	f := newMigrateFixture(t)
	// Insert an unbound account (user_id NULL) — simulates a v1
	// session that hasn't been adopted by M1-16 yet.
	const orphan = "acct_orphan"
	if _, err := f.db.ExecContext(context.Background(), `
		INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
		VALUES (?, NULL, 'orphan@example.com', 'orphan', strftime('%s','now')*1000)`,
		orphan,
	); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	insertSession(t, f.db, orphan, "jwt-orphan")
	insertSession(t, f.db, testAccountID, "jwt-bound")

	report, err := secrets.MigratePlaintextSessions(context.Background(), f.db, f.sealer, f.log, false)
	if err != nil {
		t.Fatalf("MigratePlaintextSessions: %v", err)
	}
	if report.Migrated != 1 {
		t.Fatalf("Migrated=%d want 1", report.Migrated)
	}
	if report.Skipped != 1 {
		t.Fatalf("Skipped=%d want 1", report.Skipped)
	}
	// Orphan session plaintext is untouched (we can't seal it without
	// a user_id). Bound session is zeroed.
	if got := readJWT(t, f.db, orphan); got != "jwt-orphan" {
		t.Errorf("orphan jwt mutated: %q", got)
	}
	if got := readJWT(t, f.db, testAccountID); got != "" {
		t.Errorf("bound jwt not zeroed: %q", got)
	}
	if !bytes.Contains(f.logBuf.Bytes(), []byte("secrets.migrate_unbound_account")) {
		t.Errorf("expected WARN log naming unbound account, got: %s", f.logBuf.String())
	}
}

func TestMigratePlaintextSessionsRollsBackOnFailure(t *testing.T) {
	t.Parallel()
	f := newMigrateFixture(t)
	insertSession(t, f.db, testAccountID, "jwt-good")
	insertSession(t, f.db, f.acct2, "jwt-two")

	// Force a mid-transaction failure on the second row: rebind
	// acct2.user_id to a user id that does not exist in `users`. The
	// migrate will SELECT a valid user_id from accounts (LEFT JOIN)
	// but the eventual INSERT into `secrets` violates the
	// secrets.user_id FOREIGN KEY and SQLite aborts the transaction.
	// We use `defer_foreign_keys` so the setup UPDATE (which also
	// violates the FK) can land; the constraint is then re-checked
	// at the next commit / per-statement boundary inside the
	// migration's own transaction.
	ctx := context.Background()
	// Introduce a dangling user_id reference on acct2. SQLite's PRAGMA
	// foreign_keys is per-connection and database/sql may hand the
	// UPDATE a conn with FK=OFF (modernc's default for fresh conns);
	// we don't bother forcing it — the migration's own users-existence
	// pre-check is what catches the bad row and rolls the tx back.
	// Pin the pool to one conn so the UPDATE definitely lands.
	f.db.SetMaxOpenConns(1)
	if _, err := f.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable fk: %v", err)
	}
	if _, err := f.db.ExecContext(ctx,
		`UPDATE accounts SET user_id = 'usr_ghost' WHERE id = ?`, f.acct2,
	); err != nil {
		t.Fatalf("rebind acct2: %v", err)
	}

	_, migrateErr := secrets.MigratePlaintextSessions(ctx, f.db, f.sealer, f.log, false)
	if migrateErr == nil {
		t.Fatal("expected MigratePlaintextSessions to fail on FK violation")
	}

	// Despite the failure, the first session's plaintext must be
	// untouched (the whole tx rolled back).
	if got := readJWT(t, f.db, testAccountID); got != "jwt-good" {
		t.Errorf("first session jwt mutated despite rollback: %q", got)
	}
	// No secrets rows landed.
	var n int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets`).Scan(&n); err != nil {
		t.Fatalf("count secrets: %v", err)
	}
	if n != 0 {
		t.Errorf("partial commit: %d secrets rows present after rollback", n)
	}
}

func TestMigratePlaintextSessionsEmptyIsNoOp(t *testing.T) {
	t.Parallel()
	f := newMigrateFixture(t)
	// No sessions, no secrets — a fresh DB.
	report, err := secrets.MigratePlaintextSessions(context.Background(), f.db, f.sealer, f.log, false)
	if err != nil {
		t.Fatalf("MigratePlaintextSessions: %v", err)
	}
	if report.Migrated != 0 || report.Skipped != 0 {
		t.Fatalf("empty DB: Migrated=%d Skipped=%d want 0/0", report.Migrated, report.Skipped)
	}
}
