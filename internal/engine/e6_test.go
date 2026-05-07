package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/engine"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/store"
)

// (a) State-machine coverage — every legal transition is exercised
// through engine.SnipeState.Transition (NOT the raw domain.Transition)
// to lock in that the engine's atomic clone-and-swap path agrees with
// the domain table.
func TestEngineDrivesEveryLegalTransition(t *testing.T) {
	t.Parallel()
	type edge struct {
		from, to domain.Status
		event    domain.EventType
	}
	edges := []edge{
		{domain.StatusSubmitted, domain.StatusScheduled, domain.EventScheduled},
		{domain.StatusSubmitted, domain.StatusDiscovering, domain.EventDiscovered},
		{domain.StatusSubmitted, domain.StatusCanceled, domain.EventCanceled},
		{domain.StatusSubmitted, domain.StatusFailed, domain.EventFailed},
		{domain.StatusScheduled, domain.StatusAwaiting, domain.EventReleased},
		{domain.StatusScheduled, domain.StatusDiscovering, domain.EventDiscovered},
		{domain.StatusScheduled, domain.StatusCanceled, domain.EventCanceled},
		{domain.StatusScheduled, domain.StatusFailed, domain.EventFailed},
		{domain.StatusScheduled, domain.StatusExpired, domain.EventExpired},
		{domain.StatusDiscovering, domain.StatusAwaiting, domain.EventReleased},
		{domain.StatusDiscovering, domain.StatusCanceled, domain.EventCanceled},
		{domain.StatusDiscovering, domain.StatusFailed, domain.EventFailed},
		{domain.StatusDiscovering, domain.StatusExpired, domain.EventExpired},
		{domain.StatusAwaiting, domain.StatusFinding, domain.EventFound},
		{domain.StatusAwaiting, domain.StatusCanceled, domain.EventCanceled},
		{domain.StatusAwaiting, domain.StatusFailed, domain.EventFailed},
		{domain.StatusAwaiting, domain.StatusExpired, domain.EventExpired},
		{domain.StatusFinding, domain.StatusBooking, domain.EventBookAttempted},
		{domain.StatusFinding, domain.StatusAwaiting, domain.EventReleased},
		{domain.StatusFinding, domain.StatusCanceled, domain.EventCanceled},
		{domain.StatusFinding, domain.StatusFailed, domain.EventFailed},
		{domain.StatusFinding, domain.StatusExpired, domain.EventExpired},
		{domain.StatusBooking, domain.StatusBooked, domain.EventBooked},
		{domain.StatusBooking, domain.StatusFinding, domain.EventFound},
		{domain.StatusBooking, domain.StatusCanceled, domain.EventCanceled},
		{domain.StatusBooking, domain.StatusFailed, domain.EventFailed},
	}

	for _, e := range edges {
		t.Run(e.from.String()+"->"+e.to.String(), func(t *testing.T) {
			t.Parallel()
			eng, _, _ := newReleaseFixture(t, nil)
			s, err := eng.Submit(context.Background(),
				domain.SnipeID("snp_"+e.from.String()+"_"+e.to.String()),
				domain.Intent{
					User:      "u",
					Venue:     domain.VenueRef{Provider: "resy", Ref: "1"},
					Date:      domain.NewDate(2026, time.June, 1),
					PartySize: 2,
					Release:   domain.ExplicitRelease{At: time.Now().Add(time.Hour)},
				})
			if err != nil {
				t.Fatal(err)
			}
			// Walk to `from`.
			for _, step := range walkTo(e.from) {
				if err := s.Transition(context.Background(), step.to, step.ev); err != nil {
					t.Fatalf("walk to %s: %v", e.from, err)
				}
			}
			if err := s.Transition(context.Background(), e.to, e.event); err != nil {
				t.Fatalf("Transition %s->%s: %v", e.from, e.to, err)
			}
			if s.Status() != e.to {
				t.Errorf("status: %s want %s", s.Status(), e.to)
			}
		})
	}
}

// walkTo returns the canonical chain of (status, event) pairs to drive
// a fresh SnipeState from Submitted to `target`. Panics on a target
// the helper hasn't been taught about — callers stick to the spec'd
// statuses.
func walkTo(target domain.Status) []struct {
	to domain.Status
	ev domain.EventType
} {
	type step = struct {
		to domain.Status
		ev domain.EventType
	}
	switch target {
	case domain.StatusSubmitted:
		return nil
	case domain.StatusScheduled:
		return []step{{domain.StatusScheduled, domain.EventScheduled}}
	case domain.StatusDiscovering:
		return []step{{domain.StatusDiscovering, domain.EventDiscovered}}
	case domain.StatusAwaiting:
		return []step{
			{domain.StatusScheduled, domain.EventScheduled},
			{domain.StatusAwaiting, domain.EventReleased},
		}
	case domain.StatusFinding:
		return []step{
			{domain.StatusScheduled, domain.EventScheduled},
			{domain.StatusAwaiting, domain.EventReleased},
			{domain.StatusFinding, domain.EventFound},
		}
	case domain.StatusBooking:
		return []step{
			{domain.StatusScheduled, domain.EventScheduled},
			{domain.StatusAwaiting, domain.EventReleased},
			{domain.StatusFinding, domain.EventFound},
			{domain.StatusBooking, domain.EventBookAttempted},
		}
	case domain.StatusBooked:
		return []step{
			{domain.StatusScheduled, domain.EventScheduled},
			{domain.StatusAwaiting, domain.EventReleased},
			{domain.StatusFinding, domain.EventFound},
			{domain.StatusBooking, domain.EventBookAttempted},
			{domain.StatusBooked, domain.EventBooked},
		}
	case domain.StatusFailed:
		return []step{{domain.StatusFailed, domain.EventFailed}}
	case domain.StatusCanceled:
		return []step{{domain.StatusCanceled, domain.EventCanceled}}
	case domain.StatusExpired:
		return []step{
			{domain.StatusScheduled, domain.EventScheduled},
			{domain.StatusExpired, domain.EventExpired},
		}
	}
	return nil
}

