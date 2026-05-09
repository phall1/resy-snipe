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
	"sync"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/store"
)

// Engine ties together the persistence layer, the injected clock, and
// the structured logger. It is the entry point that the CLI and the
// future daemon construct.
type Engine struct {
	store    store.Store
	clock    clock.Clock
	log      *slog.Logger
	provider providers.Provider
	policy   domain.BookingPolicy

	// Subscriber registry for the lifecycle event stream. See
	// subscribe.go for the public Subscribe / Notification surface.
	// nextSubID is monotonic so emit can deliver in registration order.
	subsMu    sync.RWMutex
	subs      map[uint64]Subscriber
	nextSubID uint64
}

// Option configures an Engine at construction.
type Option func(*Engine)

// WithProvider supplies the cross-provider booking adapter the engine
// uses for Calendar / Find / Book during release-strategy loops.
// Without a provider, only ExplicitRelease snipes can run end-to-end;
// Discovered and Continuous strategies require it.
func WithProvider(p providers.Provider) Option { return func(e *Engine) { e.provider = p } }

// WithBookingPolicy overrides the engine's default BookingPolicy
// (PollFloor 100ms, MaxConcurrent 4, DetailsSerial true). The intent
// can override per-snipe in a future Phase 2 extension; for now the
// engine-level default applies to every snipe Run handles.
func WithBookingPolicy(p domain.BookingPolicy) Option { return func(e *Engine) { e.policy = p } }

// defaultBookingPolicy is the empirical safe baseline for Resy: 100ms
// minimum interval between consecutive provider calls (anti-bot
// floor), at most 4 concurrent /3/book attempts, and serialized
// /3/details fetches so we don't burn book_tokens we won't use.
func defaultBookingPolicy() domain.BookingPolicy {
	return domain.BookingPolicy{
		MaxConcurrent: 4,
		PollFloor:     100 * time.Millisecond,
		DetailsSerial: true,
	}
}

// New constructs an Engine. All dependencies are required; nil panics
// to fail fast at boot rather than at first call. Options layer on
// top: WithProvider attaches the cross-provider adapter, WithBooking-
// Policy overrides the default rate-limit knobs.
func New(s store.Store, c clock.Clock, log *slog.Logger, opts ...Option) *Engine {
	if s == nil {
		panic("engine: nil store")
	}
	if c == nil {
		panic("engine: nil clock")
	}
	if log == nil {
		panic("engine: nil logger")
	}
	e := &Engine{
		store:  s,
		clock:  c,
		log:    log,
		policy: defaultBookingPolicy(),
	}
	for _, o := range opts {
		o(e)
	}
	return e
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
	// Submitted is the bootstrap emission — there is no prior status,
	// so From is nil. Consumers that care about transitions can ignore
	// nil-From notifications; consumers rendering a full lifecycle
	// trace use it as the start marker.
	e.emit(Notification{
		SnipeID: id,
		From:    nil,
		To:      domain.StatusSubmitted,
		Event:   ev,
	})
	return &SnipeState{inner: inner, e: e}, nil
}
