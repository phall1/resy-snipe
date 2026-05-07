package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// SlotPreparer is the optional Provider extension that lets the
// engine split /3/details from /3/book so it can serialize the former
// and parallelize the latter (E4 race-and-cancel). Resy's adapter
// implements this; cross-provider stubs that don't can fall back to
// Provider.Book sequential.
type SlotPreparer interface {
	PrepareSlot(ctx context.Context, slot providers.Slot, sess providers.Session) (providers.Slot, error)
	ConfirmSlot(ctx context.Context, slot providers.Slot, sess providers.Session, idempotencyKey string) (providers.Confirmation, error)
}

// RunBookingRace orchestrates the booking phase: serialize PrepareSlot
// across the candidate slots in priority order; for each one whose
// preparation succeeds, fire ConfirmSlot in a goroutine racing under
// a shared ctx. The first successful Confirm cancels its siblings via
// the shared ctx; the function then waits for all goroutines to exit
// before returning. No goroutines outlive RunBookingRace.
//
// The engine calls this with a SnipeState already in StatusAwaiting
// (post-release) and a Session for the user. RunBookingRace handles
// the Awaiting -> Finding -> Booking -> Booked / Failed transitions.
func (e *Engine) RunBookingRace(ctx context.Context, state *SnipeState, sess providers.Session) error {
	if e.provider == nil {
		return errors.New("engine.RunBookingRace: requires a Provider")
	}
	preparer, ok := e.provider.(SlotPreparer)
	if !ok {
		return errors.New("engine.RunBookingRace: provider does not implement SlotPreparer")
	}

	// Awaiting -> Finding.
	if err := state.Transition(ctx, domain.StatusFinding, domain.EventFound,
		slog.String(domain.LogKeyVenueRef, state.Intent().Venue.String()),
	); err != nil {
		return fmt.Errorf("engine.RunBookingRace: enter Finding: %w", err)
	}

	intent := state.Intent()
	slots, err := e.provider.Find(ctx, providers.FindRequest{
		Venue:     intent.Venue,
		Date:      intent.Date,
		PartySize: intent.PartySize,
		Times:     slotPrefTimes(intent.SlotPrefs),
		Session:   sess,
	})
	if err != nil {
		return e.failWithReason(ctx, state, "find_failed", err)
	}
	if len(slots) == 0 {
		return e.failWithReason(ctx, state, "inventory_empty", providers.ErrInventoryEmpty)
	}

	ranked := rankSlotsByPreference(slots, intent.SlotPrefs)

	// Finding -> Booking.
	if err := state.Transition(ctx, domain.StatusBooking, domain.EventBookAttempted,
		slog.String(domain.LogKeyVenueRef, intent.Venue.String()),
		slog.Int("slot_count", len(ranked)),
	); err != nil {
		return fmt.Errorf("engine.RunBookingRace: enter Booking: %w", err)
	}

	// Race begins.
	//
	// We derive raceCtx from a *detached* parent — context.WithoutCancel
	// strips ctx's Done channel — so an external SIGTERM cancellation
	// of ctx does not cancel an already-started /3/book attempt
	// (bailing mid-POST risks a confirmed-but-orphaned reservation).
	// The winner's cancel() still propagates to siblings via raceCtx
	// itself, so race-and-cancel correctness is preserved.
	//
	// PrepareSlot below intentionally still uses ctx (the cancellable
	// parent) — we DO want to bail before kicking off any new
	// /3/details when shutdown is signaled.
	raceCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()

	results := make(chan *providers.Confirmation, len(ranked))
	var wg sync.WaitGroup
	intentHash := intent.Hash()

	for i, slot := range ranked {
		if ctx.Err() != nil || raceCtx.Err() != nil {
			break
		}
		// PrepareSlot uses the parent ctx (SIGTERM-cancellable). We do
		// not want to kick off a new /3/details fetch after shutdown
		// is signaled.
		prepared, err := preparer.PrepareSlot(ctx, slot, sess)
		if err != nil {
			e.log.LogAttrs(ctx, slog.LevelWarn, "engine.RunBookingRace: PrepareSlot failed",
				slog.String(domain.LogKeySnipeID, string(state.ID())),
				slog.Int(domain.LogKeyAttempt, i+1),
				slog.String("err", err.Error()),
			)
			if errors.Is(err, providers.ErrRateLimited) ||
				errors.Is(err, providers.ErrAntiBotChallenge) {
				return e.failWithReason(ctx, state, "prepare_blocked", err)
			}
			continue
		}

		wg.Add(1)
		go func(idx int, s providers.Slot) {
			defer wg.Done()
			key := domain.IdempotencyKey(intentHash, s.Payload, idx+1)
			conf, err := preparer.ConfirmSlot(raceCtx, s, sess, key)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					e.log.LogAttrs(raceCtx, slog.LevelInfo, "engine.RunBookingRace: ConfirmSlot failed",
						slog.String(domain.LogKeySnipeID, string(state.ID())),
						slog.Int(domain.LogKeyAttempt, idx+1),
						slog.String("err", err.Error()),
					)
				}
				return
			}
			select {
			case results <- &conf:
				cancel()
			case <-raceCtx.Done():
			}
		}(i, prepared)

		// Honor the inter-call PollFloor between PrepareSlot calls so
		// /3/details fetches don't tail-gate (anti-bot floor). The
		// parent ctx is the cancellable one here too.
		if i < len(ranked)-1 {
			if err := waitWithCtx(ctx, e.clock, e.pollInterval()); err != nil {
				break
			}
		}
	}

	wg.Wait()
	close(results)

	winner := <-results
	if winner == nil {
		return e.failWithReason(ctx, state, "all_attempts_failed", errors.New("no slot booked"))
	}

	// Booking -> Booked.
	state.Inner().Result = &domain.Result{
		BookedAt:       winner.BookedAt,
		ConfirmationID: winner.ID,
	}
	return state.Transition(ctx, domain.StatusBooked, domain.EventBooked,
		slog.String(domain.LogKeyVenueRef, intent.Venue.String()),
		slog.String("confirmation_id", winner.ID),
	)
}

