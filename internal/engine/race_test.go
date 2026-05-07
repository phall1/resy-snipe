package engine_test

import (
	"context"
	"errors"
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

// fakePreparer extends fakeProvider with the SlotPreparer surface so
// race-and-cancel tests can drive PrepareSlot / ConfirmSlot independently.
type fakePreparer struct {
	*fakeProvider

	mu sync.Mutex

	prepareCalls atomic.Int32
	confirmCalls atomic.Int32

	// orderedPrepareTokens lists the order PrepareSlot was actually
	// called in (by ConfigID) so tests can assert serialization.
	orderedPrepareTokens []string
	orderedConfirmTokens []string

	prepareFn func(slot providers.Slot) (providers.Slot, error)
	confirmFn func(ctx context.Context, slot providers.Slot, idempotencyKey string) (providers.Confirmation, error)
}

func (f *fakePreparer) PrepareSlot(_ context.Context, slot providers.Slot, _ providers.Session) (providers.Slot, error) {
	f.prepareCalls.Add(1)
	payload, _ := slot.Payload.(domain.ResySlotPayload)
	f.mu.Lock()
	f.orderedPrepareTokens = append(f.orderedPrepareTokens, payload.ConfigID)
	fn := f.prepareFn
	f.mu.Unlock()
	if fn == nil {
		return slot, nil
	}
	return fn(slot)
}

func (f *fakePreparer) ConfirmSlot(ctx context.Context, slot providers.Slot, _ providers.Session, idempotencyKey string) (providers.Confirmation, error) {
	f.confirmCalls.Add(1)
	payload, _ := slot.Payload.(domain.ResySlotPayload)
	f.mu.Lock()
	f.orderedConfirmTokens = append(f.orderedConfirmTokens, payload.ConfigID)
	fn := f.confirmFn
	f.mu.Unlock()
	if fn == nil {
		return providers.Confirmation{}, errors.New("fake: no confirmFn set")
	}
	_ = idempotencyKey
	return fn(ctx, slot, idempotencyKey)
}

func newRaceFixture(t *testing.T, fp *fakePreparer) *engine.Engine {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), dir+"/race.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := store.NewSQLiteStore(db)
	// Race tests use a real clock because the goroutine race-and-cancel
	// path is genuinely concurrent — driving it with a fake clock would
	// require us to step every wait inline, which defeats the test.
	// We compensate by using a tight PollFloor so the test finishes
	// quickly.
	c := clock.NewReal()
	eng := engine.New(s, c, discardLogger(),
		engine.WithProvider(fp),
		engine.WithBookingPolicy(domain.BookingPolicy{
			PollFloor:     time.Millisecond, // tight; tests don't care about anti-bot floor
			MaxConcurrent: 4,
		}),
	)
	return eng
}

// newRacePreparer builds a fakePreparer whose ConfirmSlot returns a
// success on the slot whose ConfigID matches winningID, and fails
// (with ErrSlotTaken) for everyone else.
func newRacePreparer(winningID string) *fakePreparer {
	fp := &fakeProvider{}
	fp.findFn = func(_ context.Context, req providers.FindRequest) ([]providers.Slot, error) {
		return []providers.Slot{
			fakeSlot(req, "tok-A", domain.NewWallTime(19, 0, 0)),
			fakeSlot(req, "tok-B", domain.NewWallTime(19, 30, 0)),
			fakeSlot(req, "tok-C", domain.NewWallTime(20, 0, 0)),
		}, nil
	}
	pre := &fakePreparer{fakeProvider: fp}
	pre.prepareFn = func(slot providers.Slot) (providers.Slot, error) {
		p, _ := slot.Payload.(domain.ResySlotPayload)
		p.BookToken = "book-" + p.ConfigID
		p.PaymentMethodID = 42
		out := slot
		out.Payload = p
		return out, nil
	}
	pre.confirmFn = func(ctx context.Context, slot providers.Slot, _ string) (providers.Confirmation, error) {
		payload, _ := slot.Payload.(domain.ResySlotPayload)
		// Honor cancellation — race-and-cancel correctness.
		if ctx.Err() != nil {
			return providers.Confirmation{}, ctx.Err()
		}
		if payload.ConfigID == winningID {
			return providers.Confirmation{
				ID:       "conf-" + payload.ConfigID,
				BookedAt: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
			}, nil
		}
		// Simulate a slow lose so the winner has time to cancel.
		select {
		case <-ctx.Done():
			return providers.Confirmation{}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		return providers.Confirmation{}, providers.ErrSlotTaken
	}
	return pre
}

func fakeSlot(req providers.FindRequest, configID string, wt domain.WallTime) providers.Slot {
	return providers.Slot{
		Venue:     req.Venue,
		Date:      req.Date,
		Time:      wt,
		PartySize: req.PartySize,
		Payload: domain.ResySlotPayload{
			ConfigID:   configID,
			TemplateID: "tmpl",
		},
	}
}

func raceIntent() domain.Intent {
	return domain.Intent{
		User:      "u1",
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
		SlotPrefs: []domain.SlotPreference{
			{Time: domain.NewWallTime(19, 30, 0)}, // tok-B should be first-priority
			{Time: domain.NewWallTime(19, 0, 0)},  // tok-A second
			{Time: domain.NewWallTime(20, 0, 0)},  // tok-C third
		},
		Release: domain.ExplicitRelease{At: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)},
	}
}

