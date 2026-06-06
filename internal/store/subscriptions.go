package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"resy-snipe/internal/domain"
)

// SubscriptionRow is the read/write projection of one subscriptions row.
// Mirrors the columns declared in migration 0005_subscriptions.sql.
type SubscriptionRow struct {
	ID             domain.SubscriptionID
	UserID         domain.UserID
	GoalJSON       string
	Status         domain.SubscriptionStatus
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExpiresAt      *time.Time
	FulfilledBy    *domain.QuestID
	CompromiseJSON string
	PollInterval   time.Duration
	NextPollAt     time.Time
}

// SubscriptionListFilter narrows the row set returned by ListSubscriptions.
// Empty fields are wildcards; Limit<=0 means "no limit".
type SubscriptionListFilter struct {
	Status []domain.SubscriptionStatus
	Limit  int
}

// CreateSubscription inserts one subscriptions row. Every column is provided
// by the caller; the function performs no validation beyond the schema
// constraints.
func CreateSubscription(ctx context.Context, db *sql.DB, row SubscriptionRow) error {
	if db == nil {
		return errors.New("CreateSubscription: nil db")
	}
	if row.ID == "" || row.UserID == "" {
		return errors.New("CreateSubscription: ID and UserID are required")
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (id, user_id, goal_json, status, created_at, updated_at, expires_at, fulfilled_by, compromise_json, poll_interval, next_poll_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(row.ID),
		string(row.UserID),
		row.GoalJSON,
		row.Status.String(),
		row.CreatedAt.UnixMilli(),
		row.UpdatedAt.UnixMilli(),
		nullableTimeMillis(row.ExpiresAt),
		nullableQuestID(row.FulfilledBy),
		nullableString(row.CompromiseJSON),
		int64(row.PollInterval.Seconds()),
		row.NextPollAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("CreateSubscription: %w", err)
	}
	return nil
}

// GetSubscription returns the row identified by (userID, subID). A row owned
// by a different user surfaces as ErrNotFound.
func GetSubscription(ctx context.Context, db *sql.DB, userID domain.UserID, subID domain.SubscriptionID) (SubscriptionRow, error) {
	if db == nil {
		return SubscriptionRow{}, errors.New("GetSubscription: nil db")
	}
	row := db.QueryRowContext(ctx, `
		SELECT id, user_id, goal_json, status, created_at, updated_at, expires_at, fulfilled_by, compromise_json, poll_interval, next_poll_at
		FROM subscriptions
		WHERE id = ? AND user_id = ?`,
		string(subID), string(userID))
	return scanSubscriptionRow(row)
}

// ListSubscriptions returns rows owned by userID, narrowed by filter.
// Ordering is created_at descending so the caller sees the most recent
// subscription first. Returns an empty slice (not error) if no rows match.
func ListSubscriptions(ctx context.Context, db *sql.DB, userID domain.UserID, filter SubscriptionListFilter) ([]SubscriptionRow, error) {
	if db == nil {
		return nil, errors.New("ListSubscriptions: nil db")
	}

	var (
		where []string
		args  []any
	)
	where = append(where, "user_id = ?")
	args = append(args, string(userID))

	if len(filter.Status) > 0 {
		seen := map[string]struct{}{}
		var enumVals []string
		for _, s := range filter.Status {
			v := s.String()
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			enumVals = append(enumVals, v)
		}
		ph := make([]string, len(enumVals))
		for i, v := range enumVals {
			ph[i] = "?"
			args = append(args, v)
		}
		where = append(where, "status IN ("+strings.Join(ph, ",")+")")
	}

	//nolint:gosec // G202: the WHERE fragments are const literals.
	q := `
		SELECT id, user_id, goal_json, status, created_at, updated_at, expires_at, fulfilled_by, compromise_json, poll_interval, next_poll_at
		FROM subscriptions
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY created_at DESC, id DESC`
	if filter.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListSubscriptions: %w", err)
	}
	defer rows.Close()

	out := []SubscriptionRow{}
	for rows.Next() {
		sr, scanErr := scanSubscriptionRow(rowScanner{rows})
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, sr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSubscriptions iterate: %w", err)
	}
	return out, nil
}

