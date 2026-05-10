package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"resy-snipe/internal/domain"
)

// QuestRow is the read/write projection of one v2 `quests` row.
// Mirrors the columns declared in migration 0002_v2_multi_user.sql
// §quests. The shape is deliberately bare: callers persist a
// pre-canonicalized GoalJSON + PlanHash, and the Service layer is
// responsible for marshaling the in-memory Goal/Plan into those wire
// strings.
//
// Every column carries user_id at the SQL level (idx_quests_user_*),
// which is how cross-tenant isolation is enforced — see
// docs/v2/design/multi-user.md §tenancy-enforcement. The v2 quest
// CRUD lives as package-level functions (not Store interface methods)
// so the tenancy harness in tenant_check_test.go does not need an
// allowlist entry; the Service.StoreBackend interface defined inside
// internal/service is the consumer-side seam that binds these
// functions back to the Service layer.
type QuestRow struct {
	ID          domain.QuestID
	UserID      domain.UserID
	AccountID   domain.AccountID
	GoalJSON    string
	PlanHash    string
	Status      domain.Status
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// QuestListFilter narrows the row set returned by ListQuests. Empty
// fields are wildcards; Limit<=0 means "no limit"; Cursor (when
// non-empty) is interpreted as a created_at lower bound in RFC-3339
// nanos — the Service's filter cursor surfaces here verbatim.
type QuestListFilter struct {
	Status    []domain.Status
	AccountID *domain.AccountID
	Since     *time.Time
	Until     *time.Time
	Limit     int
	Cursor    string
}

// statusDBString returns the value persisted in quests.status. The
// CHECK constraint on quests.status enumerates a v2-specific set
// that does not match domain.Status.String() one-to-one —
// domain.StatusSubmitted renders as "submitted" while the v2 schema
// spells the starting state "pending", and StatusCanceled renders as
// "canceled" (US spelling) while the schema spells it with the
// double-l British form. Mapping centrally here means the rest of
// the package can treat domain.Status as the canonical in-memory
// form.
//
//nolint:misspell // SQL CHECK constraint value is fixed by migration 0002.
func statusDBString(s domain.Status) string {
	switch s {
	case domain.StatusSubmitted:
		return "pending"
	case domain.StatusScheduled, domain.StatusDiscovering, domain.StatusAwaiting:
		return "scheduled"
	case domain.StatusFinding, domain.StatusBooking:
		return "firing"
	case domain.StatusBooked:
		return "booked"
	case domain.StatusFailed, domain.StatusExpired:
		return "failed"
	case domain.StatusCanceled:
		return "cancelled"
	default:
		return "pending"
	}
}

// parseStatusDB inverts statusDBString. The mapping is lossy in the
// scheduled/firing/failed buckets — we collapse multiple in-memory
// statuses onto a single persisted value — so the inverse picks the
// canonical representative for each bucket. M1-11 will refine the
// quest_events stream so callers that need fine-grained engine status
// read it from there rather than from the quest row.
//
//nolint:misspell // SQL value is fixed by migration 0002.
func parseStatusDB(s string) (domain.Status, error) {
	switch s {
	case "pending":
		return domain.StatusSubmitted, nil
	case "scheduled":
		return domain.StatusScheduled, nil
	case "firing":
		return domain.StatusFinding, nil
	case "booked":
		return domain.StatusBooked, nil
	case "failed":
		return domain.StatusFailed, nil
	case "cancelled":
		return domain.StatusCanceled, nil
	default:
		return 0, fmt.Errorf("unknown quest status %q", s)
	}
}

// CreateQuest inserts one v2 quests row. Every column is provided by
// the caller; the function performs no validation beyond the schema
// constraints. Returns ErrNotFound if the (user_id, account_id) FK
// targets do not exist — the caller should have created them via
// SeedOperator + Login before reaching this point.
//
// Cross-tenant safety: user_id is part of every column write and is
// the primary filter on every read. The Service layer is the trust
// boundary for "is this caller allowed to write this row?"; CreateQuest
// trusts the supplied userID.
func CreateQuest(ctx context.Context, db *sql.DB, q QuestRow) error {
	if db == nil {
		return errors.New("CreateQuest: nil db")
	}
	if q.ID == "" || q.UserID == "" || q.AccountID == "" {
		return errors.New("CreateQuest: ID, UserID, AccountID are required")
	}
	var completedAt any
	if q.CompletedAt != nil {
		completedAt = q.CompletedAt.UnixMilli()
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO quests (id, user_id, account_id, goal_json, plan_hash, status, created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(q.ID),
		string(q.UserID),
		string(q.AccountID),
		q.GoalJSON,
		q.PlanHash,
		statusDBString(q.Status),
		q.CreatedAt.UnixMilli(),
		q.UpdatedAt.UnixMilli(),
		completedAt,
	)
	if err != nil {
		return fmt.Errorf("CreateQuest: %w", err)
	}
	return nil
}

// GetQuest returns the row identified by (userID, questID). A row
// owned by a different user surfaces as ErrNotFound, NOT a separate
// ErrUnauthorized — the design folds existence and ownership into one
// signal so a tenant cannot probe another tenant's quest IDs (see
// docs/v2/design/service-layer.md §identity-scoping).
func GetQuest(ctx context.Context, db *sql.DB, userID domain.UserID, questID domain.QuestID) (QuestRow, error) {
	if db == nil {
		return QuestRow{}, errors.New("GetQuest: nil db")
	}
	row := db.QueryRowContext(ctx, `
		SELECT id, user_id, account_id, goal_json, plan_hash, status, created_at, updated_at, completed_at
		FROM quests
		WHERE id = ? AND user_id = ?`,
		string(questID), string(userID))
	return scanQuestRow(row)
}

// ListQuests returns rows owned by userID, narrowed by filter.
// Ordering is created_at descending so the caller's UI sees the most
// recent quest first.
func ListQuests(ctx context.Context, db *sql.DB, userID domain.UserID, filter QuestListFilter) ([]QuestRow, error) {
	if db == nil {
		return nil, errors.New("ListQuests: nil db")
	}

	var (
		where []string
		args  []any
	)
	where = append(where, "user_id = ?")
	args = append(args, string(userID))

	if len(filter.Status) > 0 {
		// Translate each domain.Status to its DB enum and DEDUPE; the
		// status mapping is many-to-one, so two distinct in-memory
		// statuses collapse to one filter value and we must not emit
		// `IN (?, ?)` with a duplicate placeholder.
		seen := map[string]struct{}{}
		var enumVals []string
		for _, s := range filter.Status {
			v := statusDBString(s)
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
	if filter.AccountID != nil {
		where = append(where, "account_id = ?")
		args = append(args, string(*filter.AccountID))
	}
	if filter.Since != nil {
		where = append(where, "created_at >= ?")
		args = append(args, filter.Since.UnixMilli())
	}
	if filter.Until != nil {
		where = append(where, "created_at <= ?")
		args = append(args, filter.Until.UnixMilli())
	}
	if filter.Cursor != "" {
		ts, err := time.Parse(time.RFC3339Nano, filter.Cursor)
		if err != nil {
			return nil, fmt.Errorf("ListQuests: cursor %q: %w", filter.Cursor, err)
		}
		// Cursor is exclusive — "rows older than this".
		where = append(where, "created_at < ?")
		args = append(args, ts.UnixMilli())
	}

	// The WHERE fragments are all const literals built above; user
	// values land in `args` only. gosec flags the string concat, but
	// no caller-controlled bytes reach the SQL text — see
	// docs/laws.md §Persistence.
	//nolint:gosec // G202: the WHERE fragments are const literals.
	q := `
		SELECT id, user_id, account_id, goal_json, plan_hash, status, created_at, updated_at, completed_at
		FROM quests
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY created_at DESC, id DESC`
	if filter.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListQuests: %w", err)
	}
	defer rows.Close()

	out := []QuestRow{}
	for rows.Next() {
		qr, scanErr := scanQuestRow(rowScanner{rows})
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, qr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListQuests iterate: %w", err)
	}
	return out, nil
}

// UpdateQuestStatus flips a quest's status (and optionally completed_at)
// in place. The WHERE clause filters on user_id so a wrong-tenant call
// produces zero rows-affected, which surfaces as ErrNotFound.
//
// updatedAt is supplied by the caller per Law 7 (the Service owns the
// clock and stamps every mutation).
func UpdateQuestStatus(
	ctx context.Context,
	db *sql.DB,
	userID domain.UserID,
	questID domain.QuestID,
	newStatus domain.Status,
	completedAt *time.Time,
	updatedAt time.Time,
) error {
	if db == nil {
		return errors.New("UpdateQuestStatus: nil db")
	}
	var completedArg any
	if completedAt != nil {
		completedArg = completedAt.UnixMilli()
	}
	res, err := db.ExecContext(ctx, `
		UPDATE quests
		SET status = ?, updated_at = ?, completed_at = ?
		WHERE id = ? AND user_id = ?`,
		statusDBString(newStatus),
		updatedAt.UnixMilli(),
		completedArg,
		string(questID),
		string(userID),
	)
	if err != nil {
		return fmt.Errorf("UpdateQuestStatus: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("UpdateQuestStatus rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("quest %s: %w", questID, ErrNotFound)
	}
	return nil
}

// scanQuestRow decodes one quests row. Shared between GetQuest and
// ListQuests so the column-order contract is in exactly one place.
func scanQuestRow(s scanner) (QuestRow, error) {
	var (
		id, userID, accountID, goalJSON, planHash, status string
		createdAt, updatedAt                              int64
		completedAt                                       sql.NullInt64
	)
	if err := s.Scan(&id, &userID, &accountID, &goalJSON, &planHash, &status, &createdAt, &updatedAt, &completedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return QuestRow{}, fmt.Errorf("quest: %w", ErrNotFound)
		}
		return QuestRow{}, fmt.Errorf("scan quest: %w", err)
	}
	st, err := parseStatusDB(status)
	if err != nil {
		return QuestRow{}, fmt.Errorf("quest %s: %w", id, err)
	}
	qr := QuestRow{
		ID:        domain.QuestID(id),
		UserID:    domain.UserID(userID),
		AccountID: domain.AccountID(accountID),
		GoalJSON:  goalJSON,
		PlanHash:  planHash,
		Status:    st,
		CreatedAt: time.UnixMilli(createdAt).UTC(),
		UpdatedAt: time.UnixMilli(updatedAt).UTC(),
	}
	if completedAt.Valid {
		t := time.UnixMilli(completedAt.Int64).UTC()
		qr.CompletedAt = &t
	}
	return qr, nil
}

// MarshalGoal canonicalizes a domain.Goal into the JSON form stored in
// quests.goal_json. The Goal type carries a sealed-union VenueQuery
// field that encoding/json cannot roundtrip on its own, so the codec
// adds a "kind" discriminator and stable field shapes. This codec is
// the persistence boundary equivalent of MarshalIntent and is owned
// by the store package for the same reason: the on-disk shape evolves
// independently of domain.Goal (with a migration if necessary).
func MarshalGoal(g domain.Goal) ([]byte, error) {
	out := goalJSONShape{
		VenueQuery: encodeVenueQuery(g.VenueQuery),
		Date:       g.Date.String(),
		Party:      g.Party,
		TimePrefs: timeWindowJSON{
			Start:    g.TimePrefs.Start.String(),
			End:      g.TimePrefs.End.String(),
			Priority: g.TimePrefs.Priority.String(),
		},
		AccountID: string(g.AccountID),
		Constraints: constraintsJSON{
			TableTypes: append([]string(nil), g.Constraints.TableTypes...),
			AnyTable:   g.Constraints.AnyTable,
		},
	}
	return json.Marshal(out)
}

// UnmarshalGoal inverts MarshalGoal. An unknown venue_query.kind
// surfaces as a descriptive error rather than a silent zero value —
// the goal_json column is canonical, and a missing variant means
// somebody added a new VenueQuery without updating this codec.
func UnmarshalGoal(data []byte) (domain.Goal, error) {
	var raw goalJSONShape
	if err := json.Unmarshal(data, &raw); err != nil {
		return domain.Goal{}, fmt.Errorf("UnmarshalGoal: %w", err)
	}
	vq, err := decodeVenueQuery(raw.VenueQuery)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("UnmarshalGoal: %w", err)
	}
	d, err := domain.ParseDate(raw.Date)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("UnmarshalGoal date: %w", err)
	}
	start, err := domain.ParseWallTime(raw.TimePrefs.Start)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("UnmarshalGoal time_prefs.start: %w", err)
	}
	end, err := domain.ParseWallTime(raw.TimePrefs.End)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("UnmarshalGoal time_prefs.end: %w", err)
	}
	return domain.Goal{
		VenueQuery: vq,
		Date:       d,
		Party:      raw.Party,
		TimePrefs: domain.TimeWindow{
			Start:    start,
			End:      end,
			Priority: parseTimePriority(raw.TimePrefs.Priority),
		},
		AccountID: domain.AccountID(raw.AccountID),
		Constraints: domain.Constraints{
			TableTypes: raw.Constraints.TableTypes,
			AnyTable:   raw.Constraints.AnyTable,
		},
	}, nil
}