// awaitingState seeds a SnipeState in StatusAwaiting (post-release)
// so RunBookingRace's transitions are legal.
func awaitingState(t *testing.T, eng *engine.Engine) *engine.SnipeState {
	t.Helper()
	s, err := eng.Submit(context.Background(), domain.SnipeID("snp_race_"+t.Name()), raceIntent())
	if err != nil {
		t.Fatal(err)
	}
	walk := []domain.Status{domain.StatusScheduled, domain.StatusAwaiting}
	for _, to := range walk {
		var ev domain.EventType
		switch to {
		case domain.StatusScheduled:
			ev = domain.EventScheduled
		case domain.StatusAwaiting:
			ev = domain.EventReleased
		case domain.StatusSubmitted, domain.StatusDiscovering, domain.StatusFinding,
			domain.StatusBooking, domain.StatusBooked, domain.StatusFailed,
			domain.StatusCanceled, domain.StatusExpired:
			t.Fatalf("unexpected walk target %s", to)
		}
		if err := s.Transition(context.Background(), to, ev); err != nil {
			t.Fatalf("seed walk to %s: %v", to, err)
		}
	}
	return s
}

func TestRunBookingRace_FirstSuccessWins(t *testing.T) {
	t.Parallel()
	pre := newRacePreparer("tok-B") // user's first preference
	eng := newRaceFixture(t, pre)
	state := awaitingState(t, eng)

	if err := eng.RunBookingRace(context.Background(), state, fakeSession(t)); err != nil {
		t.Fatalf("RunBookingRace: %v", err)
	}

	if state.Status() != domain.StatusBooked {
		t.Fatalf("status = %s, want booked", state.Status())
	}
	if state.Inner().Result == nil || state.Inner().Result.ConfirmationID != "conf-tok-B" {
		t.Errorf("result wrong: %+v", state.Inner().Result)
	}

	// Serial PrepareSlot — exactly len(ordered) preparations attempted in
	// priority order. Even if confirm-tok-B winning cancels the rest,
	// we should see at least the first PrepareSlot call.
	pre.mu.Lock()
	pTokens := append([]string{}, pre.orderedPrepareTokens...)
	pre.mu.Unlock()
	if len(pTokens) == 0 || pTokens[0] != "tok-B" {
		t.Errorf("first PrepareSlot was not the highest-priority: %v", pTokens)
	}
	// Goroutine-leak verification lives in TestRunNoGoroutineLeak
	// (non-parallel) where a NumGoroutine snapshot isn't biased by
	// sibling tests' background work.
}

func TestRunBookingRace_FallthroughOnPrepareFailure(t *testing.T) {
	t.Parallel()
	pre := newRacePreparer("tok-C") // C wins
	pre.prepareFn = func(slot providers.Slot) (providers.Slot, error) {
		p, _ := slot.Payload.(domain.ResySlotPayload)
		// First slot's prepare fails (book_token expired-equivalent),
		// the rest succeed.
		if p.ConfigID == "tok-B" {
			return providers.Slot{}, providers.ErrBookTokenExpired
		}
		p.BookToken = "book-" + p.ConfigID
		p.PaymentMethodID = 42
		out := slot
		out.Payload = p
		return out, nil
	}

	eng := newRaceFixture(t, pre)
	state := awaitingState(t, eng)

	if err := eng.RunBookingRace(context.Background(), state, fakeSession(t)); err != nil {
		t.Fatalf("RunBookingRace: %v", err)
	}
	if state.Status() != domain.StatusBooked {
		t.Errorf("status = %s, want booked", state.Status())
	}
	// Confirms should not have been attempted on tok-B (its prepare failed).
	pre.mu.Lock()
	confirmTokens := append([]string{}, pre.orderedConfirmTokens...)
	pre.mu.Unlock()
	for _, ct := range confirmTokens {
		if ct == "tok-B" {
			t.Errorf("ConfirmSlot was attempted on tok-B despite PrepareSlot failure: %v", confirmTokens)
		}
	}
}

func TestRunBookingRace_AllConfirmsFailed_TransitionsToFailed(t *testing.T) {
	t.Parallel()
	pre := newRacePreparer("never") // no slot wins
	eng := newRaceFixture(t, pre)
	state := awaitingState(t, eng)

	err := eng.RunBookingRace(context.Background(), state, fakeSession(t))
	if err == nil {
		t.Fatal("expected error when no slot books")
	}
	if state.Status() != domain.StatusFailed {
		t.Errorf("status = %s, want failed", state.Status())
	}
}

func TestRunBookingRace_PrepareBlockedFailsImmediately(t *testing.T) {
	t.Parallel()
	pre := newRacePreparer("tok-B")
	pre.prepareFn = func(_ providers.Slot) (providers.Slot, error) {
		return providers.Slot{}, providers.ErrAntiBotChallenge
	}
	eng := newRaceFixture(t, pre)
	state := awaitingState(t, eng)

	if err := eng.RunBookingRace(context.Background(), state, fakeSession(t)); err == nil {
		t.Fatal("expected error on anti-bot block")
	}
	if state.Status() != domain.StatusFailed {
		t.Errorf("status = %s, want failed", state.Status())
	}
}

// fakeSession returns a session value useful for race tests; the
// preparer doesn't actually inspect it but the engine requires
// non-nil.
func fakeSession(t *testing.T) providers.Session {
	t.Helper()
	return &noopSession{}
}

type noopSession struct{}

func (*noopSession) User() domain.UserID         { return "u1" }
func (*noopSession) Provider() domain.ProviderID { return "resy" }
func (*noopSession) ExpiresAt() time.Time {
	return time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)
}
