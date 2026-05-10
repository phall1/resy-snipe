package store

// audit.go: persistence-side helpers backing the v2 audit-log contract
// (docs/v2/design/multi-user.md §audit_events, §audit-log-conventions
// and docs/v2/design/service-layer.md §audit-log-contract).
//
// Every Service-layer call writes exactly one audit_events row before
// returning. Failed auth still logs. The row is written by the
// service.Standard wrapper helper (Standard.audit); this file is the
// SQL seam — a single INSERT for the writer, a thin SELECT for the
// future operator-admin reader (M2 wave).
//
// The package-level shape (vs. a Store interface method) mirrors the
// quests and idempotency CRUD: every signature filters on user_id at
// the SQL level so the tenancy harness in tenant_check_test.go does
// not need an allowlist entry. WriteAuditEvent and ListAuditEvents
// both take a userID (the actor for the writer, the operator querying
// for the reader); they are tenant-scoped, not cross-tenant.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"resy-snipe/internal/domain"
)

// ErrAuditWrite is returned by WriteAuditEvent when the underlying
// INSERT fails. The Service wraps it in a WARN log and proceeds — an
// audit-write failure must never fail the parent Service call (it
// would punish the caller for a persistence-layer problem they can't
// fix; design/service-layer.md §audit-log-contract). Callers branch
// with errors.Is.
var ErrAuditWrite = errors.New("store: audit write failed")

// AuditEventInput is the row shape WriteAuditEvent persists. IP and
// UserAgent are transport concerns (HTTP-side metadata) and stay
// empty for the v1 wave — M2 transports populate them when they wire
// the bearer-token middleware.
//
// DetailsJSON is bounded to 4KiB at the design-doc level
// (multi-user.md §audit_events) and SHOULD NOT contain secrets,
// passwords, or full Goal/Plan payloads — those belong in
// quest_events. The store does not enforce the bound; callers (the
// Service) are responsible for keeping the payload small.
type AuditEventInput struct {
	UserID       domain.UserID
	TargetUserID *domain.UserID
	Action       string
	TargetID     string
	OK           bool
	ErrorCode    string
	IP           string
	UserAgent    string
	DetailsJSON  []byte
}

// AuditEventRow is the projection ListAuditEvents returns. Mirrors the
// columns in migration 0002_v2_multi_user.sql §audit_events.
type AuditEventRow struct {
	ID           int64
	UserID       domain.UserID
	TargetUserID *domain.UserID
	Action       string
	TargetID     string
	OK           bool
	ErrorCode    string
	IP           string
	UserAgent    string
	CreatedAt    time.Time
	DetailsJSON  []byte
}

// AuditEventFilter narrows the row set ListAuditEvents returns. Empty
// fields are wildcards; Limit<=0 means "no limit". The reader is
// future-facing (M2 operator-admin verb); the filter shape is
// minimal — Action and time bounds cover the design's three indexed
// queries (actor-recent, target-recent, action-recent).
type AuditEventFilter struct {
	Action string
	Since  *time.Time
	Until  *time.Time
	Limit  int
}

