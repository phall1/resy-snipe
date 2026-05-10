package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"resy-snipe/internal/domain"
)

// AccountRow is the read projection of one v2 `accounts` row. Mirrors
// the columns declared in migration 0002_v2_multi_user.sql §accounts.
// The Service layer surfaces these as service.Account values; the
// adapter strips display-only fields it does not need.
//
// Cross-tenant safety: every read path filters by user_id. Per
// docs/v2/design/multi-user.md §tenancy-enforcement, the column is
// load-bearing — even an admin caller goes through ListUsers for
// cross-tenant listing, never through these account paths.
type AccountRow struct {
	ID          domain.AccountID
	UserID      domain.UserID
	ResyEmail   string
	DisplayName string
	CreatedAt   time.Time
	DisabledAt  *time.Time
}

// ListAccountsForUser returns every accounts row bound to userID.
// Ordering is created_at ascending so the operator's first-seeded
// account renders first.
//
// Cross-tenant safety: the WHERE clause filters on user_id; a wrong
// userID returns an empty slice, never another tenant's rows.
func ListAccountsForUser(ctx context.Context, db *sql.DB, userID domain.UserID) ([]AccountRow, error) {
	if db == nil {
		return nil, errors.New("ListAccountsForUser: nil db")
	}
	if userID == "" {
		return nil, errors.New("ListAccountsForUser: userID is required")
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, user_id, resy_email, display_name, created_at, disabled_at
		FROM accounts
		WHERE user_id = ?
		ORDER BY created_at, id`,
		string(userID))
	if err != nil {
		return nil, fmt.Errorf("ListAccountsForUser: %w", err)
	}
	defer rows.Close()

	out := []AccountRow{}
	for rows.Next() {
		ar, scanErr := scanAccountRow(rowScanner{rows})
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, ar)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListAccountsForUser iterate: %w", err)
	}
	return out, nil
}

// GetAccountByEmail returns the (userID, email) account row. Wrong
// userID surfaces as ErrNotFound (same fold-existence-and-ownership
// rule as GetQuest).
func GetAccountByEmail(ctx context.Context, db *sql.DB, userID domain.UserID, email string) (AccountRow, error) {
	if db == nil {
		return AccountRow{}, errors.New("GetAccountByEmail: nil db")
	}
	if userID == "" || email == "" {
		return AccountRow{}, errors.New("GetAccountByEmail: userID and email are required")
	}
	row := db.QueryRowContext(ctx, `
		SELECT id, user_id, resy_email, display_name, created_at, disabled_at
		FROM accounts
		WHERE user_id = ? AND resy_email = ?`,
		string(userID), email)
	return scanAccountRow(row)
}

// BindLegacyAccountToUser claims the legacy accounts row with the
// given resy_email (carried over by migration 0002 with user_id IS
// NULL, or freshly minted by the v1 UpsertSession bridge) for
// userID, returning the account id.
//
// If a row with (user_id = userID, resy_email = email) already
// exists, it is returned as-is — the operation is idempotent so a
// re-Login of the same user against the same Resy account does not
// fork a second accounts row.
//
// This is the M1-10 hand-off seam between the resy adapter's v1
// session storage (which only knows the Resy email) and the v2
// tenancy model (which keys accounts by userID). M2 will replace it
// with a sealed-secrets-aware path that writes directly to (userID,
// resy_email, sealed_password).
func BindLegacyAccountToUser(ctx context.Context, db *sql.DB, userID domain.UserID, email string) (domain.AccountID, error) {
	if db == nil {
		return "", errors.New("BindLegacyAccountToUser: nil db")
	}
	if userID == "" || email == "" {
		return "", errors.New("BindLegacyAccountToUser: userID and email are required")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("BindLegacyAccountToUser: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Path 1: already-bound row exists.
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE user_id = ? AND resy_email = ?`,
		string(userID), email,
	).Scan(&existing)
	switch {
	case err == nil:
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("BindLegacyAccountToUser: commit: %w", err)
		}
		return domain.AccountID(existing), nil
	case errors.Is(err, sql.ErrNoRows):
		// fall through
	default:
		return "", fmt.Errorf("BindLegacyAccountToUser: lookup bound: %w", err)
	}

	// Path 2: legacy row with NULL user_id exists — claim it.
	res, err := tx.ExecContext(ctx,
		`UPDATE accounts SET user_id = ? WHERE user_id IS NULL AND resy_email = ?`,
		string(userID), email,
	)
	if err != nil {
		return "", fmt.Errorf("BindLegacyAccountToUser: claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("BindLegacyAccountToUser: rows: %w", err)
	}
	if n == 0 {
		// No legacy row either; nothing to bind. The caller is
		// responsible for ensuring a session/account exists upstream.
		return "", fmt.Errorf("account for %s: %w", email, ErrNotFound)
	}

	// Recover the now-bound row's id.
	var bound string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE user_id = ? AND resy_email = ?`,
		string(userID), email,
	).Scan(&bound); err != nil {
		return "", fmt.Errorf("BindLegacyAccountToUser: lookup claimed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("BindLegacyAccountToUser: commit: %w", err)
	}
	return domain.AccountID(bound), nil
}

// scanAccountRow decodes one accounts row. Shared between the two
// readers so the column order is in exactly one place.
func scanAccountRow(s scanner) (AccountRow, error) {
	var (
		id, userID, resyEmail, displayName string
		createdAt                          int64
		disabledAt                         sql.NullInt64
	)
	if err := s.Scan(&id, &userID, &resyEmail, &displayName, &createdAt, &disabledAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AccountRow{}, fmt.Errorf("account: %w", ErrNotFound)
		}
		return AccountRow{}, fmt.Errorf("scan account: %w", err)
	}
	ar := AccountRow{
		ID:          domain.AccountID(id),
		UserID:      domain.UserID(userID),
		ResyEmail:   resyEmail,
		DisplayName: displayName,
		CreatedAt:   time.UnixMilli(createdAt).UTC(),
	}
	if disabledAt.Valid {
		t := time.UnixMilli(disabledAt.Int64).UTC()
		ar.DisabledAt = &t
	}
	return ar, nil
}
