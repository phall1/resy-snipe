package engine_test

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/engine"
	"resy-snipe/internal/store"
)

// runIntent returns a sample intent whose ExplicitRelease fires `delay`
// after the fake clock's current time. The fake fixture starts at
// 2026-05-01T09:00:00Z (see newEngineFixture), so callers should think
// of `delay` as "from that origin" when crafting expectations.
func runIntent(releaseAt time.Time) domain.Intent {
	return domain.Intent{
		User:      "u1",
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
		SlotPrefs: []domain.SlotPreference{
			{Time: domain.NewWallTime(19, 30, 0)},
		},
		Release: domain.ExplicitRelease{At: releaseAt},
	}
}

// goroutineCountStable returns the number of goroutines once it has
// stayed the same for two consecutive samples (or after timeout).
// Plain runtime.NumGoroutine() is racy because exiting goroutines do
// not unwind synchronously; this gives the scheduler a brief moment
// to drain.
func goroutineCountStable(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	prev := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		cur := runtime.NumGoroutine()
		if cur == prev {
			return cur
		}
		prev = cur
	}
	return prev
}

// runRunInBackground starts eng.Run in a goroutine and returns a
// channel that yields its error exactly once. Tests use this to drive
// the fake clock from the foreground while Run waits for AfterFunc to
// fire.
func runRunInBackground(eng *engine.Engine, ctx context.Context, id domain.SnipeID) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- eng.Run(ctx, id)
	}()
	return errCh
}

// waitFor reads from errCh with a real-time bound so a hung Run never
// pegs the test runner. The fake clock drives logical time; this wall
// timeout is purely a test-harness backstop.
func waitFor(t *testing.T, errCh <-chan error, why string) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return: %s", why)
		return nil
	}
}

