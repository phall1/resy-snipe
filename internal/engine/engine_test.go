package engine_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/engine"
	"resy-snipe/internal/store"
)

// TestMain panics if the domain transition table is not total over
// every (from, to) status pair. domain.init() already enforces this
// in production builds; this is the belt-and-suspenders check called
// for in the E1 acceptance criteria.
func TestMain(m *testing.M) {
	statuses := domain.AllStatuses()
	if len(statuses) == 0 {
		panic("engine_test: domain.AllStatuses returned no statuses")
	}
	for _, from := range statuses {
		for _, to := range statuses {
			// CanTransition must return a definite bool for every
			// pair. The function panics on an unknown from-state, so
			// reaching here without panic is the totality check.
			_ = domain.CanTransition(from, to)
		}
	}
	os.Exit(m.Run())
}

func newEngineFixture(t *testing.T) (*engine.Engine, *clock.Fake, store.Store) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "engine.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := store.NewSQLiteStore(db)
	c := clock.NewFake(time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return engine.New(s, c, log), c, s
}

func sampleEngineIntent() domain.Intent {
	return domain.Intent{
		User:      "u1",
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
		SlotPrefs: []domain.SlotPreference{
			{Time: domain.NewWallTime(19, 30, 0)},
		},
		Release: domain.ExplicitRelease{At: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)},
	}
}

func TestSubmitCreatesSnipeAndEvent(t *testing.T) {
	t.Parallel()
	eng, _, s := newEngineFixture(t)
	ctx := context.Background()

	state, err := eng.Submit(ctx, "snp_1", sampleEngineIntent())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if state.Status() != domain.StatusSubmitted {
		t.Errorf("status: %s want submitted", state.Status())
	}

	events, err := s.ListEventsBySnipe(ctx, "snp_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != domain.EventSubmitted {
		t.Fatalf("submit did not emit Submitted event: %+v", events)
	}
}

func TestTransitionPersistsAtomically(t *testing.T) {
	t.Parallel()
	eng, fake, s := newEngineFixture(t)
	ctx := context.Background()

	state, err := eng.Submit(ctx, "snp_at", sampleEngineIntent())
	if err != nil {
		t.Fatal(err)
	}

	fake.Advance(time.Minute)
	if err := state.Transition(ctx, domain.StatusScheduled, domain.EventScheduled,
		slog.String("reason", "release_known")); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	// State updated in DB.
	got, err := s.GetSnipe(ctx, "snp_at")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != domain.StatusScheduled {
		t.Fatalf("DB status: %s want scheduled", got.Status())
	}

	// Both events landed.
	events, err := s.ListEventsBySnipe(ctx, "snp_at")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	if events[1].Type != domain.EventScheduled {
		t.Errorf("event[1] type: %s", events[1].Type)
	}
	// snipe_id is auto-attached.
	foundSnipeID := false
	for _, a := range events[1].Attrs {
		if a.Key == domain.LogKeySnipeID && a.Value.String() == "snp_at" {
			foundSnipeID = true
		}
	}
	if !foundSnipeID {
		t.Error("scheduled event missing snipe_id attribute")
	}
}

func TestTransitionRejectsInvalid(t *testing.T) {
	t.Parallel()
	eng, _, s := newEngineFixture(t)
	ctx := context.Background()

	state, err := eng.Submit(ctx, "snp_bad", sampleEngineIntent())
	if err != nil {
		t.Fatal(err)
	}

	// Submitted -> Booked is not allowed.
	err = state.Transition(ctx, domain.StatusBooked, domain.EventBooked)
	if err == nil {
		t.Fatal("invalid transition must error")
	}
	var ite domain.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("err is not InvalidTransitionError: %v", err)
	}

	// Status unchanged in memory.
	if state.Status() != domain.StatusSubmitted {
		t.Errorf("in-memory status mutated on rejected transition: %s", state.Status())
	}

	// Status unchanged in DB; only the Submitted event is present.
	got, err := s.GetSnipe(ctx, "snp_bad")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != domain.StatusSubmitted {
		t.Errorf("DB status: %s", got.Status())
	}
	events, err := s.ListEventsBySnipe(ctx, "snp_bad")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event after rejected transition, got %d", len(events))
	}
}

func TestTransitionMissingSnipeReturnsNotFound(t *testing.T) {
	t.Parallel()
	eng, _, s := newEngineFixture(t)
	ctx := context.Background()

	// Build an in-memory state without persisting it through Submit.
	innerState, err := eng.Submit(ctx, "snp_real", sampleEngineIntent())
	if err != nil {
		t.Fatal(err)
	}

	// Now delete the row out from under us via a raw store call to
	// simulate a vanished snipe, then attempt to transition.
	if _, err := s.ListSnipesByStatus(ctx, domain.StatusSubmitted); err != nil {
		t.Fatal(err)
	}
	if err := wipeSnipeRow(s, "snp_real"); err != nil {
		t.Fatalf("wipe: %v", err)
	}

	err = innerState.Transition(ctx, domain.StatusScheduled, domain.EventScheduled)
	if err == nil {
		t.Fatal("transition on missing snipe must error")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// In-memory state unchanged because persist failed.
	if innerState.Status() != domain.StatusSubmitted {
		t.Errorf("in-memory status drifted after failed persist: %s", innerState.Status())
	}
}

func TestLoadRoundTripsThroughEngine(t *testing.T) {
	t.Parallel()
	eng, fake, _ := newEngineFixture(t)
	ctx := context.Background()

	if _, err := eng.Submit(ctx, "snp_rt", sampleEngineIntent()); err != nil {
		t.Fatal(err)
	}
	fake.Advance(time.Second)

	got, err := eng.Load(ctx, "snp_rt")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status() != domain.StatusSubmitted {
		t.Errorf("loaded status: %s", got.Status())
	}
	if got.ID() != "snp_rt" {
		t.Errorf("ID: %s", got.ID())
	}
}

func TestLoadMissingReturnsNotFound(t *testing.T) {
	t.Parallel()
	eng, _, _ := newEngineFixture(t)
	_, err := eng.Load(context.Background(), "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestEngineRejectsNilDeps(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil store")
		}
	}()
	_ = engine.New(nil, clock.NewReal(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// wipeSnipeRow forcibly deletes a snipe row so we can exercise the
// engine's failed-persist path.
func wipeSnipeRow(s store.Store, id domain.SnipeID) error {
	concrete, ok := s.(*store.SQLiteStore)
	if !ok {
		return errors.New("store is not *store.SQLiteStore")
	}
	_, err := concrete.DB().ExecContext(context.Background(),
		`DELETE FROM snipes WHERE id = ?`, string(id))
	return err
}
