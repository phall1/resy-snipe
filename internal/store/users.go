package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"resy-snipe/internal/domain"
)

// UserRow is the read-side projection of one v2 users row. It mirrors
// the columns declared in migration 0002_v2_multi_user.sql §users. The
// row is intentionally bare (no password_hash, no token material) —
// the shape is for admin listings, not authentication.
//
// InvitedBy is nil for the operator (the bootstrap admin with
// invited_by IS NULL). DisabledAt is nil for active rows; a non-nil
// value marks a soft-deleted account.
type UserRow struct {
	ID         domain.UserID
	Email      string
	Role       string
	CreatedAt  time.Time
	InvitedBy  *domain.UserID
	DisabledAt *time.Time
}

// ListUsers returns every row from the v2 users table, ordered by
// created_at ascending (operator first, invitees by acceptance order).
//
// CROSS-TENANT ADMIN FUNCTION. This deliberately bypasses the
// per-user tenancy filter every Store interface method must apply: it
// is the admin "show me everyone" query the operator runs via
// `resy-snipe user list`. Production callers MUST gate the call on an
// operator-only authorization check (role='admin') — the Service
// layer's ListUsers will do this once M1-10 wires the real
// implementation; for now the CLI is the only caller and operator-
// only is enforced by the fact that the binary is run on the
// operator's host.
//
// Package-level (not on Store) deliberately: making it a Store method
// would require the tenant_check harness to allowlist it, and an
// interface method whose entire purpose is to skip tenancy filtering
// is exactly the footgun the harness exists to prevent. The function
// is a peer to GetVenueCache/UpsertVenueCache (also cross-tenant) and
// follows the same package-level pattern.
//
// The matching documentation entry lives in tenant_allowlist.go so
// the exemption is visible alongside the v1 bridge methods and the
// cache peers — even though the tenancy harness only inspects the
// Store interface, the allowlist file is where future contributors
// look to understand which surfaces sit outside the per-user filter.
//
// created_at and disabled_at are declared INTEGER unix millis in the
// 0002 migration, but SeedOperator writes ISO8601 text via strftime
// (SQLite's type-affinity rules permit this). ListUsers therefore
// reads each timestamp as a string and parses either format, so the
// admin listing works against rows produced by SeedOperator today and
// rows produced by the Service layer (which will use Go-side millis)
// tomorrow.
func ListUsers(ctx context.Context, db *sql.DB) ([]UserRow, error) {
	if db == nil {
		return nil, errors.New("ListUsers: nil db")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, email, role, created_at, invited_by, disabled_at
		FROM users
		ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("ListUsers: query: %w", err)
	}
	defer rows.Close()

	out := []UserRow{}
	for rows.Next() {
		var (
			id, email, role sql.NullString
			createdRaw      sql.NullString
			invitedByRaw    sql.NullString
			disabledRaw     sql.NullString
		)
		if err := rows.Scan(&id, &email, &role, &createdRaw, &invitedByRaw, &disabledRaw); err != nil {
			return nil, fmt.Errorf("ListUsers: scan: %w", err)
		}
		ur := UserRow{
			ID:    domain.UserID(id.String),
			Email: email.String,
			Role:  role.String,
		}
		ts, err := parseUserTimestamp(createdRaw.String)
		if err != nil {
			return nil, fmt.Errorf("ListUsers: parse created_at for %s: %w", id.String, err)
		}
		ur.CreatedAt = ts
		if invitedByRaw.Valid && invitedByRaw.String != "" {
			inv := domain.UserID(invitedByRaw.String)
			ur.InvitedBy = &inv
		}
		if disabledRaw.Valid && disabledRaw.String != "" {
			dt, err := parseUserTimestamp(disabledRaw.String)
			if err != nil {
				return nil, fmt.Errorf("ListUsers: parse disabled_at for %s: %w", id.String, err)
			}
			ur.DisabledAt = &dt
		}
		out = append(out, ur)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListUsers: iterate: %w", err)
	}
	return out, nil
}

// parseUserTimestamp decodes a users.created_at / users.disabled_at
// value into a time.Time. The column is declared INTEGER unix millis,
// but SeedOperator writes ISO8601 text via strftime (SQLite's type
// affinity is permissive). Try the integer form first because the
// Service layer (M1-10) will write millis; fall back to ISO8601 for
// rows minted by SeedOperator.
func parseUserTimestamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC(), nil
	}
	// SeedOperator's strftime('%Y-%m-%dT%H:%M:%fZ', 'now') format.
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", s); err == nil {
		return t, nil
	}
	// Also accept plain RFC3339 in case a later migration writes that.
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", s)
}
