package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/engine"
	"resy-snipe/internal/notify"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/store"
)

// runSnipeFn is the function variable run() uses to drive a snipe.
// Production points it at runSnipe; tests substitute a stub so they
// can exercise run()'s flow control (session load, exit-code mapping,
// signal wiring) without standing up a real engine + provider.
//
// This indirection is the smallest seam that keeps the existing
// session-load tests focused on what they assert (the load semantics)
// while a separate snipe_test.go covers runSnipe end-to-end.
var runSnipeFn = runSnipe

// runSnipe executes the full E7/C4 lifecycle for one CLI invocation:
//
//  1. Construct an Engine wired to the persistence layer and the
//     Provider adapter that wraps *resy.Client.
//  2. Subscribe a bridge that forwards engine.Notification events to
//     the user-facing Notifier (stdout in Phase 1).
//  3. Submit the snipe and call Run, which advances Submitted →
//     Scheduled → … → Awaiting through the chosen release strategy.
//  4. If the snipe lands on Awaiting, hand off to RunBookingRace which
//     drives Awaiting → Finding → Booking → Booked / Failed.
//  5. Emit a terminal Result through the notifier and return a Status
//     so main can pick the right exit code.
//
// The supplied ctx is the cancellation root: SIGINT/SIGTERM cancel it
// and the engine's own context plumbing handles graceful shutdown
// (in-flight Book calls detached, Prepare aborts on cancel).
func runSnipe(
	ctx context.Context,
	intent domain.Intent,
	sess providers.Session,
	s store.Store,
	provider providers.Provider,
	notifier notify.Notifier,
	logger *slog.Logger,
	clk clock.Clock,
) (domain.Status, error) {
	eng := engine.New(s, clk, logger, engine.WithProvider(provider))

	// Bridge engine events → notifier surface. The bridge is the only
	// consumer of the engine's event stream in Phase 1; the Phase 2
	// daemon will plug an additional persistence/observability
	// subscriber here without touching the engine.
	cancelSub := eng.Subscribe(notifierBridge(ctx, notifier))
	defer cancelSub()

	id := newSnipeID(intent)

	// Submit transitions <none> → Submitted and emits EventSubmitted
	// (the bootstrap notification the bridge ignores — there is no
	// from-status to render). The returned state is shadowed by the
	// post-Run reload below; we only care that Submit succeeded.
	if _, err := eng.Submit(ctx, id, intent); err != nil {
		return domain.StatusFailed, fmt.Errorf("snipe submit: %w", err)
	}

	// Drive the release strategy. Run blocks until either Awaiting is
	// reached (release fired) or the snipe lands on a non-Awaiting
	// terminal state (Expired, Failed). We capture but don't surface
	// the error here yet — RunBookingRace's branch picks it up.
	if err := eng.Run(ctx, id); err != nil {
		// Context cancellation during Run means the user pressed
		// Ctrl-C before release fired. Reload to see the persisted
		// final status — the engine guarantees no half-committed
		// transitions, so whatever we read is authoritative.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			final, loadErr := eng.Load(ctx, id)
			if loadErr != nil {
				return domain.StatusFailed, fmt.Errorf("snipe canceled (and reload failed): %w", err)
			}
			notifier.Result(ctx, id, false, "canceled before release", nil)
			return final.Status(), err
		}
		return domain.StatusFailed, fmt.Errorf("snipe run: %w", err)
	}

	// Re-load: Run mutates the engine-side state, but the source of
	// truth is the persisted row (and on resumed runs, in-memory state
	// might be stale). Reload before deciding next step.
	state, err := eng.Load(ctx, id)
	if err != nil {
		return domain.StatusFailed, fmt.Errorf("snipe reload after run: %w", err)
	}

	switch state.Status() {
	case domain.StatusAwaiting:
		// Release fired and a slot is in the inventory window — drive
		// the booking race. Final status is what RunBookingRace lands
		// on; we read it back from the store rather than inferring
		// from RunBookingRace's error so terminal-event ordering is
		// preserved.
		raceErr := eng.RunBookingRace(ctx, state, sess)
		final, loadErr := eng.Load(ctx, id)
		if loadErr != nil {
			return domain.StatusFailed, fmt.Errorf("snipe reload after booking race: %w", loadErr)
		}
		emitTerminalResult(ctx, notifier, id, final, raceErr)
		return final.Status(), raceErr
	case domain.StatusBooked, domain.StatusFailed, domain.StatusExpired, domain.StatusCanceled:
		// Run already settled into a terminal state (Continuous
		// expired, Discovered window closed, etc.). Nothing more to
		// drive — emit the terminal result.
		emitTerminalResult(ctx, notifier, id, state, nil)
		return state.Status(), nil
	default:
		// A non-terminal, non-Awaiting status here would mean Run
		// returned nil but did not reach a state RunBookingRace can
		// handle. Treat as a defensive Failed — the persisted state
		// is left untouched so an operator can inspect.
		return state.Status(), fmt.Errorf("snipe: unexpected post-run status %s", state.Status())
	}
}