type goalJSONShape struct {
	VenueQuery  venueQueryJSON  `json:"venue_query"`
	Date        string          `json:"date"`
	Party       int             `json:"party"`
	TimePrefs   timeWindowJSON  `json:"time_prefs"`
	AccountID   string          `json:"account_id"`
	Constraints constraintsJSON `json:"constraints"`
}

type venueQueryJSON struct {
	Kind string `json:"kind"`
	URL  string `json:"url,omitempty"`
	Slug string `json:"slug,omitempty"`
	City string `json:"city,omitempty"`
	Name string `json:"name,omitempty"`
}

type timeWindowJSON struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Priority string `json:"priority"`
}

type constraintsJSON struct {
	TableTypes []string `json:"table_types,omitempty"`
	AnyTable   bool     `json:"any_table,omitempty"`
}

func encodeVenueQuery(q domain.VenueQuery) venueQueryJSON {
	switch v := q.(type) {
	case domain.VenueQueryURL:
		return venueQueryJSON{Kind: "url", URL: v.URL}
	case domain.VenueQuerySlug:
		return venueQueryJSON{Kind: "slug", Slug: v.Slug, City: v.City}
	case domain.VenueQueryName:
		return venueQueryJSON{Kind: "name", Name: v.Name}
	default:
		return venueQueryJSON{Kind: "nil"}
	}
}

func decodeVenueQuery(j venueQueryJSON) (domain.VenueQuery, error) {
	switch j.Kind {
	case "url":
		return domain.VenueQueryURL{URL: j.URL}, nil
	case "slug":
		return domain.VenueQuerySlug{Slug: j.Slug, City: j.City}, nil
	case "name":
		return domain.VenueQueryName{Name: j.Name}, nil
	case "nil", "":
		return nil, nil //nolint:nilnil // intentional optional-value sentinel
	default:
		return nil, fmt.Errorf("unknown venue_query.kind %q", j.Kind)
	}
}

func parseTimePriority(s string) domain.TimePriority {
	switch s {
	case "earlier":
		return domain.PriorityEarlier
	case "later":
		return domain.PriorityLater
	default:
		return domain.PriorityNone
	}
}
