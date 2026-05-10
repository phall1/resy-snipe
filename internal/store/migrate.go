package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is a single numbered SQL file embedded into the binary.
type migration struct {
	version int
	name    string
	body    string
}

// Migrate brings the database up to the latest schema. It is
// idempotent: re-running on an up-to-date DB is a no-op. Each
// migration is applied inside its own transaction; partially-applied
// migrations are impossible because failure rolls back including the
// schema_migrations write.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		)`); err != nil {
		return fmt.Errorf("ensuring schema_migrations: %w", err)
	}

	applied, err := readApplied(ctx, db)
	if err != nil {
		return err
	}

	migs, err := loadMigrations()
	if err != nil {
		return err
	}

	// Downgrade refusal: an applied version higher than any embedded
	// migration means the DB was last touched by a newer binary. The
	// upgrade contract (ADR-0006, daemon §12.2) is forward-only.
	var maxEmbedded int
	for _, m := range migs {
		if m.version > maxEmbedded {
			maxEmbedded = m.version
		}
	}
	for v := range applied {
		if v > maxEmbedded {
			return fmt.Errorf("%w: schema v%d is newer than this binary's max v%d",
				ErrSchemaNewer, v, maxEmbedded)
		}
	}

	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("applying %04d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

// ErrSchemaNewer is returned by Migrate when the database reports a
// schema_migrations.version higher than any embedded migration in
// this binary. The caller (daemon boot) maps it to exit code 3.
var ErrSchemaNewer = errors.New("store: schema is newer than this binary; downgrade refused")

// EmbeddedMaxVersion returns the highest migration version this
// binary knows how to apply. Exposed for the daemon boot banner so
// the operator can see what "schema vN" the binary supports without
// running migrations first.
func EmbeddedMaxVersion() (int, error) {
	migs, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	var maxV int
	for _, m := range migs {
		if m.version > maxV {
			maxV = m.version
		}
	}
	return maxV, nil
}

// AppliedMaxVersion returns the highest schema_migrations.version
// currently recorded in db. Used by the boot banner.
func AppliedMaxVersion(ctx context.Context, db *sql.DB) (int, error) {
	applied, err := readApplied(ctx, db)
	if err != nil {
		return 0, err
	}
	var maxV int
	for v := range applied {
		if v > maxV {
			maxV = v
		}
	}
	return maxV, nil
}

func readApplied(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scanning version: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating schema_migrations: %w", err)
	}
	return applied, nil
}

func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return fmt.Errorf("executing body: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
		m.version, m.name,
	); err != nil {
		return fmt.Errorf("recording version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// loadMigrations enumerates the embedded migrations directory and
// returns them in version order. Filenames must match
// "<NNNN>_<name>.sql"; any deviation is a build-time error rather
// than a runtime surprise.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading embedded migrations: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("no embedded migrations found")
	}

	var migs []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		base := strings.TrimSuffix(name, ".sql")
		idx := strings.Index(base, "_")
		if idx <= 0 {
			return nil, fmt.Errorf("migration %q must be NNNN_name.sql", name)
		}
		v, err := strconv.Atoi(base[:idx])
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version: %w", name, err)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("duplicate version %d: %q and %q", v, prev, name)
		}
		seen[v] = name

		body, err := fs.ReadFile(migrationFS, "migrations/"+name)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		migs = append(migs, migration{
			version: v,
			name:    base[idx+1:],
			body:    string(body),
		})
	}
	if len(migs) == 0 {
		return nil, errors.New("no .sql migrations matched naming convention")
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	return migs, nil
}
