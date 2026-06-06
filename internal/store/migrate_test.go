package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
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
		// v1 (0001_initial)
		"events",
		"observed_release_times",
		"schema_migrations",
		"sessions",
		"snipes",
		"venues",
		// v2 (0002_v2_multi_user)
		"users", // re-created as homelab-tenant table by 0002
		"accounts",
		"tokens",
		"invites",
		"secrets",
		"secrets_meta",
		"quests",
		"intents",
		"runs",
		"quest_events",
		"audit_events",
		"venues_cache",
		"name_search_cache",
		"subscriptions",
	}
	got := listTables(t, db)
	for _, name := range want {
		if !contains(got, name) {
			t.Errorf("missing table %q (got %v)", name, got)
		}
	}
	// The v1->v2 rebind drops both shim tables.
	for _, name := range []string{"accounts_old", "sessions_old"} {
		if contains(got, name) {
			t.Errorf("table %q should have been dropped (got %v)", name, got)
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
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	want := []row{
		{version: 1, name: "initial"},
		{version: 2, name: "v2_multi_user"},
		{version: 3, name: "idempotency"},
		{version: 4, name: "tokens_reshape"},
		{version: 5, name: "subscriptions"},
	}
	if len(got) != len(want) {
		t.Fatalf("schema_migrations rows: %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("schema_migrations[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	// Already migrated once via openMigrated. Run twice more.
	for i := range 2 {
		if err := store.Migrate(context.Background(), db); err != nil {
			t.Fatalf("Migrate run %d: %v", i+2, err)
		}
	}
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("schema_migrations row count = %d, want 5 after triple-Migrate", n)
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
		INSERT INTO sessions (account_id, provider, jwt, exp)
		VALUES ('phantom_account', 'resy', 'jwt', '2026-12-31T00:00:00Z')`)
	if err == nil {
		t.Fatal("expected FK violation when inserting session for missing account")
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

// V2 migration (0002_v2_multi_user) --------------------------------

// TestMigrateV2PreservesV1Sessions exercises the v1->v2 data-
// preservation case from docs/v2/design/multi-user.md §migration-plan:
// a v1 row written into `users` + `sessions` before 0002 is reachable
// after 0002 in `accounts` + `sessions(account_id)` with all column
// values intact.
//
// We do this by applying 0001 manually, marking it applied in
// schema_migrations, seeding v1 rows, and then calling store.Migrate
// — which sees 0001 as applied and runs only 0002.
func TestMigrateV2PreservesV1Sessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v1.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 1. Apply 0001 inline (verbatim from internal/store/migrations
	// /0001_initial.sql), and record it as applied so Migrate skips
	// it.
	if err := applyV1Schema(ctx, db); err != nil {
		t.Fatalf("seed v1 schema: %v", err)
	}

	// 2. v1 fixture: two Resy logins, one session each.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, created_at) VALUES (?, ?), (?, ?)`,
		"phall@example.com", "2026-04-01T10:00:00.000Z",
		"james@example.com", "2026-04-02T11:00:00.000Z",
	); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (user_id, provider, jwt, exp, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?)`,
		"phall@example.com", "resy", "phall.jwt", "2026-12-31T00:00:00Z",
		"2026-04-01T10:00:00.000Z", "2026-04-01T10:00:00.000Z",
		"james@example.com", "resy", "james.jwt", "2026-12-31T00:00:00Z",
		"2026-04-02T11:00:00.000Z", "2026-04-02T11:00:00.000Z",
	); err != nil {
		t.Fatalf("seed sessions: %v", err)
	}

	// 3. Apply 0002.
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate (0002): %v", err)
	}

	// 4. The v1 `users` table no longer exists in its v1 shape — it's
	// been recreated by 0002 as the homelab-tenant table. The data
	// is now in `accounts`. Both v1 rows must be present, with
	// resy_email preserved and user_id NULL (M1-16 backfills).
	want := map[string]bool{
		"phall@example.com": false,
		"james@example.com": false,
	}
	rows, err := db.QueryContext(ctx,
		`SELECT resy_email, user_id, display_name FROM accounts ORDER BY resy_email`)
	if err != nil {
		t.Fatalf("select accounts: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var email, displayName string
		var userID sql.NullString
		if err := rows.Scan(&email, &userID, &displayName); err != nil {
			t.Fatal(err)
		}
		if userID.Valid {
			t.Errorf("account %q: user_id should be NULL pending M1-16 backfill, got %q",
				email, userID.String)
		}
		if displayName != "imported" {
			t.Errorf("account %q: display_name = %q, want imported", email, displayName)
		}
		want[email] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	for email, found := range want {
		if !found {
			t.Errorf("missing account row for %q", email)
		}
	}

	// 5. sessions must be rebound to account_id and JWT preserved.
	got := map[string]string{}
	rows2, err := db.QueryContext(ctx, `
		SELECT a.resy_email, s.jwt
		FROM sessions s
		JOIN accounts a ON a.id = s.account_id`)
	if err != nil {
		t.Fatalf("select sessions: %v", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var email, jwt string
		if err := rows2.Scan(&email, &jwt); err != nil {
			t.Fatal(err)
		}
		got[email] = jwt
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("rows2.Err: %v", err)
	}
	if got["phall@example.com"] != "phall.jwt" {
		t.Errorf("phall jwt = %q, want phall.jwt", got["phall@example.com"])
	}
	if got["james@example.com"] != "james.jwt" {
		t.Errorf("james jwt = %q, want james.jwt", got["james@example.com"])
	}

	// 6. Shim tables are gone.
	for _, shim := range []string{"accounts_old", "sessions_old"} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`,
			shim).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("shim table %q should be dropped", shim)
		}
	}
}

