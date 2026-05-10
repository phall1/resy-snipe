package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"resy-snipe/internal/domain"
)

// SQLiteStore implements Store against a *sql.DB opened by Open(). The
// caller owns the *sql.DB; the store does not close it.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore constructs a SQLiteStore. It does not run migrations
// — call Migrate(ctx, db) before constructing the store.
func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db}
}

// DB returns the underlying *sql.DB. Tests use this to run setup or
// teardown SQL that doesn't fit the Store interface (seeding fixture
// rows, deleting a row to provoke a missing-row code path).
// Production code must go through the Store interface, not this.
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// Compile-time interface check.
var _ Store = (*SQLiteStore)(nil)

// timeFmt is the RFC-3339 nanosecond format. Using the same format
// everywhere makes timestamp columns comparable as TEXT (they sort
// lexicographically when written this way).
const timeFmt = time.RFC3339Nano

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFmt)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(timeFmt, s)
}

// Snipes ----------------------------------------------------------------

func (s *SQLiteStore) CreateSnipe(ctx context.Context, snipe *domain.SnipeState) error {
	intentJSON, err := MarshalIntent(snipe.Intent)
	if err != nil {
		return fmt.Errorf("CreateSnipe encode intent: %w", err)
	}
	resultJSON, err := MarshalResult(snipe.Result)
	if err != nil {
		return fmt.Errorf("CreateSnipe encode result: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO snipes (id, intent_hash, intent_json, status, scheduled_at, result_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(snipe.ID),
		string(snipe.Intent.Hash()),
		string(intentJSON),
		snipe.Status().String(),
		nullableTime(snipe.ScheduledAt),
		nullableBytes(resultJSON),
		formatTime(snipe.CreatedAt),
		formatTime(snipe.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("CreateSnipe insert: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetSnipe(ctx context.Context, id domain.SnipeID) (*domain.SnipeState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, intent_json, status, scheduled_at, result_json, created_at, updated_at
		FROM snipes WHERE id = ?`, string(id))
	return scanSnipeRow(row)
}

func (s *SQLiteStore) UpdateSnipe(ctx context.Context, snipe *domain.SnipeState) error {
	intentJSON, err := MarshalIntent(snipe.Intent)
	if err != nil {
		return fmt.Errorf("UpdateSnipe encode intent: %w", err)
	}
	resultJSON, err := MarshalResult(snipe.Result)
	if err != nil {
		return fmt.Errorf("UpdateSnipe encode result: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE snipes
		SET intent_hash=?, intent_json=?, status=?, scheduled_at=?, result_json=?, updated_at=?
		WHERE id=?`,
		string(snipe.Intent.Hash()),
		string(intentJSON),
		snipe.Status().String(),
		nullableTime(snipe.ScheduledAt),
		nullableBytes(resultJSON),
		formatTime(snipe.UpdatedAt),
		string(snipe.ID),
	)
	if err != nil {
		return fmt.Errorf("UpdateSnipe: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("UpdateSnipe rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("UpdateSnipe %s: %w", snipe.ID, ErrNotFound)
	}
	return nil
}

func (s *SQLiteStore) ListSnipesByStatus(ctx context.Context, status domain.Status) ([]*domain.SnipeState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, intent_json, status, scheduled_at, result_json, created_at, updated_at
		FROM snipes WHERE status = ? ORDER BY created_at ASC`, status.String())
	if err != nil {
		return nil, fmt.Errorf("ListSnipesByStatus: %w", err)
	}
	defer rows.Close()

	var out []*domain.SnipeState
	for rows.Next() {
		st, err := scanSnipeRow(rowScanner{rows})
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSnipesByStatus iterate: %w", err)
	}
	return out, nil
}

// scanner unifies *sql.Row and *sql.Rows for shared decode logic.
type scanner interface {
	Scan(dest ...any) error
}

type rowScanner struct{ r *sql.Rows }

func (r rowScanner) Scan(dest ...any) error { return r.r.Scan(dest...) }

func scanSnipeRow(s scanner) (*domain.SnipeState, error) {
	var (
		id          string
		intentJSON  string
		statusStr   string
		scheduledAt sql.NullString
		resultJSON  sql.NullString
		createdAt   string
		updatedAt   string
	)
	if err := s.Scan(&id, &intentJSON, &statusStr, &scheduledAt, &resultJSON, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("snipe: %w", ErrNotFound)
		}
		return nil, fmt.Errorf("scan snipe: %w", err)
	}

	intent, err := UnmarshalIntent([]byte(intentJSON))
	if err != nil {
		return nil, fmt.Errorf("decode intent for snipe %s: %w", id, err)
	}

	status, err := parseStatus(statusStr)
	if err != nil {
		return nil, fmt.Errorf("snipe %s: %w", id, err)
	}

	createdT, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("snipe %s created_at: %w", id, err)
	}
	updatedT, err := parseTime(updatedAt)
	if err != nil {
		return nil, fmt.Errorf("snipe %s updated_at: %w", id, err)
	}
	var schedT time.Time
	if scheduledAt.Valid {
		schedT, err = parseTime(scheduledAt.String)
		if err != nil {
			return nil, fmt.Errorf("snipe %s scheduled_at: %w", id, err)
		}
	}

	st := domain.NewSnipeState(domain.SnipeID(id), intent, createdT)
	st.UpdatedAt = updatedT
	st.ScheduledAt = schedT
	if resultJSON.Valid && resultJSON.String != "" {
		r, err := UnmarshalResult([]byte(resultJSON.String))
		if err != nil {
			return nil, fmt.Errorf("snipe %s result: %w", id, err)
		}
		st.Result = r
	}
	if err := forceStatus(st, status); err != nil {
		return nil, fmt.Errorf("snipe %s status %s: %w", id, status, err)
	}
	return st, nil
}

// forceStatus drives a fresh SnipeState (which always starts in
// Submitted) to the persisted status by walking the transition graph.
// We can't bypass Transition's guard from outside the package, but we
// can take the canonical path for any reachable status so the loaded
// state matches the database.
func forceStatus(s *domain.SnipeState, target domain.Status) error {
	if s.Status() == target {
		return nil
	}
	for _, path := range statusPaths(target) {
		st := s.Status()
		i := 0
		for i < len(path) && path[i] != st {
			i++
		}
		if i == len(path) {
			continue
		}
		ok := true
		for ; i < len(path); i++ {
			if path[i] == s.Status() {
				continue
			}
			if err := s.Transition(path[i]); err != nil {
				ok = false
				break
			}
		}
		if ok && s.Status() == target {
			return nil
		}
	}
	return fmt.Errorf("no transition path from %s to %s", s.Status(), target)
}

// statusPaths returns one or more known sequences that reach target
// from Submitted. Order matters — earlier paths are tried first.
func statusPaths(target domain.Status) [][]domain.Status {
	switch target {
	case domain.StatusSubmitted:
		return [][]domain.Status{{domain.StatusSubmitted}}
	case domain.StatusScheduled:
		return [][]domain.Status{{domain.StatusSubmitted, domain.StatusScheduled}}
	case domain.StatusDiscovering:
		return [][]domain.Status{
			{domain.StatusSubmitted, domain.StatusDiscovering},
			{domain.StatusSubmitted, domain.StatusScheduled, domain.StatusDiscovering},
		}
	case domain.StatusAwaiting:
		return [][]domain.Status{
			{domain.StatusSubmitted, domain.StatusScheduled, domain.StatusAwaiting},
			{domain.StatusSubmitted, domain.StatusDiscovering, domain.StatusAwaiting},
		}
	case domain.StatusFinding:
		return [][]domain.Status{
			{domain.StatusSubmitted, domain.StatusScheduled, domain.StatusAwaiting, domain.StatusFinding},
		}
	case domain.StatusBooking:
		return [][]domain.Status{
			{domain.StatusSubmitted, domain.StatusScheduled, domain.StatusAwaiting, domain.StatusFinding, domain.StatusBooking},
		}
	case domain.StatusBooked:
		return [][]domain.Status{
			{domain.StatusSubmitted, domain.StatusScheduled, domain.StatusAwaiting, domain.StatusFinding, domain.StatusBooking, domain.StatusBooked},
		}
	case domain.StatusFailed:
		return [][]domain.Status{{domain.StatusSubmitted, domain.StatusFailed}}
	case domain.StatusCanceled:
		return [][]domain.Status{{domain.StatusSubmitted, domain.StatusCanceled}}
	case domain.StatusExpired:
		return [][]domain.Status{
			{domain.StatusSubmitted, domain.StatusScheduled, domain.StatusExpired},
		}
	}
	return nil
}

func parseStatus(s string) (domain.Status, error) {
	for _, st := range domain.AllStatuses() {
		if st.String() == s {
			return st, nil
		}
	}
	return 0, fmt.Errorf("unknown status %q", s)
}

// TransitionSnipe atomically updates the snipe row and appends an event.
func (s *SQLiteStore) TransitionSnipe(ctx context.Context, snipe *domain.SnipeState, ev domain.Event) error {
	intentJSON, err := MarshalIntent(snipe.Intent)
	if err != nil {
		return fmt.Errorf("TransitionSnipe encode intent: %w", err)
	}
	resultJSON, err := MarshalResult(snipe.Result)
	if err != nil {
		return fmt.Errorf("TransitionSnipe encode result: %w", err)
	}
	attrsJSON, err := EncodeAttrs(ev.Attrs)
	if err != nil {
		return fmt.Errorf("TransitionSnipe encode attrs: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("TransitionSnipe begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE snipes
		SET intent_hash=?, intent_json=?, status=?, scheduled_at=?, result_json=?, updated_at=?
		WHERE id=?`,
		string(snipe.Intent.Hash()),
		string(intentJSON),
		snipe.Status().String(),
		nullableTime(snipe.ScheduledAt),
		nullableBytes(resultJSON),
		formatTime(snipe.UpdatedAt),
		string(snipe.ID),
	)
	if err != nil {
		return fmt.Errorf("TransitionSnipe update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("TransitionSnipe rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("TransitionSnipe %s: %w", snipe.ID, ErrNotFound)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO events (snipe_id, type, at, fields_json)
		VALUES (?, ?, ?, ?)`,
		string(snipe.ID),
		string(ev.Type),
		formatTime(ev.At),
		nullableBytes(attrsJSON),
	)
	if err != nil {
		return fmt.Errorf("TransitionSnipe append: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("TransitionSnipe commit: %w", err)
	}
	return nil
}

// Events ----------------------------------------------------------------

func (s *SQLiteStore) AppendEvent(ctx context.Context, snipeID domain.SnipeID, e domain.Event) error {
	attrsJSON, err := EncodeAttrs(e.Attrs)
	if err != nil {
		return fmt.Errorf("AppendEvent encode attrs: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO events (snipe_id, type, at, fields_json)
		VALUES (?, ?, ?, ?)`,
		string(snipeID),
		string(e.Type),
		formatTime(e.At),
		nullableBytes(attrsJSON),
	)
	if err != nil {
		return fmt.Errorf("AppendEvent: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListEventsBySnipe(ctx context.Context, snipeID domain.SnipeID) ([]domain.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT type, at, fields_json
		FROM events WHERE snipe_id = ? ORDER BY id ASC`, string(snipeID))
	if err != nil {
		return nil, fmt.Errorf("ListEventsBySnipe: %w", err)
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var (
			typeStr    string
			at         string
			fieldsJSON sql.NullString
		)
		if err := rows.Scan(&typeStr, &at, &fieldsJSON); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		atT, err := parseTime(at)
		if err != nil {
			return nil, fmt.Errorf("event at: %w", err)
		}
		var attrs []byte
		if fieldsJSON.Valid {
			attrs = []byte(fieldsJSON.String)
		}
		decoded, err := DecodeAttrs(attrs)
		if err != nil {
			return nil, fmt.Errorf("decode event attrs: %w", err)
		}
		out = append(out, domain.Event{
			Type:  domain.EventType(typeStr),
			At:    atT,
			Attrs: decoded,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListEventsBySnipe iterate: %w", err)
	}
	return out, nil
}

// Sessions --------------------------------------------------------------

func (s *SQLiteStore) UpsertSession(ctx context.Context, sess SessionRow) error {
	// v2 bridge: SessionRow.UserID is still a Resy email (the v1
	// domain.UserID), but sessions now FK to accounts(id), not
	// users(id) (migrations/0002_v2_multi_user.sql). We map
	// UserID -> accounts.resy_email -> accounts.id, ensuring-or-
	// creating an account row in the same transaction. The
	// account's user_id is left NULL — M1-16 (operator seeding +
	// data adoption) is what binds these v1-imported rows to a
	// homelab user.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("UpsertSession: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// INSERT OR IGNORE on the (user_id, resy_email) UNIQUE
	// constraint is idempotent for the (NULL, email) shape too,
	// because SQLite's UNIQUE treats NULLs as distinct — so we add
	// our own existence check first. Note: we cannot fold both
	// branches into ON CONFLICT because the conflict target column
	// (resy_email alone) is not unique.
	var acctID string
	row := tx.QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE user_id IS NULL AND resy_email = ?`,
		string(sess.UserID))
	switch err := row.Scan(&acctID); {
	case errors.Is(err, sql.ErrNoRows):
		acctID = "acct_legacy_" + string(sess.UserID)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
			VALUES (?, NULL, ?, 'legacy-v1', ?)`,
			acctID, string(sess.UserID), sess.CreatedAt.UnixMilli(),
		); err != nil {
			return fmt.Errorf("UpsertSession: ensure account: %w", err)
		}
	case err != nil:
		return fmt.Errorf("UpsertSession: lookup account: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (account_id, provider, jwt, exp, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, provider) DO UPDATE SET
			jwt = excluded.jwt,
			exp = excluded.exp,
			updated_at = excluded.updated_at`,
		acctID,
		string(sess.Provider),
		sess.JWT,
		formatTime(sess.ExpiresAt),
		formatTime(sess.CreatedAt),
		formatTime(sess.UpdatedAt),
	); err != nil {
		return fmt.Errorf("UpsertSession: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("UpsertSession: commit: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetSession(
	ctx context.Context,
	user domain.UserID,
	provider domain.ProviderID,
	now time.Time,
) (SessionRow, error) {
	// v2 bridge: sessions now FK to accounts(id); v1 domain.UserID
	// is the Resy email, which lives on accounts.resy_email. We
	// join to recover it. See UpsertSession comment for the rest of
	// the bridge.
	row := s.db.QueryRowContext(ctx, `
		SELECT a.resy_email, s.provider, s.jwt, s.exp, s.created_at, s.updated_at
		FROM sessions s
		JOIN accounts a ON a.id = s.account_id
		WHERE a.resy_email = ? AND s.provider = ?`,
		string(user), string(provider))

	var (
		uid, prov, jwt, exp, created, updated string
	)
	if err := row.Scan(&uid, &prov, &jwt, &exp, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionRow{}, fmt.Errorf("session %s/%s: %w", user, provider, ErrNotFound)
		}
		return SessionRow{}, fmt.Errorf("GetSession: %w", err)
	}

	expT, err := parseTime(exp)
	if err != nil {
		return SessionRow{}, fmt.Errorf("session exp: %w", err)
	}
	createdT, err := parseTime(created)
	if err != nil {
		return SessionRow{}, fmt.Errorf("session created_at: %w", err)
	}
	updatedT, err := parseTime(updated)
	if err != nil {
		return SessionRow{}, fmt.Errorf("session updated_at: %w", err)
	}

	out := SessionRow{
		UserID:    domain.UserID(uid),
		Provider:  domain.ProviderID(prov),
		JWT:       jwt,
		ExpiresAt: expT,
		CreatedAt: createdT,
		UpdatedAt: updatedT,
	}
	if !expT.IsZero() && !expT.After(now) {
		return out, fmt.Errorf("session %s/%s exp %s: %w",
			user, provider, expT.Format(time.RFC3339), ErrSessionExpired)
	}
	return out, nil
}

func (s *SQLiteStore) DeleteSession(ctx context.Context, user domain.UserID, provider domain.ProviderID) error {
	// v2 bridge: same UserID->resy_email indirection as GetSession.
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE provider = ? AND account_id IN (
			SELECT id FROM accounts WHERE resy_email = ?
		)`,
		string(provider), string(user))
	if err != nil {
		return fmt.Errorf("DeleteSession: %w", err)
	}
	return nil
}

// Venues ---------------------------------------------------------------

func (s *SQLiteStore) UpsertVenue(ctx context.Context, v domain.Venue) error {
	if err := v.Validate(); err != nil {
		return fmt.Errorf("UpsertVenue: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO venues (provider, ref, name, tz)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(provider, ref) DO UPDATE SET
			name = excluded.name,
			tz = excluded.tz,
			last_seen_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		string(v.Provider), v.Ref, v.Name, v.TZ.String())
	if err != nil {
		return fmt.Errorf("UpsertVenue: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetVenue(ctx context.Context, ref domain.VenueRef) (domain.Venue, error) {
	var (
		prov, refStr, name, tz string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT provider, ref, name, tz FROM venues WHERE provider = ? AND ref = ?`,
		string(ref.Provider), ref.Ref).
		Scan(&prov, &refStr, &name, &tz)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Venue{}, fmt.Errorf("venue %s: %w", ref, ErrNotFound)
		}
		return domain.Venue{}, fmt.Errorf("GetVenue: %w", err)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return domain.Venue{}, fmt.Errorf("venue %s tz %q: %w", ref, tz, err)
	}
	return domain.Venue{
		Provider: domain.ProviderID(prov),
		Ref:      refStr,
		Name:     name,
		TZ:       loc,
	}, nil
}

// Observed release times -----------------------------------------------

func (s *SQLiteStore) UpsertObserved(ctx context.Context, o ObservedRelease) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO observed_release_times (provider, venue_ref, observed_local_time, days_offset, observed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(provider, venue_ref) DO UPDATE SET
			observed_local_time = excluded.observed_local_time,
			days_offset         = excluded.days_offset,
			observed_at         = excluded.observed_at`,
		string(o.Venue.Provider), o.Venue.Ref,
		o.ObservedLocalTime.String(), o.DaysOffset,
		formatTime(o.ObservedAt))
	if err != nil {
		return fmt.Errorf("UpsertObserved: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetObserved(ctx context.Context, ref domain.VenueRef) (ObservedRelease, error) {
	var (
		prov, venueRef, localTime, observedAt string
		daysOffset                            int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT provider, venue_ref, observed_local_time, days_offset, observed_at
		FROM observed_release_times WHERE provider = ? AND venue_ref = ?`,
		string(ref.Provider), ref.Ref).
		Scan(&prov, &venueRef, &localTime, &daysOffset, &observedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ObservedRelease{}, fmt.Errorf("observed %s: %w", ref, ErrNotFound)
		}
		return ObservedRelease{}, fmt.Errorf("GetObserved: %w", err)
	}
	wt, err := domain.ParseWallTime(localTime)
	if err != nil {
		return ObservedRelease{}, fmt.Errorf("observed wall time %q: %w", localTime, err)
	}
	atT, err := parseTime(observedAt)
	if err != nil {
		return ObservedRelease{}, fmt.Errorf("observed at: %w", err)
	}
	return ObservedRelease{
		Venue:             domain.VenueRef{Provider: domain.ProviderID(prov), Ref: venueRef},
		ObservedLocalTime: wt,
		DaysOffset:        daysOffset,
		ObservedAt:        atT,
	}, nil
}

// helpers ---------------------------------------------------------------

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}
