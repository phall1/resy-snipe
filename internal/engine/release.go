package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/store"
)

// runStrategy dispatches on the snipe's ReleaseStrategy variant.
// Run has already done the wake-up wait; runStrategy executes the
// strategy-specific behavior from the Scheduled state forward.
//
// All paths honor ctx cancellation: when ctx is canceled mid-loop,
// they exit cleanly without persisting a partial transition.
func (e *Engine) runStrategy(ctx context.Context, state *SnipeState) error {
	switch r := state.Intent().Release.(type) {
	case domain.ExplicitRelease:
		// Caller (Run) already waited until r.At. There's nothing to
		// poll; transition straight to Awaiting.
		return state.Transition(ctx, domain.StatusAwaiting, domain.EventReleased,
			slog.String(domain.LogKeyVenueRef, state.Intent().Venue.String()),
		)
	case domain.DiscoveredRelease:
		if e.provider == nil {
			return errors.New("engine: DiscoveredRelease requires a Provider — pass engine.WithProvider")
		}
		return e.runDiscoveredRelease(ctx, state, r)
	case domain.ContinuousRelease:
		if e.provider == nil {
			return errors.New("engine: ContinuousRelease requires a Provider — pass engine.WithProvider")
		}
		return e.runContinuousRelease(ctx, state, r)
	case nil:
		return errors.New("engine: nil ReleaseStrategy")
	default:
		return fmt.Errorf("engine: unknown ReleaseStrategy %T", r)
	}
}

// runDiscoveredRelease moves the snipe into Discovering and polls
// Provider.Calendar between ProbeFrom and ProbeUntil. When the target
// date appears as available, the wall-clock instant is captured into
// observed_release_times (so the next snipe for this venue can default
// to ExplicitRelease at the same time-of-day) and the snipe transitions
// to Awaiting.
func (e *Engine) runDiscoveredRelease(ctx context.Context, state *SnipeState, r domain.DiscoveredRelease) error {
	if r.ProbeFrom.IsZero() || r.ProbeUntil.IsZero() {
		return errors.New("engine: DiscoveredRelease requires ProbeFrom and ProbeUntil")
	}
	if !r.ProbeUntil.After(r.ProbeFrom) {
		return errors.New("engine: DiscoveredRelease ProbeUntil must be after ProbeFrom")
	}

	if err := e.advanceToDiscovering(ctx, state, r); err != nil {
		return err
	}

	// Wait for ProbeFrom if we are still ahead of it.
	if delay := r.ProbeFrom.Sub(e.clock.Now()); delay > 0 {
		if err := waitWithCtx(ctx, e.clock, delay); err != nil {
			return err
		}
	}

	intent := state.Intent()
	target := intent.Date
	venue := intent.Venue

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !e.clock.Now().Before(r.ProbeUntil) {
			// Window closed without a hit; fail the snipe.
			return state.Transition(ctx, domain.StatusFailed, domain.EventFailed,
				slog.String("reason", "discovered_window_expired"),
				slog.Time("probe_until", r.ProbeUntil),
			)
		}

		cal, err := e.provider.Calendar(ctx, venue, providers.DateRange{
			Start:     target,
			End:       target,
			PartySize: intent.PartySize,
		})
		if err != nil {
			// Soft failures (rate limited, transient anti-bot) bubble
			// up to the engine wrapper which will re-call the strategy
			// after a backoff. For Phase 1 we surface them once and
			// stop.
			return fmt.Errorf("engine: discovered Calendar: %w", err)
		}
		if calendarContainsAvailable(cal, target) {
			now := e.clock.Now()
			if err := e.recordObserved(ctx, intent, now); err != nil {
				e.log.LogAttrs(ctx, slog.LevelWarn, "engine: record observed release failed",
					slog.String(domain.LogKeySnipeID, string(state.ID())),
					slog.String("err", err.Error()),
				)
			}
			return state.Transition(ctx, domain.StatusAwaiting, domain.EventReleased,
				slog.String(domain.LogKeyVenueRef, venue.String()),
				slog.Time("observed_at", now),
			)
		}

		// Not yet released; wait one PollFloor tick and try again.
		if err := waitWithCtx(ctx, e.clock, e.pollInterval()); err != nil {
			return err
		}
	}
}