// notifierBridge returns a Subscriber that routes engine events to the
// user-facing Notifier. Sub-Awaiting transitions become Notifier.Transition
// calls; the bootstrap Submitted emission (From == nil) is skipped
// because the notifier line "<none> -> submitted" carries no useful
// information. Terminal transitions are NOT routed through Result here
// — the caller emits Result with the booking confirmation message
// after RunBookingRace returns, which has the user-readable detail.
//
// Subscribers must not block (see engine.Subscriber doc). The stdout
// notifier is lock-protected and writes synchronously; that is fast
// enough that in-line delivery is safe. A future buffered transport
// would wrap notifier in a queue here.
func notifierBridge(ctx context.Context, n notify.Notifier) engine.Subscriber {
	return func(note engine.Notification) {
		if note.From == nil {
			return
		}
		// We pass the slog.Attr slice straight through. The notifier
		// is responsible for picking out the keys it wants to render.
		n.Transition(ctx, note.SnipeID, *note.From, note.To, note.Event.Attrs)
	}
}

// emitTerminalResult formats a single Notifier.Result line for the
// snipe's terminal state. The success case carries the confirmation
// id; failure surfaces the Status name plus the wrapped error. We
// pull the confirmation id off the persisted state.Result rather than
// the in-memory race winner so the path is identical for resumed
// runs.
func emitTerminalResult(
	ctx context.Context,
	n notify.Notifier,
	id domain.SnipeID,
	state *engine.SnipeState,
	raceErr error,
) {
	switch {
	case state.Status() == domain.StatusBooked && state.Inner().Result != nil:
		conf := state.Inner().Result
		msg := "confirmation " + conf.ConfirmationID
		n.Result(ctx, id, true, msg, []slog.Attr{
			slog.String("confirmation_id", conf.ConfirmationID),
			slog.Time("booked_at", conf.BookedAt),
		})
	case state.Status() == domain.StatusBooked:
		// Booked but no Result — defensive; should not happen.
		n.Result(ctx, id, true, "booked", nil)
	default:
		msg := state.Status().String()
		var attrs []slog.Attr
		if raceErr != nil {
			msg = msg + ": " + raceErr.Error()
			attrs = append(attrs, slog.String("err", raceErr.Error()))
		}
		n.Result(ctx, id, false, msg, attrs)
	}
}

// newSnipeID derives a stable-prefixed snipe id from the intent hash so
// log scraping can correlate the persisted row to the live invocation
// without a separate UUID dependency. Collisions on the same hash are
// fine — store.CreateSnipe enforces uniqueness and returns an error
// the user sees as "already running".
func newSnipeID(intent domain.Intent) domain.SnipeID {
	h := string(intent.Hash())
	if len(h) > 12 {
		h = h[:12]
	}
	return domain.SnipeID("snp_" + h)
}

// installSignalCancel wraps ctx so SIGINT and SIGTERM cancel it. The
// returned cancel must be called by the caller (defer is the canonical
// site) to release the signal-handler goroutine.
func installSignalCancel(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
}