// TestMigrateV2OperatorUniqueIndex covers the partial unique index
// idx_users_one_operator: at most one homelab user may have
// invited_by IS NULL (the operator). Design §users.detail.
func TestMigrateV2OperatorUniqueIndex(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	ctx := context.Background()

	// First operator row inserts cleanly.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES ('usr_op', 'op@example.com', x'00', 'admin', NULL, 0)`); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	// Second NULL-inviter must collide on the partial unique index.
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES ('usr_op2', 'op2@example.com', x'00', 'admin', NULL, 0)`)
	if err == nil {
		t.Fatal("expected second NULL-inviter user to violate idx_users_one_operator")
	}

	// A real invited user (invited_by NOT NULL) must still insert.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES ('usr_a', 'a@example.com', x'00', 'user', 'usr_op', 0)`); err != nil {
		t.Fatalf("insert invited user: %v", err)
	}
}

// TestMigrateV2ForeignKeys spot-checks that key v2 FKs are wired
// (and ON DELETE CASCADE / SET NULL where the design demands). We
// rely on PRAGMA foreign_key_list for shape and ON DELETE behavior
// for cascade semantics.
func TestMigrateV2ForeignKeys(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	ctx := context.Background()

	cases := []struct {
		table, col, refTable, refCol, onDelete string
	}{
		{"accounts", "user_id", "users", "id", "CASCADE"},
		{"tokens", "user_id", "users", "id", "CASCADE"},
		{"invites", "invited_by", "users", "id", "CASCADE"},
		{"secrets", "user_id", "users", "id", "CASCADE"},
		{"secrets", "account_id", "accounts", "id", "CASCADE"},
		{"quests", "user_id", "users", "id", "CASCADE"},
		{"quests", "account_id", "accounts", "id", "CASCADE"},
		{"intents", "quest_id", "quests", "id", "CASCADE"},
		{"intents", "user_id", "users", "id", "CASCADE"},
		{"runs", "quest_id", "quests", "id", "CASCADE"},
		{"runs", "intent_id", "intents", "id", "CASCADE"},
		{"quest_events", "quest_id", "quests", "id", "CASCADE"},
		{"audit_events", "user_id", "users", "id", "CASCADE"},
		{"audit_events", "target_user_id", "users", "id", "SET NULL"},
		{"subscriptions", "user_id", "users", "id", "CASCADE"},
		{"subscriptions", "fulfilled_by", "quests", "id", "SET NULL"},
	}
	for _, c := range cases {
		if !hasForeignKey(t, ctx, db, c.table, c.col, c.refTable, c.refCol, c.onDelete) {
			t.Errorf("missing FK %s.%s -> %s.%s ON DELETE %s",
				c.table, c.col, c.refTable, c.refCol, c.onDelete)
		}
	}
}

// applyV1Schema reproduces 0001_initial.sql line-for-line (the v1
// shape) so the v2-migration test can land a v1 fixture before
// store.Migrate runs only 0002. We also pre-populate
// schema_migrations with the 0001 row so Migrate skips it.
func applyV1Schema(ctx context.Context, db *sql.DB) error {
	const v1DDL = `
CREATE TABLE schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE TABLE users (
    id         TEXT PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE TABLE sessions (
    user_id    TEXT NOT NULL,
    provider   TEXT NOT NULL,
    jwt        TEXT NOT NULL,
    exp        TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (user_id, provider),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE TABLE venues (
    provider     TEXT NOT NULL,
    ref          TEXT NOT NULL,
    name         TEXT,
    tz           TEXT NOT NULL,
    last_seen_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (provider, ref)
);
CREATE TABLE snipes (
    id            TEXT PRIMARY KEY,
    intent_hash   TEXT NOT NULL,
    intent_json   TEXT NOT NULL,
    status        TEXT NOT NULL,
    scheduled_at  TEXT,
    result_json   TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_snipes_intent_hash ON snipes(intent_hash);
CREATE INDEX idx_snipes_status      ON snipes(status);
CREATE TABLE events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    snipe_id    TEXT NOT NULL,
    type        TEXT NOT NULL,
    at          TEXT NOT NULL,
    fields_json TEXT,
    FOREIGN KEY (snipe_id) REFERENCES snipes(id) ON DELETE CASCADE
);
CREATE INDEX idx_events_snipe_id ON events(snipe_id);
CREATE TABLE observed_release_times (
    provider            TEXT NOT NULL,
    venue_ref           TEXT NOT NULL,
    observed_local_time TEXT NOT NULL,
    days_offset         INTEGER NOT NULL,
    observed_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (provider, venue_ref)
);
INSERT INTO schema_migrations (version, name) VALUES (1, 'initial');
`
	_, err := db.ExecContext(ctx, v1DDL)
	return err
}

// hasForeignKey scans PRAGMA foreign_key_list(<table>) for a match.
// Returns true if the table has an FK col -> refTable.refCol with
// the requested ON DELETE behavior ("CASCADE", "SET NULL", ...).
func hasForeignKey(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	table, col, refTable, refCol, onDelete string,
) bool {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT "table", "from", "to", on_delete FROM pragma_foreign_key_list(?)`,
		table)
	if err != nil {
		t.Fatalf("pragma foreign_key_list(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var refT, from, to, onDel string
		if err := rows.Scan(&refT, &from, &to, &onDel); err != nil {
			t.Fatal(err)
		}
		if refT == refTable && from == col && to == refCol && onDel == onDelete {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return false
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
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return names
}

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