// WriteAuditEvent inserts one audit_events row for the supplied input.
// userID (the actor) is required; every other field is optional.
//
// Tenancy: userID is the actor and is filtered on every read. The
// audit_events table is per-tenant from the actor's perspective, but
// cross-tenant from the subject's (target_user_id) perspective — the
// design folds both into the same table per multi-user.md §8.
//
// A write failure surfaces as ErrAuditWrite (wrapped with %w so the
// caller can branch). The Service wraps that into a WARN log and
// proceeds — never fail the parent call because of an audit-write
// problem.
func WriteAuditEvent(ctx context.Context, db *sql.DB, evt AuditEventInput) error {
	if db == nil {
		return fmt.Errorf("%w: nil db", ErrAuditWrite)
	}
	if evt.UserID == "" {
		return fmt.Errorf("%w: actor UserID is required", ErrAuditWrite)
	}
	if evt.Action == "" {
		return fmt.Errorf("%w: action is required", ErrAuditWrite)
	}

	var targetUser any
	if evt.TargetUserID != nil && *evt.TargetUserID != "" {
		targetUser = string(*evt.TargetUserID)
	}
	var targetID any
	if evt.TargetID != "" {
		targetID = evt.TargetID
	}
	var errCode any
	if evt.ErrorCode != "" {
		errCode = evt.ErrorCode
	}
	var ip any
	if evt.IP != "" {
		ip = evt.IP
	}
	var ua any
	if evt.UserAgent != "" {
		ua = evt.UserAgent
	}
	var details any
	if len(evt.DetailsJSON) > 0 {
		details = string(evt.DetailsJSON)
	}
	okInt := 0
	if evt.OK {
		okInt = 1
	}

	// created_at: we use strftime('%s', 'now') * 1000 inline so the
	// INSERT is self-contained — Service.audit does not need to thread
	// a fresh clock value through (Law 7 is preserved because the
	// audit row's timestamp is bookkeeping, not a domain mutation;
	// see design/service-layer.md §audit-log-contract for the
	// exemption). M2 may switch to a caller-supplied `now` if the
	// transport's request-ID middleware needs to align timestamps.
	_, err := db.ExecContext(ctx, `
		INSERT INTO audit_events
			(user_id, target_user_id, action, target_id, ok, error_code, ip, user_agent, created_at, details_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, CAST(strftime('%s', 'now') AS INTEGER) * 1000, ?)`,
		string(evt.UserID),
		targetUser,
		evt.Action,
		targetID,
		okInt,
		errCode,
		ip,
		ua,
		details,
	)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrAuditWrite, err)
	}
	return nil
}

// ListAuditEvents returns audit_events rows owned by userID (as the
// actor), narrowed by filter, ordered most-recent-first. The reader
// is a future operator-admin surface (M2 wave); M1 wires it for the
// writer-side tests and to keep the SQL shape stable.
//
// Tenancy: every WHERE filters on user_id at the SQL level. A
// cross-tenant operator query (audit.read) would need a separate
// package-level function on target_user_id; that lives behind the
// admin role gate when it lands.
func ListAuditEvents(
	ctx context.Context,
	db *sql.DB,
	userID domain.UserID,
	filter AuditEventFilter,
) ([]AuditEventRow, error) {
	if db == nil {
		return nil, errors.New("ListAuditEvents: nil db")
	}

	var (
		where []string
		args  []any
	)
	where = append(where, "user_id = ?")
	args = append(args, string(userID))
	if filter.Action != "" {
		where = append(where, "action = ?")
		args = append(args, filter.Action)
	}
	if filter.Since != nil {
		where = append(where, "created_at >= ?")
		args = append(args, filter.Since.UnixMilli())
	}
	if filter.Until != nil {
		where = append(where, "created_at <= ?")
		args = append(args, filter.Until.UnixMilli())
	}

	// The WHERE fragments are const literals; user-controlled values
	// land in args only. gosec G202 is suppressed by review per the
	// peer queries in quests.go.
	//nolint:gosec // G202: WHERE fragments are const literals.
	q := `
		SELECT id, user_id, target_user_id, action, target_id, ok, error_code, ip, user_agent, created_at, details_json
		FROM audit_events
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY created_at DESC, id DESC`
	if filter.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListAuditEvents: %w", err)
	}
	defer rows.Close()

	out := []AuditEventRow{}
	for rows.Next() {
		var (
			id                                                int64
			actor, action                                     string
			targetUser, targetID, errCode, ip, ua, detailsRaw sql.NullString
			ok                                                int
			createdAt                                         int64
		)
		if err := rows.Scan(&id, &actor, &targetUser, &action, &targetID, &ok, &errCode, &ip, &ua, &createdAt, &detailsRaw); err != nil {
			return nil, fmt.Errorf("ListAuditEvents scan: %w", err)
		}
		row := AuditEventRow{
			ID:        id,
			UserID:    domain.UserID(actor),
			Action:    action,
			OK:        ok == 1,
			CreatedAt: time.UnixMilli(createdAt).UTC(),
		}
		if targetUser.Valid {
			u := domain.UserID(targetUser.String)
			row.TargetUserID = &u
		}
		if targetID.Valid {
			row.TargetID = targetID.String
		}
		if errCode.Valid {
			row.ErrorCode = errCode.String
		}
		if ip.Valid {
			row.IP = ip.String
		}
		if ua.Valid {
			row.UserAgent = ua.String
		}
		if detailsRaw.Valid {
			row.DetailsJSON = []byte(detailsRaw.String)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListAuditEvents iterate: %w", err)
	}
	return out, nil
}