// UpdateSubscriptionStatus flips a subscription's status (and optionally
// fulfilled_by and next_poll_at) in place. The WHERE clause filters on
// user_id so a wrong-tenant call produces zero rows-affected, which
// surfaces as ErrNotFound.
func UpdateSubscriptionStatus(
	ctx context.Context,
	db *sql.DB,
	userID domain.UserID,
	subID domain.SubscriptionID,
	newStatus domain.SubscriptionStatus,
	fulfilledBy *domain.QuestID,
	nextPollAt *time.Time,
	updatedAt time.Time,
) error {
	if db == nil {
		return errors.New("UpdateSubscriptionStatus: nil db")
	}
	var nextPollMs sql.NullInt64
	if nextPollAt != nil {
		nextPollMs = sql.NullInt64{Int64: nextPollAt.UnixMilli(), Valid: true}
	}
	res, err := db.ExecContext(ctx, `
		UPDATE subscriptions
		SET status = ?, fulfilled_by = ?, next_poll_at = COALESCE(?, next_poll_at), updated_at = ?
		WHERE id = ? AND user_id = ?`,
		newStatus.String(),
		nullableQuestID(fulfilledBy),
		nextPollMs,
		updatedAt.UnixMilli(),
		string(subID),
		string(userID),
	)
	if err != nil {
		return fmt.Errorf("UpdateSubscriptionStatus: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("UpdateSubscriptionStatus rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("subscription %s: %w", subID, ErrNotFound)
	}
	return nil
}

// parseSubscriptionStatus maps a persisted status value to its domain enum.
func parseSubscriptionStatus(s string) (domain.SubscriptionStatus, error) {
	switch s {
	case "active":
		return domain.SubscriptionActive, nil
	case "paused":
		return domain.SubscriptionPaused, nil
	case "fulfilled":
		return domain.SubscriptionFulfilled, nil
	case "expired":
		return domain.SubscriptionExpired, nil
	case "cancelled":
		return domain.SubscriptionCancelled, nil
	default:
		return 0, fmt.Errorf("unknown subscription status %q", s)
	}
}

// scanSubscriptionRow decodes one subscriptions row. Shared between
// GetSubscription and ListSubscriptions so the column-order contract is in
// exactly one place.
func scanSubscriptionRow(s scanner) (SubscriptionRow, error) {
	var (
		id, userID, goalJSON, status          string
		createdAt, updatedAt, pollIntervalSec int64
		nextPollAt                            int64
		expiresAt                             sql.NullInt64
		fulfilledBy                           sql.NullString
		compromiseJSON                        sql.NullString
	)
	if err := s.Scan(&id, &userID, &goalJSON, &status, &createdAt, &updatedAt, &expiresAt, &fulfilledBy, &compromiseJSON, &pollIntervalSec, &nextPollAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SubscriptionRow{}, fmt.Errorf("subscription: %w", ErrNotFound)
		}
		return SubscriptionRow{}, fmt.Errorf("scan subscription: %w", err)
	}
	st, err := parseSubscriptionStatus(status)
	if err != nil {
		return SubscriptionRow{}, fmt.Errorf("subscription %s: %w", id, err)
	}
	sr := SubscriptionRow{
		ID:           domain.SubscriptionID(id),
		UserID:       domain.UserID(userID),
		GoalJSON:     goalJSON,
		Status:       st,
		CreatedAt:    time.UnixMilli(createdAt).UTC(),
		UpdatedAt:    time.UnixMilli(updatedAt).UTC(),
		PollInterval: time.Duration(pollIntervalSec) * time.Second,
		NextPollAt:   time.UnixMilli(nextPollAt).UTC(),
	}
	if expiresAt.Valid {
		t := time.UnixMilli(expiresAt.Int64).UTC()
		sr.ExpiresAt = &t
	}
	if fulfilledBy.Valid {
		qid := domain.QuestID(fulfilledBy.String)
		sr.FulfilledBy = &qid
	}
	if compromiseJSON.Valid {
		sr.CompromiseJSON = compromiseJSON.String
	}
	return sr, nil
}

// nullableTimeMillis returns nil for a nil/zero time, otherwise the Unix
// millisecond timestamp.
func nullableTimeMillis(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// nullableQuestID returns nil for a nil quest ID, otherwise the string value.
func nullableQuestID(q *domain.QuestID) any {
	if q == nil {
		return nil
	}
	return string(*q)
}

// nullableString returns nil for an empty string, otherwise the string value.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
