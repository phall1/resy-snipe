package planner_test

import (
	"errors"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/planner"
)

// TestComputeDrop_WorkedExample asserts the exact example from
// design/planner.md §drop-moment-math: target 2026-06-09, 30 days
// ahead, midnight America/New_York → 2026-05-10T04:00:00Z.
func TestComputeDrop_WorkedExample(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")

	got, err := planner.ComputeDrop(
		domain.NewDate(2026, time.June, 9),
		30,
		domain.NewWallTime(0, 0, 0),
		ny,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := time.Date(2026, time.May, 10, 4, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("drop = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Location() != time.UTC {
		t.Errorf("drop location = %v, want UTC", got.Location())
	}
}

// TestComputeDrop_TimezoneVariations exercises the algorithm across
// several IANA zones the operator may legitimately see in production.
func TestComputeDrop_TimezoneVariations(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")
	la := mustLoc(t, "America/Los_Angeles")
	est5edt := mustLoc(t, "EST5EDT") // Astoria DC's actual tz string in v1 configs

	cases := []struct {
		name      string
		target    domain.Date
		daysAhead int
		wallTime  domain.WallTime
		tz        *time.Location
		want      time.Time // UTC
	}{
		{
			name:      "UTC midnight 30d ahead",
			target:    domain.NewDate(2026, time.June, 9),
			daysAhead: 30,
			wallTime:  domain.NewWallTime(0, 0, 0),
			tz:        time.UTC,
			want:      time.Date(2026, time.May, 10, 0, 0, 0, 0, time.UTC),
		},
		{
			name:      "America/New_York 09:00 EDT 30d ahead",
			target:    domain.NewDate(2026, time.June, 9),
			daysAhead: 30,
			wallTime:  domain.NewWallTime(9, 0, 0),
			tz:        ny,
			// 09:00 EDT (UTC-4) = 13:00Z
			want: time.Date(2026, time.May, 10, 13, 0, 0, 0, time.UTC),
		},
		{
			name:      "America/Los_Angeles midnight 30d ahead",
			target:    domain.NewDate(2026, time.June, 9),
			daysAhead: 30,
			wallTime:  domain.NewWallTime(0, 0, 0),
			tz:        la,
			// 00:00 PDT (UTC-7) = 07:00Z
			want: time.Date(2026, time.May, 10, 7, 0, 0, 0, time.UTC),
		},
		{
			name:      "EST5EDT (Astoria DC) midnight 30d ahead",
			target:    domain.NewDate(2026, time.June, 9),
			daysAhead: 30,
			wallTime:  domain.NewWallTime(0, 0, 0),
			tz:        est5edt,
			// EST5EDT behaves like America/New_York for DST purposes; on
			// 2026-05-10 it is in EDT (UTC-4) → 04:00Z.
			want: time.Date(2026, time.May, 10, 4, 0, 0, 0, time.UTC),
		},
		{
			name:      "America/New_York 09:00 in EST (winter target)",
			target:    domain.NewDate(2027, time.February, 9),
			daysAhead: 30,
			wallTime:  domain.NewWallTime(9, 0, 0),
			tz:        ny,
			// 2027-01-10 09:00 EST (UTC-5) = 14:00Z
			want: time.Date(2027, time.January, 10, 14, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := planner.ComputeDrop(tc.target, tc.daysAhead, tc.wallTime, tc.tz)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("drop = %s, want %s", got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

// TestComputeDrop_DSTGap pins down behavior when the anchor date and
// wall-time land in a spring-forward gap (the wall time does not exist
// on the venue's calendar).
//
// In America/New_York 2026, DST begins on Sunday 2026-03-08: clocks
// jump 02:00 EST → 03:00 EDT. A wall time of 02:30 on that day does
// not exist. Go's stdlib time.Date normalizes such inputs by treating
// the offset as the pre-transition zone (EST, UTC-5): 02:30 EST →
// 07:30Z. We document and assert that behavior here so future Go
// upgrades that change normalization rules surface as a test failure
// rather than a silent drop-moment drift.
func TestComputeDrop_DSTGap_SpringForward(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")

	// target − 30 days = 2026-03-08 (DST gap day).
	got, err := planner.ComputeDrop(
		domain.NewDate(2026, time.April, 7),
		30,
		domain.NewWallTime(2, 30, 0),
		ny,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// Go's documented behavior on DST-gap wall times: time.Date keeps
	// the pre-transition (EST, UTC-5) zone offset, so a 02:30 input is
	// normalized to the absolute instant equivalent to "01:30 EST" =
	// 06:30Z. (Stated differently: Go does not silently advance into
	// the post-gap zone; the wall clock is treated as if it had not
	// yet sprung forward.) Asserting the exact UTC instant here guards
	// against silent drift if stdlib normalization rules ever change.
	want := time.Date(2026, time.March, 8, 6, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("DST-gap drop = %s, want %s\n"+
			"  Go's time.Date normalizes a non-existent wall time using "+
			"the pre-transition zone offset; see drop.go DST commentary.",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestComputeDrop_DSTOverlap_FallBack pins down behavior when the
// anchor date and wall-time land in a fall-back overlap (the wall time
// exists twice). Go's stdlib picks the earlier of the two occurrences
// (EDT, UTC-4) on America/New_York fall-back day.
func TestComputeDrop_DSTOverlap_FallBack(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")

	// target − 30 days = 2026-11-01 (DST overlap day: 02:00 EDT → 01:00 EST).
	got, err := planner.ComputeDrop(
		domain.NewDate(2026, time.December, 1),
		30,
		domain.NewWallTime(1, 30, 0),
		ny,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// First 01:30 (EDT, UTC-4) = 05:30Z; second 01:30 (EST, UTC-5) = 06:30Z.
	// Go picks the earlier (EDT).
	want := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("DST-overlap drop = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestComputeDrop_ZeroDaysAhead exercises the "release on target date"
// edge — uncommon but legal; the planner should not refuse it.
func TestComputeDrop_ZeroDaysAhead(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")
	got, err := planner.ComputeDrop(
		domain.NewDate(2026, time.June, 9),
		0,
		domain.NewWallTime(10, 0, 0),
		ny,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// 2026-06-09 10:00 EDT (UTC-4) = 14:00Z
	want := time.Date(2026, time.June, 9, 14, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("drop = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestComputeDrop_OneDayAhead exercises the "release on day before"
// case (the Discovered fallback prior in strategy.go).
func TestComputeDrop_OneDayAhead(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")
	got, err := planner.ComputeDrop(
		domain.NewDate(2026, time.June, 9),
		1,
		domain.NewWallTime(0, 0, 0),
		ny,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := time.Date(2026, time.June, 8, 4, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("drop = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestComputeDrop_OneYearAhead sanity-checks the calendar arithmetic
// across a full year. 2026-06-09 minus 365 days = 2025-06-09.
func TestComputeDrop_OneYearAhead(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")
	got, err := planner.ComputeDrop(
		domain.NewDate(2026, time.June, 9),
		365,
		domain.NewWallTime(0, 0, 0),
		ny,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// 2025-06-09 00:00 EDT = 2025-06-09T04:00Z
	want := time.Date(2025, time.June, 9, 4, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("drop = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestComputeDrop_NegativeDaysAhead errors out — there is no defined
// semantics for "release N days after the target reservation".
func TestComputeDrop_NegativeDaysAhead(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")
	_, err := planner.ComputeDrop(
		domain.NewDate(2026, time.June, 9),
		-1,
		domain.NewWallTime(0, 0, 0),
		ny,
	)
	if !errors.Is(err, planner.ErrInvalidReleaseDaysAhead) {
		t.Fatalf("err = %v, want errors.Is %v", err, planner.ErrInvalidReleaseDaysAhead)
	}
}

// TestComputeDrop_NilLocation errors out — the planner refuses to
// fabricate a default timezone. The Resolver guarantees Venue.TZ.
func TestComputeDrop_NilLocation(t *testing.T) {
	t.Parallel()
	_, err := planner.ComputeDrop(
		domain.NewDate(2026, time.June, 9),
		30,
		domain.NewWallTime(0, 0, 0),
		nil,
	)
	if !errors.Is(err, planner.ErrMissingLocation) {
		t.Fatalf("err = %v, want errors.Is %v", err, planner.ErrMissingLocation)
	}
}

// TestDiscoveredProbeWindow_Defaults asserts the documented defaults:
// 90s lead, 5m width, UTC output.
func TestDiscoveredProbeWindow_Defaults(t *testing.T) {
	t.Parallel()
	approx := time.Date(2026, time.May, 10, 4, 0, 0, 0, time.UTC)
	from, until := planner.DiscoveredProbeWindow(approx)

	wantFrom := time.Date(2026, time.May, 10, 3, 58, 30, 0, time.UTC)
	wantUntil := time.Date(2026, time.May, 10, 4, 3, 30, 0, time.UTC)
	if !from.Equal(wantFrom) {
		t.Errorf("ProbeFrom = %s, want %s", from.Format(time.RFC3339), wantFrom.Format(time.RFC3339))
	}
	if !until.Equal(wantUntil) {
		t.Errorf("ProbeUntil = %s, want %s", until.Format(time.RFC3339), wantUntil.Format(time.RFC3339))
	}
	if width := until.Sub(from); width != 5*time.Minute {
		t.Errorf("probe width = %s, want 5m", width)
	}
	if from.Location() != time.UTC {
		t.Errorf("ProbeFrom location = %v, want UTC", from.Location())
	}
}