// failWithReason transitions the snipe to Failed with a reason
// attribute and the underlying error wrapped.
func (e *Engine) failWithReason(ctx context.Context, state *SnipeState, reason string, cause error) error {
	if transitionErr := state.Transition(ctx, domain.StatusFailed, domain.EventFailed,
		slog.String("reason", reason),
		slog.String("err", cause.Error()),
	); transitionErr != nil {
		return fmt.Errorf("engine.RunBookingRace: %s (and transition to failed: %w): %w",
			reason, transitionErr, cause)
	}
	return fmt.Errorf("engine.RunBookingRace: %s: %w", reason, cause)
}

// rankSlotsByPreference orders slots so the ones whose (Time,
// TableType) match the user's SlotPreferences are tried first, in
// order. Unranked slots come last in their original Find order.
func rankSlotsByPreference(slots []providers.Slot, prefs []domain.SlotPreference) []providers.Slot {
	if len(prefs) == 0 {
		return slots
	}
	priority := func(s providers.Slot) int {
		for i, p := range prefs {
			if p.Time == s.Time && (p.TableType == "" || p.TableType == s.TableType) {
				return i
			}
		}
		return len(prefs) // unranked
	}
	out := make([]providers.Slot, len(slots))
	copy(out, slots)
	sort.SliceStable(out, func(i, j int) bool {
		return priority(out[i]) < priority(out[j])
	})
	return out
}

// slotPrefTimes pulls the time-of-day hints out of the user's
// SlotPreferences for the FindRequest.Times hint.
func slotPrefTimes(prefs []domain.SlotPreference) []domain.WallTime {
	out := make([]domain.WallTime, 0, len(prefs))
	seen := make(map[domain.WallTime]struct{}, len(prefs))
	for _, p := range prefs {
		if _, dup := seen[p.Time]; dup {
			continue
		}
		seen[p.Time] = struct{}{}
		out = append(out, p.Time)
	}
	return out
}
