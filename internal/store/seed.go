package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"resy-snipe/internal/domain"
)

// ErrSeedFailed is the sentinel wrapping any failure inside SeedOperator
// or BackfillOperatorAccounts. Callers branch on errors.Is(err,
// ErrSeedFailed) to distinguish a seed/backfill failure from an
// unrelated SQL error surfaced by neighboring code.
var ErrSeedFailed = errors.New("store: operator seed failed")

// SeedOpts configures SeedOperator. Email is required. PasswordHash is
// the already-hashed (argon2id PHC-encoded) operator password produced
// by the Service layer — SeedOperator itself never hashes a password
// (M1-10 owns that). UserID, if set, pins the operator's `usr_…` id;
// when empty, SeedOperator derives a stable id from Email so re-runs on
// the same DB produce the same row.
type SeedOpts struct {
	// Email is the operator's login email. Must be non-empty.
	Email string

	// PasswordHash is the argon2id PHC-encoded password produced by
	// the Service layer. May be empty when the operator has not yet
	// chosen a password — in that case bearer tokens (M1-10) are the
	// only path to authenticate. The DDL stores this as a NOT NULL
	// BLOB, so an empty PasswordHash is persisted as a zero-length
	// blob (x'').
	PasswordHash []byte

	// UserID optionally pins the operator's usr_… id. When empty,
	// SeedOperator derives a deterministic id from Email so re-runs
	// against the same DB are idempotent without relying on UPSERT
	// semantics.
	UserID string
}

// SeedOperator is the v1→v2 first-run step that mints the homelab's
// single operator row (role='admin', invited_by IS NULL) in the v2
// `users` table created by migration 0002_v2_multi_user.sql. It is
// idempotent: re-running with the same Email is a no-op and returns the
// existing operator's UserID.
//
// This is *only* for the first-run operator. Subsequent users join via
// Service.InviteUser (M1-10) — see ADR-0011.
//
// Companion step: BackfillOperatorAccounts (below), which binds every
// v1-imported `accounts` row (carried forward by 0002 with user_id
// NULL) to the operator. The two together complete the migration plan
// in docs/v2/design/multi-user.md §migration-plan steps 4–5.
//
// Errors are wrapped with ErrSeedFailed so the caller can branch on
// errors.Is(err, ErrSeedFailed).
func SeedOperator(ctx context.Context, db *sql.DB, opts SeedOpts) (domain.UserID, error) {
	if db == nil {
		return "", fmt.Errorf("%w: nil db", ErrSeedFailed)
	}
	email := strings.TrimSpace(opts.Email)
	if email == "" {
		return "", fmt.Errorf("%w: email is required", ErrSeedFailed)
	}

	userID := strings.TrimSpace(opts.UserID)
	if userID == "" {
		userID = deriveOperatorID(email)
	}

	// password_hash is declared NOT NULL BLOB; an empty hash maps to
	// a zero-length blob, which is legal under the NOT NULL constraint
	// (it's not NULL, just empty).
	hash := opts.PasswordHash
	if hash == nil {
		hash = []byte{}
	}

	// INSERT OR IGNORE handles two idempotence paths:
	//   1. Re-running with the same UserID — the PRIMARY KEY on
	//      users.id rejects the duplicate, and we fall through.
	//   2. Re-running with a different UserID but same Email — the
	//      UNIQUE constraint on users.email rejects it.
	// We follow with a SELECT (by email, which is UNIQUE) to recover
	// whichever id ended up persisted (the existing one on a no-op,
	// the freshly-minted one on first run).
	//
	// The partial unique index idx_users_one_operator (invited_by IS
	// NULL) is the third safety belt: if somebody managed to insert a
	// second operator row in between, that INSERT would fail outright,
	// not silently no-op via the IGNORE.
	// created_at is computed by SQLite (strftime) so this package stays
	// inside Law 7 — Go-side wall-clock reads live in internal/clock.
	// Schema-default style, matches the rest of the migrations.
	if _, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES (?, ?, ?, 'admin', NULL, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`,
		userID, email, hash,
	); err != nil {
		return "", fmt.Errorf("%w: insert operator: %w", ErrSeedFailed, err)
	}

	// Recover the persisted id by email (UNIQUE). This makes the
	// return value correct even when the caller's UserID differs from
	// a previously-seeded one.
	var got string
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE email = ?`, email,
	).Scan(&got); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Shouldn't happen — INSERT OR IGNORE either inserted
			// (row present) or noop'd (row already present). If we
			// land here something else deleted the row mid-flight.
			return "", fmt.Errorf("%w: operator row missing after insert", ErrSeedFailed)
		}
		return "", fmt.Errorf("%w: lookup operator: %w", ErrSeedFailed, err)
	}
	return domain.UserID(got), nil
}

// BackfillOperatorAccounts binds every v1-imported `accounts` row
// (those left with user_id IS NULL by migration 0002) to the supplied
// operator. Returns the number of rows updated. Idempotent — a second
// call returns 0.
//
// Spec: docs/v2/design/multi-user.md §migration-plan step 5. This
// completes the M1-14 migration, which intentionally left
// accounts.user_id nullable pending this backfill.
//
// After this runs the Service layer enforces the NOT NULL invariant in
// code (every account-bearing Store method requires a userID); the
// column stays nominally nullable in the schema so the migration can
// land cleanly on a DB that has no operator yet.
func BackfillOperatorAccounts(ctx context.Context, db *sql.DB, operatorID domain.UserID) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("%w: nil db", ErrSeedFailed)
	}
	if strings.TrimSpace(string(operatorID)) == "" {
		return 0, fmt.Errorf("%w: operatorID is required", ErrSeedFailed)
	}

	res, err := db.ExecContext(ctx,
		`UPDATE accounts SET user_id = ? WHERE user_id IS NULL`,
		string(operatorID))
	if err != nil {
		return 0, fmt.Errorf("%w: update accounts: %w", ErrSeedFailed, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%w: rows affected: %w", ErrSeedFailed, err)
	}
	return int(n), nil
}

// deriveOperatorID returns a stable usr_… id derived from email so
// SeedOperator is idempotent across re-runs even when the caller never
// supplies a UserID. The hash is truncated to 8 lower-case hex chars,
// matching the on-disk id width seen elsewhere (e.g. acct_xxxxxxxx) and
// keeping the row visually distinct from the random ids the Service
// layer mints for invited users.
func deriveOperatorID(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "usr_" + hex.EncodeToString(sum[:4])
}
