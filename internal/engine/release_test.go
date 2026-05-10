package engine_test

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/engine"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/store"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeProvider implements providers.Provider with caller-controlled
// behavior so engine tests can drive each release strategy through
// known sequences.
type fakeProvider struct {
	mu sync.Mutex

	calendarCalls atomic.Int32
	findCalls     atomic.Int32

	calendarFn func(ctx context.Context, ref domain.VenueRef, r providers.DateRange) (providers.Calendar, error)
	findFn     func(ctx context.Context, req providers.FindRequest) ([]providers.Slot, error)
}

func (*fakeProvider) ID() domain.ProviderID { return "resy" }
func (*fakeProvider) Login(context.Context, providers.Credentials) (providers.Session, error) {
	return nil, errors.New("fake: Login not used")
}
func (*fakeProvider) Ping(context.Context, providers.Session) error { return nil }
func (*fakeProvider) SearchVenues(context.Context, providers.Query) ([]domain.Venue, error) {
	return nil, nil
}
func (*fakeProvider) ResolveVenue(context.Context, string, string) (domain.Venue, error) {
	return domain.Venue{}, providers.ErrVenueNotFound
}
func (*fakeProvider) Book(context.Context, providers.Slot, providers.Session) (providers.Confirmation, error) {
	return providers.Confirmation{}, errors.New("fake: Book not used")
}
func (f *fakeProvider) Calendar(ctx context.Context, ref domain.VenueRef, r providers.DateRange) (providers.Calendar, error) {
	f.calendarCalls.Add(1)
	f.mu.Lock()
	fn := f.calendarFn
	f.mu.Unlock()
	if fn == nil {
		return providers.Calendar{}, errors.New("fake: no calendarFn set")
	}
	return fn(ctx, ref, r)
}
func (f *fakeProvider) Find(ctx context.Context, req providers.FindRequest) ([]providers.Slot, error) {
	f.findCalls.Add(1)
	f.mu.Lock()
	fn := f.findFn
	f.mu.Unlock()
	if fn == nil {
		return nil, errors.New("fake: no findFn set")
	}
	return fn(ctx, req)
}

func newReleaseFixture(t *testing.T, fp *fakeProvider) (*engine.Engine, *clock.Fake, store.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), dir+"/release.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := store.NewSQLiteStore(db)
	c := clock.NewFake(time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC))

	opts := []engine.Option{}
	if fp != nil {
		opts = append(opts, engine.WithProvider(fp))
	}
	eng := engine.New(s, c, discardLogger(), opts...)
	return eng, c, s
}

func discoveredIntent(probeFrom, probeUntil time.Time) domain.Intent {
	return domain.Intent{
		User:      "u1",
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
		SlotPrefs: []domain.SlotPreference{{Time: domain.NewWallTime(19, 30, 0)}},
		Release:   domain.DiscoveredRelease{ProbeFrom: probeFrom, ProbeUntil: probeUntil},
	}
}

func continuousIntent(until time.Time) domain.Intent {
	return domain.Intent{
		User:      "u1",
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
		SlotPrefs: []domain.SlotPreference{{Time: domain.NewWallTime(19, 30, 0)}},
		Release:   domain.ContinuousRelease{Until: until},
	}
}

