package planner_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/planner"
	"resy-snipe/internal/providers"
)

// fixture builders ----------------------------------------------------------

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return loc
}

func baseGoal(date domain.Date) domain.Goal {
	return domain.Goal{
		VenueQuery: domain.VenueQuerySlug{Slug: "astoria-dc", City: "ny"},
		Date:       date,
		Party:      2,
		TimePrefs: domain.TimeWindow{
			Start:    domain.NewWallTime(19, 0, 0),
			End:      domain.NewWallTime(21, 0, 0),
			Priority: domain.PriorityEarlier,
		},
		AccountID: "user-1",
	}
}

func baseVenue(tz *time.Location) domain.Venue {
	return domain.Venue{
		Provider: "resy",
		Ref:      "49716",
		Name:     "Astoria DC",
		TZ:       tz,
	}
}

func calendarWith(ref domain.VenueRef, day providers.CalendarDay) providers.Calendar {
	return providers.Calendar{
		Venue: ref,
		Days:  []providers.CalendarDay{day},
	}
}

// helpers -------------------------------------------------------------------

func ptrTime(t time.Time) *time.Time { return &t }

func notesContain(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// table-driven tests --------------------------------------------------------

func TestSelectStrategy(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")
	la := mustLoc(t, "America/Los_Angeles")
	utc := time.UTC

	target := domain.NewDate(2026, time.June, 9)
	targetRef := domain.VenueRef{Provider: "resy", Ref: "49716"}

	// A fixed "now" we can reason about: 2026-05-08T15:00Z, well before
	// a 30-day-ahead release.
	now := time.Date(2026, time.May, 8, 15, 0, 0, 0, utc)

	cases := []struct {
		name         string
		in           planner.StrategyInput
		wantErrIs    error
		wantStrategy string // "continuous" | "explicit" | "discovered"
		wantNoteSub  string // optional substring
		extraAssert  func(t *testing.T, s domain.ReleaseStrategy)
	}{
		{
			name: "target date in past returns sentinel",
			in: planner.StrategyInput{
				Goal:  baseGoal(domain.NewDate(2026, time.May, 1)),
				Venue: baseVenue(ny),
				Now:   now,
			},
			wantErrIs: planner.ErrTargetInPast,
		},
		{
			name: "missing venue tz returns domain sentinel",
			in: planner.StrategyInput{
				Goal:  baseGoal(target),
				Venue: domain.Venue{Provider: "resy", Ref: "49716"},
				Now:   now,
			},
			wantErrIs: domain.ErrVenueMissingTZ,
		},
		{
			name: "target bookable now selects Continuous (operator-tz LA, venue-tz NY)",
			in: planner.StrategyInput{
				Goal:             baseGoal(target),
				Venue:            baseVenue(ny),
				Now:              now.In(la),
				CalendarSnapshot: calendarWith(targetRef, providers.CalendarDay{Date: target, Available: true}),
			},
			wantStrategy: "continuous",
			wantNoteSub:  "Continuous",
		},
		{
			name: "target listed but not available falls through to Discovered",
			in: planner.StrategyInput{
				Goal:             baseGoal(target),
				Venue:            baseVenue(ny),
				Now:              now,
				CalendarSnapshot: calendarWith(targetRef, providers.CalendarDay{Date: target, Available: false}),
			},
			wantStrategy: "discovered",
			wantNoteSub:  "Discovered",
		},
		{
			name: "empty calendar + observed release avg selects Explicit",
			in: planner.StrategyInput{
				Goal:               baseGoal(target),
				Venue:              baseVenue(ny),
				Now:                now,
				ObservedReleaseAvg: ptrTime(time.Date(2026, time.May, 10, 4, 0, 0, 0, utc)),
			},
			wantStrategy: "explicit",
			wantNoteSub:  "observed release average",
			extraAssert: func(t *testing.T, s domain.ReleaseStrategy) {
				t.Helper()
				e, ok := s.(domain.ExplicitRelease)
				if !ok {
					t.Fatalf("want ExplicitRelease, got %T", s)
				}
				want := time.Date(2026, time.May, 10, 4, 0, 0, 0, utc)
				if !e.At.Equal(want) {
					t.Errorf("ExplicitRelease.At = %s, want %s", e.At, want)
				}
			},
		},
		{
			name: "empty calendar + release config selects Explicit via config",
			in: planner.StrategyInput{
				Goal:  baseGoal(target),
				Venue: baseVenue(ny),
				Now:   now,
				ReleaseConfig: planner.VenueReleaseConfig{
					DaysAhead: 30,
					TimeLocal: domain.NewWallTime(0, 0, 0),
				},
			},
			wantStrategy: "explicit",
			wantNoteSub:  "release_days_ahead=30",
			extraAssert: func(t *testing.T, s domain.ReleaseStrategy) {
				t.Helper()
				e, ok := s.(domain.ExplicitRelease)
				if !ok {
					t.Fatalf("want ExplicitRelease, got %T", s)
				}
				// 2026-06-09 in NY minus 30 days = 2026-05-10
				// 2026-05-10T00:00 EDT (UTC-4) = 2026-05-10T04:00Z.
				want := time.Date(2026, time.May, 10, 4, 0, 0, 0, utc)
				if !e.At.Equal(want) {
					t.Errorf("ExplicitRelease.At = %s, want %s", e.At, want)
				}
			},
		},
		{
			name: "no calendar + no observation + no config selects Discovered",
			in: planner.StrategyInput{
				Goal:  baseGoal(target),
				Venue: baseVenue(ny),
				Now:   now,
			},
			wantStrategy: "discovered",
			wantNoteSub:  "Discovered",
		},
		{
			name: "observation wins over release config (more authoritative signal)",
			in: planner.StrategyInput{
				Goal:               baseGoal(target),
				Venue:              baseVenue(ny),
				Now:                now,
				ObservedReleaseAvg: ptrTime(time.Date(2026, time.May, 10, 5, 0, 0, 0, utc)),
				ReleaseConfig: planner.VenueReleaseConfig{
					DaysAhead: 30,
					TimeLocal: domain.NewWallTime(0, 0, 0),
				},
			},
			wantStrategy: "explicit",
			wantNoteSub:  "observed",
			extraAssert: func(t *testing.T, s domain.ReleaseStrategy) {
				t.Helper()
				e, ok := s.(domain.ExplicitRelease)
				if !ok {
					t.Fatalf("want ExplicitRelease, got %T", s)
				}
				want := time.Date(2026, time.May, 10, 5, 0, 0, 0, utc)
				if !e.At.Equal(want) {
					t.Errorf("ExplicitRelease.At = %s, want %s", e.At, want)
				}
			},
		},
		{
			name: "calendar bookability wins over observation (target is already open)",
			in: planner.StrategyInput{
				Goal:               baseGoal(target),
				Venue:              baseVenue(ny),
				Now:                now,
				CalendarSnapshot:   calendarWith(targetRef, providers.CalendarDay{Date: target, Available: true}),
				ObservedReleaseAvg: ptrTime(time.Date(2026, time.May, 10, 4, 0, 0, 0, utc)),
			},
			wantStrategy: "continuous",
			wantNoteSub:  "Continuous",
		},
		{
			name: "calendar with unrelated days falls through to Discovered",
			in: planner.StrategyInput{
				Goal:  baseGoal(target),
				Venue: baseVenue(ny),
				Now:   now,
				CalendarSnapshot: providers.Calendar{
					Venue: targetRef,
					Days: []providers.CalendarDay{
						{Date: domain.NewDate(2026, time.June, 8), Available: true},
						{Date: domain.NewDate(2026, time.June, 10), Available: true},
					},
				},
			},
			wantStrategy: "discovered",
		},
		{
			name: "Continuous default Until is end-of-venue-day when ContinuousUntil unset",
			in: planner.StrategyInput{
				Goal:             baseGoal(target),
				Venue:            baseVenue(ny),
				Now:              now,
				CalendarSnapshot: calendarWith(targetRef, providers.CalendarDay{Date: target, Available: true}),
			},
			wantStrategy: "continuous",
			extraAssert: func(t *testing.T, s domain.ReleaseStrategy) {
				t.Helper()
				c, ok := s.(domain.ContinuousRelease)
				if !ok {
					t.Fatalf("want ContinuousRelease, got %T", s)
				}
				// now=2026-05-08T15:00Z = 2026-05-08T11:00 NY (EDT, UTC-4),
				// end-of-day = 2026-05-08T23:59:59 EDT = 2026-05-09T03:59:59Z.
				want := time.Date(2026, time.May, 9, 3, 59, 59, 0, utc)
				if !c.Until.Equal(want) {
					t.Errorf("ContinuousRelease.Until = %s, want %s", c.Until, want)
				}
			},
		},
		{
			name: "Continuous honors caller-supplied Until",
			in: planner.StrategyInput{
				Goal:             baseGoal(target),
				Venue:            baseVenue(ny),
				Now:              now,
				CalendarSnapshot: calendarWith(targetRef, providers.CalendarDay{Date: target, Available: true}),
				ContinuousUntil:  time.Date(2026, time.May, 8, 18, 0, 0, 0, utc),
			},
			wantStrategy: "continuous",
			extraAssert: func(t *testing.T, s domain.ReleaseStrategy) {
				t.Helper()
				c, ok := s.(domain.ContinuousRelease)
				if !ok {
					t.Fatalf("want ContinuousRelease, got %T", s)
				}
				want := time.Date(2026, time.May, 8, 18, 0, 0, 0, utc)
				if !c.Until.Equal(want) {
					t.Errorf("ContinuousRelease.Until = %s, want %s", c.Until, want)
				}
			},
		},
		{
			name: "Discovered probe window is 5m wide with 90s lead",
			in: planner.StrategyInput{
				Goal:  baseGoal(target),
				Venue: baseVenue(ny),
				Now:   now,
			},
			wantStrategy: "discovered",
			extraAssert: func(t *testing.T, s domain.ReleaseStrategy) {
				t.Helper()
				d, ok := s.(domain.DiscoveredRelease)
				if !ok {
					t.Fatalf("want DiscoveredRelease, got %T", s)
				}
				width := d.ProbeUntil.Sub(d.ProbeFrom)
				if width != 5*time.Minute {
					t.Errorf("probe width = %s, want 5m", width)
				}
				// Day-before midnight NY = 2026-06-08T00:00 EDT = 2026-06-08T04:00Z.
				// ProbeFrom = that minus 90s.
				wantFrom := time.Date(2026, time.June, 8, 3, 58, 30, 0, utc)
				if !d.ProbeFrom.Equal(wantFrom) {
					t.Errorf("ProbeFrom = %s, want %s", d.ProbeFrom, wantFrom)
				}
			},
		},
		{
			name: "target == today in venue tz is not past",
			in: planner.StrategyInput{
				Goal: baseGoal(domain.NewDate(2026, time.May, 8)),
				// Venue in NY; now=2026-05-08T15:00Z → 11:00 NY on the same day.
				Venue: baseVenue(ny),
				Now:   now,
			},
			wantStrategy: "discovered",
		},
		{
			name: "past-date check uses venue tz, not UTC",
			// Now=2026-05-08T02:00Z is 2026-05-07T22:00 NY → NY-today is May 7.
			// Goal.Date=2026-05-07 must NOT be flagged as past.
			in: planner.StrategyInput{
				Goal:  baseGoal(domain.NewDate(2026, time.May, 7)),
				Venue: baseVenue(ny),
				Now:   time.Date(2026, time.May, 8, 2, 0, 0, 0, utc),
			},
			wantStrategy: "discovered",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotStrategy, notes, err := planner.SelectStrategy(context.Background(), tc.in)
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErrIs)
				}
				if gotStrategy != nil {
					t.Errorf("strategy = %v on error, want nil", gotStrategy)
				}
				if notes != nil {
					t.Errorf("notes = %v on error, want nil", notes)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			switch tc.wantStrategy {
			case "continuous":
				if _, ok := gotStrategy.(domain.ContinuousRelease); !ok {
					t.Fatalf("strategy = %T, want ContinuousRelease", gotStrategy)
				}
			case "explicit":
				if _, ok := gotStrategy.(domain.ExplicitRelease); !ok {
					t.Fatalf("strategy = %T, want ExplicitRelease", gotStrategy)
				}
			case "discovered":
				if _, ok := gotStrategy.(domain.DiscoveredRelease); !ok {
					t.Fatalf("strategy = %T, want DiscoveredRelease", gotStrategy)
				}
			default:
				t.Fatalf("test bug: wantStrategy=%q", tc.wantStrategy)
			}
			if len(notes) == 0 {
				t.Errorf("expected at least one note; got none")
			}
			if tc.wantNoteSub != "" && !notesContain(notes, tc.wantNoteSub) {
				t.Errorf("notes %q do not contain %q", notes, tc.wantNoteSub)
			}
			if tc.extraAssert != nil {
				tc.extraAssert(t, gotStrategy)
			}
		})
	}
}

// TestSelectStrategy_ContextNotConsulted documents the M1-06 contract:
// SelectStrategy does no blocking work, so it ignores ctx cancellation.
// M1-07/M1-08 may introduce work that honors ctx; this test guards
// against accidental regressions to that behavior in the strategy
// selection layer.
func TestSelectStrategy_IgnoresCanceledContext(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")
	target := domain.NewDate(2026, time.June, 9)
	in := planner.StrategyInput{
		Goal:  baseGoal(target),
		Venue: baseVenue(ny),
		Now:   time.Date(2026, time.May, 8, 15, 0, 0, 0, time.UTC),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gotStrategy, notes, err := planner.SelectStrategy(ctx, in)
	if err != nil {
		t.Fatalf("err = %v, want nil (M1-06 has no cancellable work)", err)
	}
	if gotStrategy == nil {
		t.Fatal("strategy = nil")
	}
	if len(notes) == 0 {
		t.Fatal("expected notes")
	}
}
