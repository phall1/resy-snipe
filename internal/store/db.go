package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DefaultDBPath returns the default sqlite path for resy-snipe. The
// data directory is XDG-aware: $XDG_DATA_HOME if set, else
// ~/.local/share. The caller is responsible for ensuring the
// containing directory exists; Open creates it.
func DefaultDBPath() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving user home: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "resy-snipe", "db.sqlite"), nil
}

// Open opens (creating if necessary) the resy-snipe sqlite database
// at path. If path is empty, DefaultDBPath() is used. The returned
// *sql.DB has WAL journaling, foreign-key enforcement, a 5s busy
// timeout, and synchronous=NORMAL. The caller owns Close.
//
// modernc.org/sqlite is the driver — pure Go, no CGO. The driver name
// registered with database/sql is "sqlite" (not "sqlite3").
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		var err error
		path, err = DefaultDBPath()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating database directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite at %s: %w", path, err)
	}

	if err := configurePragmas(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	// Sanity-check the connection so an unwriteable file or corrupt
	// database surfaces here, not at first use.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging sqlite at %s: %w", path, err)
	}

	return db, nil
}

func configurePragmas(ctx context.Context, db *sql.DB) error {
	// PRAGMA journal_mode returns the new mode as a row; we use
	// QueryRow to consume it. The other PRAGMAs return no rows when
	// set, so Exec is fine.
	var journal string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&journal); err != nil {
		return fmt.Errorf("PRAGMA journal_mode: %w", err)
	}
	if journal != "wal" {
		return fmt.Errorf("PRAGMA journal_mode: got %q, want wal", journal)
	}

	for _, p := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA synchronous = NORMAL`,
	} {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}
