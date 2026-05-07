package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
)

// errUnsupportedReleaseStrategy is returned by Run when the snipe's
// release strategy is one Phase 1 cannot schedule on its own. Phase 1
// only knows how to schedule ExplicitRelease, which carries the
// release time inline. DiscoveredRelease and ContinuousRelease both
// require provider polling to determine when to fire and are deferred
// to E3 (release strategies).
var errUnsupportedReleaseStrategy = errors.New("engine: release strategy not yet supported by Run")

// Run drives a single snipe through the Phase 1 scheduling loop:
//
//  1. Load the snipe.
//  2. If still Submitted, transition Submitted -> Scheduled and stamp
//     ScheduledAt from the intent. (E1 already enforces transition
//     legality.)
//  3. Compute the wake delay as ScheduledAt - clock.Now(). Past or
//     present scheduled times fire synchronously; future ones fire via
//     Clock.AfterFunc.
//  4. On fire, transition Scheduled -> Awaiting and emit EventReleased,
//     which is the seam E3/E4 will hang the booking workflow on.
//
// Run returns once the workflow either fires or exits via ctx
// cancellation. If ctx is canceled before the scheduled time arrives,
// no transition is attempted and Run returns ctx.Err(). Any goroutine
// or timer Run starts is cleaned up before Run returns; there are no
// background leaks once Run is done.
//
// In Phase 2 the daemon will call Run in a long-lived loop per snipe;
// the Phase 1 CLI calls it exactly once per invocation.
func (e *Engine) Run(ctx context.Context, id domain.SnipeID) error {
	state, err := e.Load(ctx, id)
	if err != nil {
		return err
	}

	// Submitted is the only entry point that has not yet committed a
	// scheduled time. Move it to Scheduled and stamp ScheduledAt from
	// the intent so that resumed runs (post-restart) can pick up where
	// they left off without re-deriving the time.
	if state.Status() == domain.StatusSubmitted {
		scheduledAt, err := scheduledAtFromIntent(state.Intent())
		if err != nil {
			return fmt.Errorf("engine: derive scheduled_at for %s: %w", id, err)
		}
		if err := state.transitionToScheduled(ctx, scheduledAt); err != nil {
			return fmt.Errorf("engine: schedule %s: %w", id, err)
		}
	}

	// If the snipe is already past Scheduled (e.g. someone else picked
	// it up, or the loop is being re-entered), there is nothing for
	// Phase 1 Run to do here — it would either step on E3's territory
	// or land on a terminal status. Treat as no-op.
	if state.Status() != domain.StatusScheduled {
		return nil
	}

	delay := state.Inner().ScheduledAt.Sub(e.clock.Now())
	if delay <= 0 {
		return e.fireRelease(ctx, state)
	}

	return e.waitAndFire(ctx, state, delay)
}

// waitAndFire schedules the release callback and blocks until it fires
// or ctx is canceled. Cancellation before fire is silent: no
// transition is attempted and the timer is stopped so the callback
// does not run.
func (e *Engine) waitAndFire(ctx context.Context, state *SnipeState, delay time.Duration) error {
	done := make(chan struct{})
	var fireErr error

	var timer clock.Timer //nolint:staticcheck // timer is captured by the closure below; declaring + assigning together would shadow.
	timer = e.clock.AfterFunc(delay, func() {
		defer close(done)
		// ctx may have been canceled between scheduling and fire. The
		// callback must not race ahead and persist a transition for a
		// snipe whose run was abandoned.
		if ctx.Err() != nil {
			return
		}
		fireErr = e.fireRelease(ctx, state)
	})

	select {
	case <-done:
		return fireErr
	case <-ctx.Done():
		// Two cases:
		//   - Stop() returned true: the callback is guaranteed not to
		//     run. done will never be closed; return immediately.
		//   - Stop() returned false: the callback has already started
		//     (or is about to). Wait for done so we never leave the
		//     callback running after Run returns.
		if timer.Stop() {
			return ctx.Err()
		}
		<-done
		return ctx.Err()
	}
}

// fireRelease performs the post-wake transition. Phase 1 stops at
// Awaiting; E3 picks up from there.
func (e *Engine) fireRelease(ctx context.Context, state *SnipeState) error {
	return state.Transition(ctx, domain.StatusAwaiting, domain.EventReleased,
		slog.String(domain.LogKeyVenueRef, state.Intent().Venue.String()),
		slog.String(domain.LogKeyIntentHash, string(state.Intent().Hash())),
	)
}

// scheduledAtFromIntent extracts the wall-clock fire time from an
// intent's release strategy. Phase 1 only handles ExplicitRelease;
// DiscoveredRelease and ContinuousRelease require provider polling
// and are E3's job.
func scheduledAtFromIntent(intent domain.Intent) (time.Time, error) {
	switch r := intent.Release.(type) {
	case domain.ExplicitRelease:
		return r.At, nil
	case domain.DiscoveredRelease, domain.ContinuousRelease:
		return time.Time{}, errUnsupportedReleaseStrategy
	case nil:
		return time.Time{}, errors.New("engine: intent has nil release strategy")
	default:
		return time.Time{}, fmt.Errorf("engine: unknown release strategy %T", r)
	}
}

// transitionToScheduled moves a Submitted snipe to Scheduled and
// stamps ScheduledAt on the row in the same Store transaction. It is
// the only path that mutates ScheduledAt; future setters would belong
// alongside this one.
//
// Implementation mirrors SnipeState.Transition: clone, mutate the
// candidate, persist atomically, swap on success.
func (s *SnipeState) transitionToScheduled(ctx context.Context, scheduledAt time.Time) error {
	const to = domain.StatusScheduled
	from := s.inner.Status()
	if !domain.CanTransition(from, to) {
		return domain.InvalidTransitionError{From: from, To: to}
	}

	now := s.e.clock.Now()

	candidate := s.inner.Clone()
	if err := candidate.Transition(to); err != nil {
		// CanTransition already passed; reaching here would mean a
		// table inconsistency.
		return fmt.Errorf("engine: candidate.Transition: %w", err)
	}
	candidate.ScheduledAt = scheduledAt
	candidate.UpdatedAt = now

	attrs := []slog.Attr{
		slog.String(domain.LogKeySnipeID, string(candidate.ID)),
		slog.Time("scheduled_at", scheduledAt),
		slog.String(domain.LogKeyVenueRef, candidate.Intent.Venue.String()),
	}
	ev := domain.Event{
		Type:  domain.EventScheduled,
		At:    now,
		Attrs: attrs,
	}

	if err := s.e.store.TransitionSnipe(ctx, candidate, ev); err != nil {
		return fmt.Errorf("engine: persist transition %s -> %s: %w", from, to, err)
	}
	s.inner = candidate

	logAttrs := append([]slog.Attr{
		slog.String("event", string(domain.EventScheduled)),
		slog.String("from", from.String()),
		slog.String("to", to.String()),
	}, attrs...)
	s.e.log.LogAttrs(ctx, slog.LevelInfo, "snipe transition", logAttrs...)
	return nil
}