// pollInterval returns the engine's effective polling interval — the
// configured PollFloor, clamped to the empirical 100ms anti-bot
// minimum.
func (e *Engine) pollInterval() time.Duration {
	return max(e.policy.PollFloor, 100*time.Millisecond)
}

// runContinuousRelease polls Provider.Find at PollFloor cadence until
// either an inventory hit lands or ctx / Until elapses. Any hit
// transitions the snipe to Awaiting (E4 picks up Booking from there).
func (e *Engine) runContinuousRelease(ctx context.Context, state *SnipeState, r domain.ContinuousRelease) error {
	if r.Until.IsZero() {
		return errors.New("engine: ContinuousRelease requires Until")
	}

	intent := state.Intent()
	floor := e.pollInterval()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !e.clock.Now().Before(r.Until) {
			return state.Transition(ctx, domain.StatusExpired, domain.EventExpired,
				slog.String("reason", "continuous_window_expired"),
				slog.Time("until", r.Until),
			)
		}

		req := providers.FindRequest{
			Venue:     intent.Venue,
			Date:      intent.Date,
			PartySize: intent.PartySize,
		}
		slots, err := e.provider.Find(ctx, req)
		if err != nil {
			if errors.Is(err, providers.ErrInventoryEmpty) {
				if waitErr := waitWithCtx(ctx, e.clock, floor); waitErr != nil {
					return waitErr
				}
				continue
			}
			return fmt.Errorf("engine: continuous Find: %w", err)
		}
		if len(slots) > 0 {
			return state.Transition(ctx, domain.StatusAwaiting, domain.EventFound,
				slog.String(domain.LogKeyVenueRef, intent.Venue.String()),
				slog.Int("slot_count", len(slots)),
			)
		}
		if err := waitWithCtx(ctx, e.clock, floor); err != nil {
			return err
		}
	}
}

// advanceToDiscovering moves a Scheduled snipe into Discovering with a
// matching event. Idempotent: if the snipe is already Discovering it
// is a no-op.
func (e *Engine) advanceToDiscovering(ctx context.Context, state *SnipeState, r domain.DiscoveredRelease) error {
	if state.Status() == domain.StatusDiscovering {
		return nil
	}
	return state.Transition(ctx, domain.StatusDiscovering, domain.EventDiscovered,
		slog.String(domain.LogKeyVenueRef, state.Intent().Venue.String()),
		slog.Time("probe_from", r.ProbeFrom),
		slog.Time("probe_until", r.ProbeUntil),
	)
}

// recordObserved writes an observed_release_times row so the next
// snipe for this venue can default to ExplicitRelease at the observed
// time-of-day, days-offset before the target date.
func (e *Engine) recordObserved(ctx context.Context, intent domain.Intent, now time.Time) error {
	venueLoc, err := e.lookupVenueTZ(ctx, intent.Venue)
	if err != nil {
		// We can't write a venue-local time without the TZ. Persist
		// the wall-clock UTC anyway under the local time column;
		// future runs will refresh once the TZ is known.
		venueLoc = time.UTC
	}
	local := now.In(venueLoc)
	wt := domain.NewWallTime(local.Hour(), local.Minute(), local.Second())
	target := intent.Date.In(venueLoc)
	days := int(target.Sub(now).Hours() / 24)
	row := store.ObservedRelease{
		Venue:             intent.Venue,
		ObservedLocalTime: wt,
		DaysOffset:        days,
		ObservedAt:        now,
	}
	return e.store.UpsertObserved(ctx, row)
}

// lookupVenueTZ returns the venue's timezone from the venue cache.
func (e *Engine) lookupVenueTZ(ctx context.Context, ref domain.VenueRef) (*time.Location, error) {
	v, err := e.store.GetVenue(ctx, ref)
	if err != nil {
		return nil, err
	}
	if v.TZ == nil {
		return nil, errors.New("engine: cached venue has nil TZ")
	}
	return v.TZ, nil
}

// calendarContainsAvailable reports whether the calendar lists the
// target date as available.
func calendarContainsAvailable(cal providers.Calendar, target domain.Date) bool {
	for _, d := range cal.Days {
		if d.Date == target && d.Available {
			return true
		}
	}
	return false
}

// waitWithCtx blocks for d as measured by the supplied clock, or
// returns ctx.Err() if ctx is canceled first.
func waitWithCtx(ctx context.Context, c clock.Clock, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-c.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
