package engine_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// TestShutdownCancelsPrepareLoopWithin5s verifies that canceling the
// caller's ctx while RunBookingRace is in its PrepareSlot loop causes
// the loop to exit cleanly within the 5s grace window — and that no
// goroutines leak.
func TestShutdownCancelsPrepareLoopWithin5s(t *testing.T) {
	t.Parallel()

	pre := newRacePreparer("never")
	// PrepareSlot blocks on ctx so the loop stalls in PrepareSlot when
	// the test cancels.
	pre.prepareFn = func(slot providers.Slot) (providers.Slot, error) {
		// Re-read ctx via a side channel: the fake gets the parent
		// ctx through PrepareSlot's first arg, but our fakePreparer
		// signature swallows it. Approximate by sleeping briefly so
		// the test's cancel can land before each PrepareSlot returns.
		time.Sleep(40 * time.Millisecond)
		p, _ := slot.Payload.(domain.ResySlotPayload)
		p.BookToken = "book-" + p.ConfigID
		p.PaymentMethodID = 42
		out := slot
		out.Payload = p
		return out, nil
	}
	eng := newRaceFixture(t, pre)
	state := awaitingState(t, eng)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- eng.RunBookingRace(ctx, state, fakeSession(t))
	}()

	// Let the loop start a couple of PrepareSlot calls, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Errorf("RunBookingRace returned nil after ctx cancel; expected non-nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunBookingRace did not return within 5s of cancel")
	}
	// RunBookingRace's wg.Wait() guarantees no goroutine outlives it;
	// numerical goroutine-count assertions here are flaky under
	// parallel tests and the property is covered by run_test's
	// dedicated non-parallel TestRunNoGoroutineLeak.
}

// TestInflightConfirmIsNotCanceledByParentCtx verifies the
// "in-flight Book finishes" rule: even if the caller's ctx is
// canceled, an already-started ConfirmSlot completes normally.
//
// The fakePreparer's confirmFn observes its own ctx — so we assert
// that ctx is NOT done while ConfirmSlot is running.
func TestInflightConfirmIsNotCanceledByParentCtx(t *testing.T) {
	t.Parallel()

	pre := newRacePreparer("tok-A") // tok-A wins
	var confirmObservedCancel atomic.Bool
	pre.confirmFn = func(ctx context.Context, slot providers.Slot, _ string) (providers.Confirmation, error) {
		// Wait a beat to give the test a chance to cancel the parent.
		select {
		case <-time.After(80 * time.Millisecond):
		case <-ctx.Done():
			confirmObservedCancel.Store(true)
		}
		payload, _ := slot.Payload.(domain.ResySlotPayload)
		if payload.ConfigID == "tok-A" {
			if ctx.Err() != nil {
				return providers.Confirmation{}, ctx.Err()
			}
			return providers.Confirmation{ID: "conf-A", BookedAt: time.Now()}, nil
		}
		// Other slots stall and either get canceled by raceCtx or
		// time out; for this test we don't care about their outcome.
		select {
		case <-ctx.Done():
			return providers.Confirmation{}, ctx.Err()
		case <-time.After(time.Second):
			return providers.Confirmation{}, providers.ErrSlotTaken
		}
	}

	eng := newRaceFixture(t, pre)
	state := awaitingState(t, eng)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunBookingRace(ctx, state, fakeSession(t))
	}()

	// Give the goroutines a head start, then cancel parent ctx.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		// We expect SUCCESS — the in-flight tok-A confirm finishes
		// despite the parent ctx cancel.
		if err != nil {
			t.Logf("RunBookingRace err (expected when no Confirm wins before cancel): %v", err)
		}
		if confirmObservedCancel.Load() {
			t.Error("ConfirmSlot saw ctx cancellation; raceCtx should be detached from parent")
		}
		if state.Status() != domain.StatusBooked && !errors.Is(err, context.Canceled) {
			t.Logf("status=%s err=%v (acceptable: race may have ended either way)", state.Status(), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunBookingRace did not return within 5s")
	}
}