// runFuture is the minimal handle returned by driveRunInBackground:
// a Wait() that blocks until Run returns and reports its error.
type runFuture struct {
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func (f *runFuture) Wait(t *testing.T, bound time.Duration) error {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(bound):
		t.Fatal("Run did not return within bound")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// driveRunInBackground spins Run on a goroutine; callers Wait().
func driveRunInBackground(t *testing.T, eng *engine.Engine, id domain.SnipeID) *runFuture {
	t.Helper()
	f := &runFuture{done: make(chan struct{})}
	go func() {
		err := eng.Run(context.Background(), id)
		f.mu.Lock()
		f.err = err
		f.mu.Unlock()
		close(f.done)
	}()
	return f
}

func TestDiscoveredReleaseFiresOnAvailableDate(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{}

	// Calendar returns sold-out for the first call, then available on
	// the second. The engine should poll twice before transitioning.
	var calls atomic.Int32
	target := domain.NewDate(2026, time.June, 1)
	fp.calendarFn = func(_ context.Context, _ domain.VenueRef, _ providers.DateRange) (providers.Calendar, error) {
		n := calls.Add(1)
		avail := n >= 2
		return providers.Calendar{
			Days: []providers.CalendarDay{{Date: target, Available: avail}},
		}, nil
	}

	eng, fake, s := newReleaseFixture(t, fp)
	probeFrom := fake.Now().Add(time.Second)
	probeUntil := fake.Now().Add(time.Hour)

	// Seed a venue row so observed_release_times can compute a
	// venue-local time.
	if err := s.UpsertVenue(context.Background(), domain.Venue{
		Provider: "resy", Ref: "38660", Name: "Carbone", TZ: time.UTC,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.Submit(context.Background(), "snp_disc", discoveredIntent(probeFrom, probeUntil)); err != nil {
		t.Fatal(err)
	}

	fut := driveRunInBackground(t, eng, "snp_disc")

	// Advance to ProbeFrom so the loop starts.
	advanceToCallCount(t, fake, &fp.calendarCalls, 1)
	// Now advance one PollFloor tick so the second poll fires.
	advanceToCallCount(t, fake, &fp.calendarCalls, 2)

	if err := fut.Wait(t, 2*time.Second); err != nil {
		t.Fatalf("Run err: %v", err)
	}

	// Snipe should be Awaiting; observed_release_times row written.
	loaded, err := s.GetSnipe(context.Background(), "snp_disc")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status() != domain.StatusAwaiting {
		t.Errorf("status: %s want awaiting", loaded.Status())
	}
	obs, err := s.GetObserved(context.Background(),
		domain.VenueRef{Provider: "resy", Ref: "38660"})
	if err != nil {
		t.Fatalf("observed: %v", err)
	}
	if obs.ObservedAt.IsZero() {
		t.Error("observed_at not set")
	}
}

func TestDiscoveredReleaseExpiresWhenWindowCloses(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{}
	target := domain.NewDate(2026, time.June, 1)
	fp.calendarFn = func(_ context.Context, _ domain.VenueRef, _ providers.DateRange) (providers.Calendar, error) {
		return providers.Calendar{
			Days: []providers.CalendarDay{{Date: target, Available: false}},
		}, nil
	}

	eng, fake, s := newReleaseFixture(t, fp)
	probeFrom := fake.Now()
	probeUntil := fake.Now().Add(500 * time.Millisecond)

	if _, err := eng.Submit(context.Background(), "snp_exp", discoveredIntent(probeFrom, probeUntil)); err != nil {
		t.Fatal(err)
	}

	fut := driveRunInBackground(t, eng, "snp_exp")

	// Drive past the window with a few PollFloor ticks.
	for range 6 {
		fake.Advance(100 * time.Millisecond)
		runtime.Gosched()
	}

	if err := fut.Wait(t, 2*time.Second); err != nil {
		t.Fatalf("Run err: %v", err)
	}

	loaded, err := s.GetSnipe(context.Background(), "snp_exp")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status() != domain.StatusFailed {
		t.Errorf("status: %s want failed (window closed)", loaded.Status())
	}
}

func TestContinuousReleaseFiresOnInventoryHit(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{}
	var calls atomic.Int32
	fp.findFn = func(_ context.Context, req providers.FindRequest) ([]providers.Slot, error) {
		n := calls.Add(1)
		if n < 2 {
			return nil, providers.ErrInventoryEmpty
		}
		return []providers.Slot{{
			Venue: req.Venue, Date: req.Date,
			Time:    domain.NewWallTime(19, 30, 0),
			Payload: domain.ResySlotPayload{ConfigID: "cfg-1"},
		}}, nil
	}

	eng, fake, s := newReleaseFixture(t, fp)

	if _, err := eng.Submit(context.Background(), "snp_cont",
		continuousIntent(fake.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}

	fut := driveRunInBackground(t, eng, "snp_cont")

	advanceToCallCount(t, fake, &fp.findCalls, 1)
	advanceToCallCount(t, fake, &fp.findCalls, 2)

	if err := fut.Wait(t, 2*time.Second); err != nil {
		t.Fatalf("Run err: %v", err)
	}

	loaded, err := s.GetSnipe(context.Background(), "snp_cont")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status() != domain.StatusAwaiting {
		t.Errorf("status: %s want awaiting", loaded.Status())
	}
}

func TestContinuousReleaseExpiresWhenWindowCloses(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{}
	fp.findFn = func(_ context.Context, _ providers.FindRequest) ([]providers.Slot, error) {
		return nil, providers.ErrInventoryEmpty
	}

	eng, fake, s := newReleaseFixture(t, fp)
	until := fake.Now().Add(300 * time.Millisecond)
	if _, err := eng.Submit(context.Background(), "snp_cexp", continuousIntent(until)); err != nil {
		t.Fatal(err)
	}

	fut := driveRunInBackground(t, eng, "snp_cexp")

	for range 5 {
		fake.Advance(100 * time.Millisecond)
		runtime.Gosched()
	}

	if err := fut.Wait(t, 2*time.Second); err != nil {
		t.Fatalf("Run err: %v", err)
	}

	loaded, err := s.GetSnipe(context.Background(), "snp_cexp")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status() != domain.StatusExpired {
		t.Errorf("status: %s want expired", loaded.Status())
	}
}

func TestStrategyRequiresProviderForPolling(t *testing.T) {
	t.Parallel()
	// No provider attached.
	eng, fake, _ := newReleaseFixture(t, nil)

	if _, err := eng.Submit(context.Background(), "snp_disc_no_prov",
		discoveredIntent(fake.Now(), fake.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	err := eng.Run(context.Background(), "snp_disc_no_prov")
	if err == nil {
		t.Fatal("expected error without provider")
	}
}

// advanceToCallCount drives the fake clock forward in PollFloor
// increments until the supplied counter reaches `want`, with a fixed
// 5s real-time bound so a buggy loop fails the test instead of
// hanging.
func advanceToCallCount(t *testing.T, fake *clock.Fake, counter *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for counter.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("counter stuck at %d, wanted %d", counter.Load(), want)
		}
		fake.Advance(100 * time.Millisecond)
		runtime.Gosched()
	}
}
