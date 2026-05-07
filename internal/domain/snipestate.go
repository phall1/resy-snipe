package domain

import (
	"log/slog"
	"time"
)

// Result describes the outcome of a successful booking. Failure modes
// are recorded on the Event log instead of populating Result.
type Result struct {
	BookedAt       time.Time
	ConfirmationID string
}

// Event is a single audit record appended every time a SnipeState
// transitions or otherwise emits a noteworthy fact. Attrs is the
// typed, slog-style attribute payload. We intentionally use
// []slog.Attr rather than map[string]any so the field stays typed at
// the language level and so the same attributes can be passed to the
// engine's logger without a conversion.
type Event struct {
	Type  EventType
	At    time.Time
	Attrs []slog.Attr
}

// SnipeState is the engine's per-snipe view: the user's Intent plus the
// runtime status, scheduling, result, and audit trail. Status is
// unexported and only changeable through Transition so the guard
// cannot be bypassed.
type SnipeState struct {
	ID          SnipeID
	Intent      Intent
	status      Status
	ScheduledAt time.Time
	Result      *Result
	Events      []Event
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewSnipeState constructs a SnipeState in the initial Submitted status.
// Timestamps are caller-provided (the engine owns the clock); no
// time.Now() lives outside internal/clock.
func NewSnipeState(id SnipeID, intent Intent, createdAt time.Time) *SnipeState {
	return &SnipeState{
		ID:        id,
		Intent:    intent,
		status:    StatusSubmitted,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
}

// Status returns the current status. The state is read-only from
// outside the package; mutation goes through Transition.
func (s *SnipeState) Status() Status { return s.status }

// Transition advances the state machine to `to`. It returns
// InvalidTransitionError if the move is not in the table; on success
// the status is updated. UpdatedAt is intentionally NOT updated here:
// the engine layer (which owns the clock) is responsible for stamping
// time, so the domain package can stay clock-free.
func (s *SnipeState) Transition(to Status) error {
	if !CanTransition(s.status, to) {
		return InvalidTransitionError{From: s.status, To: to}
	}
	s.status = to
	return nil
}
