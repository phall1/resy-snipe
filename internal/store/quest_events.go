package store

// quest_events.go: writer-side helpers for the v2 quest_events stream.
// The reader (ListQuestEvents) lives in quests.go alongside the rest
// of the quests CRUD; the writer lives here so the engine-event →
// quest_events bridge has a single owner.
//
// The Service layer is the only caller. Standard.SubscribeQuest
// installs an engine.Subscribe callback that funnels every transition
// notification through WriteQuestEvent (M1-11 §engine-event → quest_event
// bridge). Other Service methods that effect explicit lifecycle moves
// (CreateQuest → "submitted", CancelQuest → "canceled") also call
// WriteQuestEvent directly.
//
// Tenancy: every write filters on user_id at the SQL level. The row
// carries the actor's user_id (denormalized from the parent quests
// row), and the insert is gated by a subquery that re-checks
// quests.user_id matches — a wrong-tenant call therefore inserts
// zero rows, which surfaces as ErrNotFound.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"resy-snipe/internal/domain"
)

// WriteQuestEvent inserts one quest_events row for (userID, questID).
// The engine-emitted Event's Type, At, and Attrs are persisted; the
// Attrs slice is canonicalized through EncodeAttrs (the same codec the
// v1 events table uses) so the on-disk JSON shape is stable across
// reader and writer.
//
// Tenancy: the INSERT's quest_id is gated by a SELECT … WHERE quest_id
// AND user_id, so a caller passing a userID that does not own questID
// is a no-op (zero rows-affected → ErrNotFound). The Service has
// already gated on tenancy upstream (every SubscribeQuest call passes
// the GetQuest tenancy gate first; CreateQuest writes after a
// successful quests insert), so this is defense in depth rather than
// the primary check.
//
// A non-fatal write failure surfaces wrapped; the Service treats every
// quest_event write the same way it treats audit writes — log a WARN
// and proceed (a missing event-stream row is a degraded experience,
// not a correctness bug; the engine's in-memory state machine still
// advances).
func WriteQuestEvent(
	ctx context.Context,
	db *sql.DB,
	userID domain.UserID,
	questID domain.QuestID,
	evt domain.Event,
) error {
	if db == nil {
		return errors.New("WriteQuestEvent: nil db")
	}
	if userID == "" || questID == "" {
		return errors.New("WriteQuestEvent: userID and questID are required")
	}
	if evt.Type == "" {
		return errors.New("WriteQuestEvent: event type is required")
	}

	fieldsJSON, err := EncodeAttrs(evt.Attrs)
	if err != nil {
		return fmt.Errorf("WriteQuestEvent encode attrs: %w", err)
	}
	var fieldsArg any
	if len(fieldsJSON) > 0 {
		fieldsArg = string(fieldsJSON)
	}

	// The INSERT … SELECT form enforces tenancy at the SQL layer:
	// zero rows match when the (questID, userID) pair does not own
	// a quests row, and the insert silently no-ops. We then check
	// rows-affected and surface ErrNotFound on miss so the Service
	// can branch on a documented sentinel rather than a swallowed
	// success.
	res, err := db.ExecContext(ctx, `
		INSERT INTO quest_events (quest_id, user_id, type, at, fields_json)
		SELECT ?, ?, ?, ?, ?
		WHERE EXISTS (
			SELECT 1 FROM quests
			WHERE id = ? AND user_id = ?
		)`,
		string(questID),
		string(userID),
		string(evt.Type),
		evt.At.UnixMilli(),
		fieldsArg,
		string(questID),
		string(userID),
	)
	if err != nil {
		return fmt.Errorf("WriteQuestEvent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("WriteQuestEvent rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("quest %s: %w", questID, ErrNotFound)
	}
	return nil
}
