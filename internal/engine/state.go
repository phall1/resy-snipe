package engine

import (
	"context"
	"fmt"
	"log/slog"

	"resy-snipe/internal/domain"
)

// SnipeState wraps a domain.SnipeState with the engine handle so
// Transition can be the single, atomic gate for status changes:
//
//   - validate via the domain transition table,
//   - persist the new status and an Event in one Store transaction,
//   - swap the in-memory state only after the persist commits.
//
// Engine code outside this file MUST NOT mutate the inner SnipeState
// directly. There is no setter for status; the only path is
// Transition.
type SnipeState struct {
	inner *domain.SnipeState
	e     *Engine
}

// ID returns the snipe's identifier.
func (s *SnipeState) ID() domain.SnipeID { return s.inner.ID }

// Status returns the current persisted status.
func (s *SnipeState) Status() domain.Status { return s.inner.Status() }

// Intent returns the user's original intent (immutable).
func (s *SnipeState) Intent() domain.Intent { return s.inner.Intent }

// Inner returns a read-only handle to the underlying domain state.
// Callers must not call Transition on it directly — that bypass would
// move the in-memory state ahead of the database. Use SnipeState.Transition.
func (s *SnipeState) Inner() *domain.SnipeState { return s.inner }

// Transition advances the snipe's status and atomically appends an
// Event of eventType with the supplied attrs. If the transition is
// not in the table, it returns domain.InvalidTransitionError and
// touches no persistence. If the persist fails, the in-memory state
// is left unchanged.
func (s *SnipeState) Transition(
	ctx context.Context,
	to domain.Status,
	eventType domain.EventType,
	attrs ...slog.Attr,
) error {
	from := s.inner.Status()
	if !domain.CanTransition(from, to) {
		return domain.InvalidTransitionError{From: from, To: to}
	}

	now := s.e.clock.Now()

	// Build a candidate clone so a failed persist never leaves the
	// live in-memory state ahead of the database.
	candidate := s.inner.Clone()
	if err := candidate.Transition(to); err != nil {
		// Should not happen — CanTransition already passed.
		return fmt.Errorf("engine: candidate.Transition: %w", err)
	}
	candidate.UpdatedAt = now

	// Always include snipe_id on the audit event so log queries can
	// trace a snipe end-to-end.
	finalAttrs := make([]slog.Attr, 0, len(attrs)+1)
	finalAttrs = append(finalAttrs, slog.String(domain.LogKeySnipeID, string(candidate.ID)))
	finalAttrs = append(finalAttrs, attrs...)

	ev := domain.Event{
		Type:  eventType,
		At:    now,
		Attrs: finalAttrs,
	}

	if err := s.e.store.TransitionSnipe(ctx, candidate, ev); err != nil {
		return fmt.Errorf("engine: persist transition %s -> %s: %w", from, to, err)
	}

	// Persist committed; swap.
	s.inner = candidate

	logAttrs := append([]slog.Attr{
		slog.String("event", string(eventType)),
		slog.String("from", from.String()),
		slog.String("to", to.String()),
	}, finalAttrs...)
	s.e.log.LogAttrs(ctx, slog.LevelInfo, "snipe transition", logAttrs...)
	return nil
}
