package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"resy-snipe/internal/domain"
)

// TokenRow is the read projection of one tokens row. Mirrors the
// columns declared in migration 0004_tokens_reshape.sql; the Service
// layer surfaces these as service.Token values (with the hash stripped
// away — only IDs and metadata leave the daemon process boundary).
//
// Hash is the BLAKE2b-256 digest of the plaintext token. The plaintext
// itself is returned once at IssueToken time and never persisted; a
// lost token is revoked + reissued, never recovered.
type TokenRow struct {
	ID        string
	UserID    domain.UserID
	Hash      []byte
	Scopes    string // 'user' | 'operator'
	Label     string
	CreatedAt time.Time
	LastSeen  *time.Time
	RevokedAt *time.Time
}

// InsertToken persists a freshly-minted token row. Callers (the
// Service IssueToken verb) mint the ULID id, hash the plaintext via
// BLAKE2b, and stamp createdAt themselves — the store does not read
// the clock (Law 7).
//
// The hash uniqueness constraint is enforced by idx_tokens_hash; a
// duplicate is treated as a programming error (32 bytes of crypto/rand
// collided) and surfaces as a wrapped DB error.
func InsertToken(ctx context.Context, db *sql.DB, t TokenRow) error {
	if db == nil {
		return errors.New("InsertToken: nil db")
	}
	if t.ID == "" || t.UserID == "" || len(t.Hash) == 0 || t.Scopes == "" || t.Label == "" {
		return errors.New("InsertToken: id, user_id, hash, scopes, and label are required")
	}
	if t.Scopes != "user" && t.Scopes != "operator" {
		return fmt.Errorf("InsertToken: scopes must be 'user' or 'operator', got %q", t.Scopes)
	}
	if t.CreatedAt.IsZero() {
		return errors.New("InsertToken: createdAt is required (Law 7)")
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO tokens (id, user_id, hash, scopes, label, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, string(t.UserID), t.Hash, t.Scopes, t.Label, t.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("InsertToken: %w", err)
	}
	return nil
}

// FindTokenByHash returns the (non-revoked) token row whose hash
// matches. Used by the HTTP auth middleware to authenticate a bearer:
// BLAKE2b the presented plaintext, look it up here, set ctx accordingly.
//
// Revoked tokens surface as ErrNotFound — the middleware treats a
// revoked token identically to a never-issued one.
func FindTokenByHash(ctx context.Context, db *sql.DB, hash []byte) (TokenRow, error) {
	if db == nil {
		return TokenRow{}, errors.New("FindTokenByHash: nil db")
	}
	if len(hash) == 0 {
		return TokenRow{}, errors.New("FindTokenByHash: hash is required")
	}
	row := db.QueryRowContext(ctx, `
		SELECT id, user_id, hash, scopes, label, created_at, last_seen, revoked_at
		FROM tokens
		WHERE hash = ? AND revoked_at IS NULL`,
		hash)
	return scanTokenRow(row)
}

// ListTokensForUser returns every token row bound to userID, including
// revoked rows so the operator can audit. Ordering is created_at
// ascending so the first-issued token renders first.
//
// Cross-tenant safety: the WHERE clause filters on user_id; an
// operator listing across tenants goes through a separate path (not
// yet wired — the M2 surface is per-user only).
func ListTokensForUser(ctx context.Context, db *sql.DB, userID domain.UserID) ([]TokenRow, error) {
	if db == nil {
		return nil, errors.New("ListTokensForUser: nil db")
	}
	if userID == "" {
		return nil, errors.New("ListTokensForUser: userID is required")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, user_id, hash, scopes, label, created_at, last_seen, revoked_at
		FROM tokens
		WHERE user_id = ?
		ORDER BY created_at, id`,
		string(userID))
	if err != nil {
		return nil, fmt.Errorf("ListTokensForUser: %w", err)
	}
	defer rows.Close()

	out := []TokenRow{}
	for rows.Next() {
		tr, scanErr := scanTokenRow(rowScanner{rows})
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListTokensForUser iterate: %w", err)
	}
	return out, nil
}

// RevokeToken marks the (userID, tokenID) token as revoked by
// stamping revoked_at. Idempotent: revoking an already-revoked token
// is not an error (the UPDATE matches the row but the SET is a
// re-stamp, which the design treats as benign).
//
// Wrong-tenant calls produce ErrNotFound — an operator revoking
// across tenants goes through a separate operator-tier path.
func RevokeToken(ctx context.Context, db *sql.DB, userID domain.UserID, tokenID string, now time.Time) error {
	if db == nil {
		return errors.New("RevokeToken: nil db")
	}
	if userID == "" || tokenID == "" {
		return errors.New("RevokeToken: userID and tokenID are required")
	}
	if now.IsZero() {
		return errors.New("RevokeToken: now is required (Law 7)")
	}
	res, err := db.ExecContext(ctx, `
		UPDATE tokens
		SET revoked_at = ?
		WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		now.Unix(), tokenID, string(userID))
	if err != nil {
		return fmt.Errorf("RevokeToken: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("RevokeToken rows: %w", err)
	}
	if affected == 0 {
		// Either the row does not exist, belongs to another user, or
		// was already revoked. The first two collapse to ErrNotFound;
		// the third we surface as ErrNotFound as well because re-
		// revoking an already-dead credential is operationally a
		// no-op from the caller's perspective and the design has no
		// dedicated sentinel for it.
		return ErrNotFound
	}
	return nil
}

// TouchTokenSeen stamps last_seen on the token whose hash matches.
// Called by the auth middleware on successful authentication. The
// middleware coalesces calls — design/daemon.md §4.2 specifies "~1Hz"
// — so this is best-effort: a failure logs but does not fail the
// request.
//
// Returns no error if the hash does not match a live row — the caller
// just authenticated against this row, so a vanishing hash here means
// concurrent revocation; the next request will 401 cleanly.
func TouchTokenSeen(ctx context.Context, db *sql.DB, hash []byte, now time.Time) error {
	if db == nil {
		return errors.New("TouchTokenSeen: nil db")
	}
	if len(hash) == 0 {
		return errors.New("TouchTokenSeen: hash is required")
	}
	if now.IsZero() {
		return errors.New("TouchTokenSeen: now is required (Law 7)")
	}
	_, err := db.ExecContext(ctx, `
		UPDATE tokens
		SET last_seen = ?
		WHERE hash = ? AND revoked_at IS NULL`,
		now.Unix(), hash)
	if err != nil {
		return fmt.Errorf("TouchTokenSeen: %w", err)
	}
	return nil
}

// scanTokenRow decodes one tokens row. Shared between the QueryRow and
// QueryContext call sites via the scanner interface defined in
// sqlite.go.
func scanTokenRow(s scanner) (TokenRow, error) {
	var (
		t          TokenRow
		uid        string
		createdSec int64
		lastSeen   sql.NullInt64
		revokedAt  sql.NullInt64
	)
	err := s.Scan(&t.ID, &uid, &t.Hash, &t.Scopes, &t.Label, &createdSec, &lastSeen, &revokedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TokenRow{}, ErrNotFound
		}
		return TokenRow{}, fmt.Errorf("scanTokenRow: %w", err)
	}
	t.UserID = domain.UserID(uid)
	t.CreatedAt = time.Unix(createdSec, 0).UTC()
	if lastSeen.Valid {
		ts := time.Unix(lastSeen.Int64, 0).UTC()
		t.LastSeen = &ts
	}
	if revokedAt.Valid {
		ts := time.Unix(revokedAt.Int64, 0).UTC()
		t.RevokedAt = &ts
	}
	return t, nil
}