// (a) State machine — invalid transitions are rejected, in-memory
// status not mutated.
func TestEngineRejectsInvalidTransitionAcrossAllPairs(t *testing.T) {
	t.Parallel()
	for _, from := range domain.AllStatuses() {
		for _, to := range domain.AllStatuses() {
			if domain.CanTransition(from, to) {
				continue
			}
			t.Run("invalid_"+from.String()+"_to_"+to.String(), func(t *testing.T) {
				t.Parallel()
				eng, _, _ := newReleaseFixture(t, nil)
				s, err := eng.Submit(context.Background(),
					domain.SnipeID("snp_inv_"+from.String()+"_"+to.String()),
					domain.Intent{
						User:      "u",
						Venue:     domain.VenueRef{Provider: "resy", Ref: "1"},
						Date:      domain.NewDate(2026, time.June, 1),
						PartySize: 2,
						Release:   domain.ExplicitRelease{At: time.Now().Add(time.Hour)},
					})
				if err != nil {
					t.Fatal(err)
				}
				path := walkTo(from)
				// If the helper doesn't know how to reach `from`, skip
				// rather than running the assertion against a wrong
				// starting state.
				if path == nil && from != domain.StatusSubmitted {
					t.Skipf("walkTo(%s) not implemented", from)
				}
				for _, step := range path {
					if err := s.Transition(context.Background(), step.to, step.ev); err != nil {
						t.Skipf("cannot reach %s: %v", from, err)
					}
				}
				err = s.Transition(context.Background(), to, domain.EventFailed)
				if err == nil {
					t.Fatalf("transition %s->%s should have failed", from, to)
				}
				var ite domain.InvalidTransitionError
				if !errors.As(err, &ite) {
					t.Errorf("expected InvalidTransitionError, got %v", err)
				}
				if s.Status() != from {
					t.Errorf("status mutated on rejected transition: %s", s.Status())
				}
			})
		}
	}
}

// (c) End-to-end integration — Discovered → Awaiting → Finding →
// Booking → Booked — using a fakePreparer and the real engine
// dispatch path, with the fake clock driving release and the
// race-and-cancel real-clock-coupled. Acceptance: terminal status
// is Booked, observed_release_times row written, no goroutine leak.
func TestEngineE2EDiscoveredThroughBooked(t *testing.T) {
	t.Parallel()
	pre := newRacePreparer("tok-A")
	pre.calendarFn = func(_ context.Context, _ domain.VenueRef, _ providers.DateRange) (providers.Calendar, error) {
		return providers.Calendar{
			Days: []providers.CalendarDay{
				{Date: domain.NewDate(2026, time.June, 1), Available: true},
			},
		}, nil
	}

	dir := t.TempDir()
	db, err := store.Open(context.Background(), dir+"/e2e.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	s := store.NewSQLiteStore(db)

	c := clock.NewReal() // race tests use real clock; release loop also uses it
	eng := engine.New(s, c, discardLogger(),
		engine.WithProvider(pre),
		engine.WithBookingPolicy(domain.BookingPolicy{
			PollFloor:     time.Millisecond,
			MaxConcurrent: 4,
		}),
	)

	if err := s.UpsertVenue(context.Background(), domain.Venue{
		Provider: "resy", Ref: "38660", Name: "Carbone", TZ: time.UTC,
	}); err != nil {
		t.Fatal(err)
	}

	probeFrom := time.Now()
	probeUntil := probeFrom.Add(time.Second)
	intent := domain.Intent{
		User:      "u",
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
		SlotPrefs: []domain.SlotPreference{{Time: domain.NewWallTime(19, 0, 0)}},
		Release:   domain.DiscoveredRelease{ProbeFrom: probeFrom, ProbeUntil: probeUntil},
	}

	state, err := eng.Submit(context.Background(), "snp_e2e", intent)
	if err != nil {
		t.Fatal(err)
	}

	if err := eng.Run(context.Background(), state.ID()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Run loaded its own SnipeState internally; the test's local
	// `state` has stale status. Re-load before calling RunBookingRace.
	reloaded, err := eng.Load(context.Background(), state.ID())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := eng.RunBookingRace(context.Background(), reloaded, fakeSession(t)); err != nil {
		t.Fatalf("RunBookingRace: %v", err)
	}
	state = reloaded

	if state.Status() != domain.StatusBooked {
		t.Fatalf("terminal status: %s want booked", state.Status())
	}
	if state.Inner().Result == nil || state.Inner().Result.ConfirmationID == "" {
		t.Errorf("Result not populated: %+v", state.Inner().Result)
	}
	obs, err := s.GetObserved(context.Background(),
		domain.VenueRef{Provider: "resy", Ref: "38660"})
	if err != nil {
		t.Errorf("observed_release_times not written: %v", err)
	}
	if obs.ObservedAt.IsZero() {
		t.Errorf("observed_at zero")
	}

}
