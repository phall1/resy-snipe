// Package engine implements the workflow state machine, scheduler
// loop, and race-and-cancel booking orchestration. It depends on the
// providers interface (never on a concrete adapter) and on the store
// interface, so a second provider or persistence backend can be
// added without engine changes.
package engine

import (
	"context"
	"fmt"
	"log/slog"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/store"
)

// Engine ties together the persistence layer, the injected clock, and
// the structured logger. It is the entry point that the CLI and the
// future daemon construct.
type Engine struct {
	store store.Store
	clock clock.Clock
	log   *slog.Logger
}

// New constructs an Engine. All dependencies are required; nil panics
// to fail fast at boot rather than at first call.
func New(s store.Store, c clock.Clock, log *slog.Logger) *Engine {
	if s == nil {
		panic("engine: nil store")
	}
	if c == nil {
		panic("engine: nil clock")
	}
	if log == nil {
		panic("engine: nil logger")
	}
	return &Engine{store: s, clock: c, log: log}
}

// Load reads the snipe with the given id and returns the engine-level
// wrapper. Returns store.ErrNotFound (wrapped) if the snipe does not
// exist.
func (e *Engine) Load(ctx context.Context, id domain.SnipeID) (*SnipeState, error) {
	inner, err := e.store.GetSnipe(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("engine: load %s: %w", id, err)
	}
	return &SnipeState{inner: inner, e: e}, nil
}

// Submit creates a new snipe with the given intent and returns the
// engine-level handle. The new state lands in StatusSubmitted with a
// matching EventSubmitted record.
func (e *Engine) Submit(ctx context.Context, id domain.SnipeID, intent domain.Intent) (*SnipeState, error) {
	now := e.clock.Now()
	inner := domain.NewSnipeState(id, intent, now)
	if err := e.store.CreateSnipe(ctx, inner); err != nil {
		return nil, fmt.Errorf("engine: create %s: %w", id, err)
	}
	ev := domain.Event{
		Type: domain.EventSubmitted,
		At:   now,
		Attrs: []slog.Attr{
			slog.String(domain.LogKeySnipeID, string(id)),
			slog.String(domain.LogKeyIntentHash, string(intent.Hash())),
			slog.String(domain.LogKeyVenueRef, intent.Venue.String()),
		},
	}
	if err := e.store.AppendEvent(ctx, id, ev); err != nil {
		return nil, fmt.Errorf("engine: append submitted event %s: %w", id, err)
	}
	e.log.Info("snipe submitted",
		slog.String(domain.LogKeySnipeID, string(id)),
		slog.String(domain.LogKeyIntentHash, string(intent.Hash())),
		slog.String(domain.LogKeyVenueRef, intent.Venue.String()),
	)
	return &SnipeState{inner: inner, e: e}, nil
}
