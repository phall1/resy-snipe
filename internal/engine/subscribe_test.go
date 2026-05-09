package engine_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/engine"
)

// counters is a small fixed-size atomic.Int64 array shared by the
// fan-out test; using atomic.Int64 instead of int64+atomic.AddInt64 is
// the project's preferred form (modernize linter).
type counters [5]atomic.Int64

// recordingSubscriber is a thread-safe Notification capture that
// satisfies engine.Subscriber. Tests assert against the captured slice
// once the run loop has settled.
type recordingSubscriber struct {
	mu    sync.Mutex
	notes []engine.Notification
}

func (r *recordingSubscriber) sub(n engine.Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, n)
}

func (r *recordingSubscriber) snapshot() []engine.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]engine.Notification, len(r.notes))
	copy(out, r.notes)
	return out
}

// TestSubscribeReceivesSubmittedAndTransitions exercises the full
// emission set on a single Run: Submitted (no From), Scheduled
// (Submitted->Scheduled), Released (Scheduled->Awaiting). The
// recorded order must match the persisted event order.
func TestSubscribeReceivesSubmittedAndTransitions(t *testing.T) {
	t.Parallel()
	eng, fake, _ := newEngineFixture(t)
	ctx := context.Background()

	rec := &recordingSubscriber{}
	cancel := eng.Subscribe(rec.sub)
	defer cancel()

	releaseAt := fake.Now().Add(50 * time.Millisecond)
	if _, err := eng.Submit(ctx, "snp_sub", runIntent(releaseAt)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	errCh := runRunInBackground(eng, ctx, "snp_sub")
	waitUntil(t, func() bool {
		for _, n := range rec.snapshot() {
			if n.To == domain.StatusScheduled {
				return true
			}
		}
		return false
	}, "Scheduled emission never delivered")

	fake.Advance(50 * time.Millisecond)
	if err := waitFor(t, errCh, "release fire"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := rec.snapshot()
	if len(got) != 3 {
		t.Fatalf("notification count = %d, want 3 (submitted, scheduled, released): %+v", len(got), got)
	}

	// 1. Submitted is the bootstrap emission — From must be nil so
	//    consumers can distinguish creation from a real transition.
	if got[0].From != nil {
		t.Errorf("submitted From = %v, want nil", *got[0].From)
	}
	if got[0].To != domain.StatusSubmitted {
		t.Errorf("submitted To = %s, want submitted", got[0].To)
	}
	if got[0].Event.Type != domain.EventSubmitted {
		t.Errorf("submitted Event.Type = %s, want submitted", got[0].Event.Type)
	}
	if got[0].SnipeID != "snp_sub" {
		t.Errorf("submitted SnipeID = %s, want snp_sub", got[0].SnipeID)
	}

	// 2. Scheduled — From=Submitted, To=Scheduled, EventScheduled.
	if got[1].From == nil || *got[1].From != domain.StatusSubmitted {
		t.Errorf("scheduled From = %v, want submitted", got[1].From)
	}
	if got[1].To != domain.StatusScheduled {
		t.Errorf("scheduled To = %s, want scheduled", got[1].To)
	}
	if got[1].Event.Type != domain.EventScheduled {
		t.Errorf("scheduled Event.Type = %s, want scheduled", got[1].Event.Type)
	}

	// 3. Released — From=Scheduled, To=Awaiting, EventReleased.
	if got[2].From == nil || *got[2].From != domain.StatusScheduled {
		t.Errorf("released From = %v, want scheduled", got[2].From)
	}
	if got[2].To != domain.StatusAwaiting {
		t.Errorf("released To = %s, want awaiting", got[2].To)
	}
	if got[2].Event.Type != domain.EventReleased {
		t.Errorf("released Event.Type = %s, want released", got[2].Event.Type)
	}
}

// TestSubscribeCancelStopsDeliveries verifies that the cancel
// function returned by Subscribe removes the subscriber and is
// idempotent.
func TestSubscribeCancelStopsDeliveries(t *testing.T) {
	t.Parallel()
	eng, _, _ := newEngineFixture(t)
	ctx := context.Background()

	rec := &recordingSubscriber{}
	cancel := eng.Subscribe(rec.sub)

	if _, err := eng.Submit(ctx, "snp_a", sampleEngineIntent()); err != nil {
		t.Fatalf("Submit a: %v", err)
	}
	cancel()
	cancel() // idempotent

	if _, err := eng.Submit(ctx, "snp_b", sampleEngineIntent()); err != nil {
		t.Fatalf("Submit b: %v", err)
	}

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("got %d notifications after cancel, want 1: %+v", len(got), got)
	}
	if got[0].SnipeID != "snp_a" {
		t.Errorf("notified for %s, want snp_a", got[0].SnipeID)
	}
}

// TestSubscribeMultipleFanOut verifies every registered subscriber
// receives every emission, in registration order.
func TestSubscribeMultipleFanOut(t *testing.T) {
	t.Parallel()
	eng, _, _ := newEngineFixture(t)
	ctx := context.Background()

	const subscribers = 5
	var counts counters
	cancels := make([]func(), subscribers)
	var orderMu sync.Mutex
	var order []int

	for i := range subscribers {
		cancels[i] = eng.Subscribe(func(_ engine.Notification) {
			counts[i].Add(1)
			orderMu.Lock()
			order = append(order, i)
			orderMu.Unlock()
		})
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	if _, err := eng.Submit(ctx, "snp_fan", sampleEngineIntent()); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	for i := range counts {
		if got := counts[i].Load(); got != 1 {
			t.Errorf("subscriber %d count = %d, want 1", i, got)
		}
	}

	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != subscribers {
		t.Fatalf("delivery list length = %d, want %d", len(order), subscribers)
	}
	for i, idx := range order {
		if idx != i {
			t.Errorf("delivery order[%d] = %d, want %d (subscribers must fire in registration order)", i, idx, i)
		}
	}
}

// TestSubscribePanicIsContained confirms a panicking subscriber does
// not unwind the engine's emit path or starve later subscribers.
func TestSubscribePanicIsContained(t *testing.T) {
	t.Parallel()
	eng, _, _ := newEngineFixture(t)
	ctx := context.Background()

	var laterCalled atomic.Int64
	cancel1 := eng.Subscribe(func(_ engine.Notification) {
		panic("intentional test panic")
	})
	defer cancel1()
	cancel2 := eng.Subscribe(func(_ engine.Notification) {
		laterCalled.Add(1)
	})
	defer cancel2()

	// Must not panic out of Submit even though the first subscriber
	// always panics.
	if _, err := eng.Submit(ctx, "snp_panic", sampleEngineIntent()); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if got := laterCalled.Load(); got != 1 {
		t.Errorf("downstream subscriber call count = %d, want 1", got)
	}
}

// TestSubscribeNilPanics fails fast on a nil subscriber — silently
// dropping events would mask a wiring bug at boot.
func TestSubscribeNilPanics(t *testing.T) {
	t.Parallel()
	eng, _, _ := newEngineFixture(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("Subscribe(nil) did not panic")
		}
	}()
	_ = eng.Subscribe(nil)
}