func TestRunSchedulesAndFiresOnAdvance(t *testing.T) {
	t.Parallel()
	eng, fake, s := newEngineFixture(t)
	ctx := context.Background()

	releaseAt := fake.Now().Add(100 * time.Millisecond)
	if _, err := eng.Submit(ctx, "snp_run", runIntent(releaseAt)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	errCh := runRunInBackground(eng, ctx, "snp_run")

	// Give Run a moment to load, transition Submitted -> Scheduled,
	// and register the AfterFunc before we move time forward.
	waitUntil(t, func() bool {
		got, err := s.GetSnipe(ctx, "snp_run")
		return err == nil && got.Status() == domain.StatusScheduled
	}, "snipe never reached Scheduled")

	fake.Advance(100 * time.Millisecond)

	if err := waitFor(t, errCh, "after Advance past release"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := s.GetSnipe(ctx, "snp_run")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != domain.StatusAwaiting {
		t.Errorf("status: %s want awaiting", got.Status())
	}

	events, err := s.ListEventsBySnipe(ctx, "snp_run")
	if err != nil {
		t.Fatal(err)
	}
	if !hasEventOfType(events, domain.EventScheduled) {
		t.Errorf("expected EventScheduled, got %+v", eventTypes(events))
	}
	if !hasEventOfType(events, domain.EventReleased) {
		t.Errorf("expected EventReleased, got %+v", eventTypes(events))
	}
}

func TestRunCtxCancelBeforeFireSkipsWorkflow(t *testing.T) {
	t.Parallel()
	eng, fake, s := newEngineFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	releaseAt := fake.Now().Add(time.Second)
	if _, err := eng.Submit(ctx, "snp_cancel", runIntent(releaseAt)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	errCh := runRunInBackground(eng, ctx, "snp_cancel")

	// Wait for the snipe to land on Scheduled before canceling so we
	// know the AfterFunc has been registered.
	waitUntil(t, func() bool {
		got, err := s.GetSnipe(context.Background(), "snp_cancel")
		return err == nil && got.Status() == domain.StatusScheduled
	}, "snipe never reached Scheduled")

	cancel()

	err := waitFor(t, errCh, "after ctx cancel")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}

	// Advancing past the release time after cancel must NOT trigger a
	// transition — the timer was stopped before fire.
	fake.Advance(2 * time.Second)
	// Give any (incorrectly-running) callback a chance to land before
	// we assert.
	time.Sleep(20 * time.Millisecond)

	got, err := s.GetSnipe(context.Background(), "snp_cancel")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != domain.StatusScheduled {
		t.Errorf("status: %s want scheduled (cancel must skip Awaiting)", got.Status())
	}
	events, err := s.ListEventsBySnipe(context.Background(), "snp_cancel")
	if err != nil {
		t.Fatal(err)
	}
	if hasEventOfType(events, domain.EventReleased) {
		t.Errorf("EventReleased emitted despite ctx cancel: %+v", eventTypes(events))
	}
}

func TestRunPastScheduledAtFiresImmediately(t *testing.T) {
	t.Parallel()
	eng, fake, s := newEngineFixture(t)
	ctx := context.Background()

	// Release time already in the past relative to the fake clock.
	releaseAt := fake.Now().Add(-time.Hour)
	if _, err := eng.Submit(ctx, "snp_past", runIntent(releaseAt)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Must fire without any Advance — no timer wait at all.
	if err := eng.Run(ctx, "snp_past"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := s.GetSnipe(ctx, "snp_past")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != domain.StatusAwaiting {
		t.Errorf("status: %s want awaiting", got.Status())
	}
	events, err := s.ListEventsBySnipe(ctx, "snp_past")
	if err != nil {
		t.Fatal(err)
	}
	if !hasEventOfType(events, domain.EventReleased) {
		t.Errorf("expected EventReleased, got %+v", eventTypes(events))
	}
}

func TestRunReturnsErrorForUnsupportedReleaseStrategy(t *testing.T) {
	t.Parallel()
	eng, fake, _ := newEngineFixture(t)
	ctx := context.Background()

	intent := runIntent(time.Time{})
	intent.Release = domain.DiscoveredRelease{
		ProbeFrom:  fake.Now(),
		ProbeUntil: fake.Now().Add(time.Hour),
	}
	if _, err := eng.Submit(ctx, "snp_disco", intent); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if err := eng.Run(ctx, "snp_disco"); err == nil {
		t.Fatal("Run on DiscoveredRelease must error in Phase 1")
	}
}

func TestRunNoGoroutineLeak(t *testing.T) { //nolint:paralleltest // goroutine counts are global; parallel runs would bias the snapshot.
	// NOT t.Parallel: goroutine counts are global; running parallel
	// would let other tests' goroutines bias the snapshot.
	eng, fake, _ := newEngineFixture(t)
	ctx := context.Background()

	before := goroutineCountStable(t)

	releaseAt := fake.Now().Add(50 * time.Millisecond)
	if _, err := eng.Submit(ctx, "snp_leak", runIntent(releaseAt)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	errCh := runRunInBackground(eng, ctx, "snp_leak")
	// Give Run time to schedule before advancing.
	time.Sleep(20 * time.Millisecond)
	fake.Advance(50 * time.Millisecond)
	if err := waitFor(t, errCh, "fire path"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	after := goroutineCountStable(t)
	if after > before {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}
}

func TestRunMissingSnipeReturnsNotFound(t *testing.T) {
	t.Parallel()
	eng, _, _ := newEngineFixture(t)
	err := eng.Run(context.Background(), "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Run err = %v, want ErrNotFound", err)
	}
}

// TestRunSmokeRealClock exercises Run end-to-end against the real
// clock with a 50ms release. It is intentionally short so flakes are
// noisy rather than time-pressured.
func TestRunSmokeRealClock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), dir+"/smoke.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := store.NewSQLiteStore(db)
	c := clock.NewReal()
	log := slog.New(slog.DiscardHandler)
	eng := engine.New(s, c, log)

	ctx := context.Background()
	releaseAt := c.Now().Add(50 * time.Millisecond)
	if _, err := eng.Submit(ctx, "snp_smoke", runIntent(releaseAt)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	start := time.Now()
	if err := eng.Run(ctx, "snp_smoke"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)
	// Allow 40ms slack on the lower bound — different OSes round
	// monotonic-clock samples differently and the wall-clock can
	// shave a few ms off the scheduled 50ms wake.
	if elapsed < 40*time.Millisecond {
		t.Errorf("Run returned in %s, expected at least ~50ms", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run returned in %s, expected well under 2s", elapsed)
	}
}

// --- helpers --------------------------------------------------------

// waitUntil polls cond up to a real-time bound. Used to synchronize
// between the foreground test (which drives the fake clock) and the
// background Run goroutine (which has just persisted a status change).
func waitUntil(t *testing.T, cond func() bool, why string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waitUntil timeout: %s", why)
}

func hasEventOfType(events []domain.Event, t domain.EventType) bool {
	for _, e := range events {
		if e.Type == t {
			return true
		}
	}
	return false
}

func eventTypes(events []domain.Event) []domain.EventType {
	out := make([]domain.EventType, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}
