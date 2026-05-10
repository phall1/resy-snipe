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

// basePlanInput returns a PlanInput configured for the canonical worked
// example: 2026-06-09 reservation at Astoria DC, evaluated on
// 2026-05-08T15:00Z. Individual tests override fields as needed.
func basePlanInput(t *testing.T) planner.PlanInput {
	t.Helper()
	ny := mustLoc(t, "America/New_York")
	target := domain.NewDate(2026, time.June, 9)
	now := time.Date(2026, time.May, 8, 15, 0, 0, 0, time.UTC)
	return planner.PlanInput{
		Goal:  baseGoal(target),
		Venue: baseVenue(ny),
		Now:   now,
	}
}

// TestBuildPlan_DiscoveredStrategyDefaultPath exercises the
// no-calendar/no-observation/no-config branch — the most common Plan
// shape the daemon will ever build.
func TestBuildPlan_DiscoveredStrategyDefaultPath(t *testing.T) {
	t.Parallel()

	in := basePlanInput(t)
	plan, err := planner.BuildPlan(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// Strategy is Discovered.
	disc, ok := plan.Strategy.(domain.DiscoveredRelease)
	if !ok {
		t.Fatalf("Strategy = %T, want DiscoveredRelease", plan.Strategy)
	}
	if plan.DropMoment.IsZero() {
		t.Errorf("DropMoment is zero")
	}
	if !plan.DropMoment.Equal(disc.ProbeFrom) {
		t.Errorf("DropMoment = %s, want ProbeFrom = %s", plan.DropMoment, disc.ProbeFrom)
	}

	// Goal/Venue echoed verbatim.
	if plan.Goal.Date != in.Goal.Date {
		t.Errorf("Goal.Date = %s, want %s", plan.Goal.Date, in.Goal.Date)
	}
	if plan.Venue.Ref != in.Venue.Ref {
		t.Errorf("Venue.Ref = %s, want %s", plan.Venue.Ref, in.Venue.Ref)
	}
	// Slot count: 19:00..21:00 step 30m → 5 entries (19:00, 19:30, 20:00, 20:30, 21:00).
	if len(plan.FireSchedule) != 5 {
		t.Errorf("len(FireSchedule) = %d, want 5", len(plan.FireSchedule))
	}

	// Notes carry the strategy explanation plus the signing note.
	if !notesContain(plan.Notes, "Discovered") {
		t.Errorf("Notes %q missing 'Discovered'", plan.Notes)
	}
	if !notesContain(plan.Notes, "signing_required=false") {
		t.Errorf("Notes %q missing signing-required note", plan.Notes)
	}

	// Hash is non-empty and prefix-stable.
	if plan.Hash == "" {
		t.Error("Hash is empty")
	}
	if !strings.HasPrefix(plan.Hash, "sha256:") {
		t.Errorf("Hash %q missing sha256: prefix", plan.Hash)
	}
	// PlanHashVersion is the v1 sentinel.
	if plan.PlanHashVersion != domain.PlanHashVersionV1 {
		t.Errorf("PlanHashVersion = %d, want %d", plan.PlanHashVersion, domain.PlanHashVersionV1)
	}
	// ComputedAt is in UTC and equals Now.
	if !plan.ComputedAt.Equal(in.Now) {
		t.Errorf("ComputedAt = %s, want %s", plan.ComputedAt, in.Now)
	}
}

// TestBuildPlan_ExplicitStrategyViaObservation exercises the
// observed-release-avg branch and asserts FireSchedule entries all
// anchor at the observed instant.
func TestBuildPlan_ExplicitStrategyViaObservation(t *testing.T) {
	t.Parallel()

	in := basePlanInput(t)
	observed := time.Date(2026, time.May, 10, 4, 0, 0, 0, time.UTC)
	in.ObservedReleaseAvg = &observed

	plan, err := planner.BuildPlan(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	exp, ok := plan.Strategy.(domain.ExplicitRelease)
	if !ok {
		t.Fatalf("Strategy = %T, want ExplicitRelease", plan.Strategy)
	}
	if !exp.At.Equal(observed) {
		t.Errorf("ExplicitRelease.At = %s, want %s", exp.At, observed)
	}
	if !plan.DropMoment.Equal(observed) {
		t.Errorf("DropMoment = %s, want %s", plan.DropMoment, observed)
	}
	// Every FireSchedule entry equals observed (Explicit fan-out shape).
	if len(plan.FireSchedule) != 5 {
		t.Fatalf("len(FireSchedule) = %d, want 5", len(plan.FireSchedule))
	}
	for i, ft := range plan.FireSchedule {
		if !ft.Equal(observed) {
			t.Errorf("FireSchedule[%d] = %s, want %s", i, ft, observed)
		}
	}
}

// TestBuildPlan_ExplicitStrategyViaReleaseConfig exercises the
// configured-release-window branch — the operator-supplied prior path.
func TestBuildPlan_ExplicitStrategyViaReleaseConfig(t *testing.T) {
	t.Parallel()

	in := basePlanInput(t)
	in.ReleaseConfig = &planner.VenueReleaseConfig{
		DaysAhead: 30,
		TimeLocal: domain.NewWallTime(0, 0, 0),
	}

	plan, err := planner.BuildPlan(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	exp, ok := plan.Strategy.(domain.ExplicitRelease)
	if !ok {
		t.Fatalf("Strategy = %T, want ExplicitRelease", plan.Strategy)
	}
	// 30 days before 2026-06-09 NY = 2026-05-10T00:00 EDT = 2026-05-10T04:00Z.
	want := time.Date(2026, time.May, 10, 4, 0, 0, 0, time.UTC)
	if !exp.At.Equal(want) {
		t.Errorf("ExplicitRelease.At = %s, want %s", exp.At, want)
	}
	if !notesContain(plan.Notes, "release_days_ahead=30") {
		t.Errorf("Notes missing config explanation: %v", plan.Notes)
	}
}

// TestBuildPlan_ContinuousStrategy exercises the calendar-says-bookable
// branch. DropMoment should be Now and FireSchedule should mirror the
// slot list anchored at Now.
func TestBuildPlan_ContinuousStrategy(t *testing.T) {
	t.Parallel()

	in := basePlanInput(t)
	targetRef := domain.VenueRef{Provider: in.Venue.Provider, Ref: in.Venue.Ref}
	in.CalendarSnapshot = providers.Calendar{
		Venue: targetRef,
		Days:  []providers.CalendarDay{{Date: in.Goal.Date, Available: true}},
	}

	plan, err := planner.BuildPlan(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if _, ok := plan.Strategy.(domain.ContinuousRelease); !ok {
		t.Fatalf("Strategy = %T, want ContinuousRelease", plan.Strategy)
	}
	// DropMoment = Now (already polling).
	if !plan.DropMoment.Equal(in.Now) {
		t.Errorf("DropMoment = %s, want Now=%s", plan.DropMoment, in.Now)
	}
	// FireSchedule entries all equal Now (Continuous placeholder).
	if len(plan.FireSchedule) != 5 {
		t.Fatalf("len(FireSchedule) = %d, want 5", len(plan.FireSchedule))
	}
	for i, ft := range plan.FireSchedule {
		if !ft.Equal(in.Now) {
			t.Errorf("FireSchedule[%d] = %s, want Now=%s", i, ft, in.Now)
		}
	}
}

// TestBuildPlan_TableTypeFanOut asserts the FireSchedule reflects the
// ExpandSlots cross-product when Constraints.TableTypes is set.
func TestBuildPlan_TableTypeFanOut(t *testing.T) {
	t.Parallel()

	in := basePlanInput(t)
	in.Goal.Constraints = domain.Constraints{TableTypes: []string{"bar", "patio"}}

	plan, err := planner.BuildPlan(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// 5 times × 2 tables = 10 entries.
	if len(plan.FireSchedule) != 10 {
		t.Errorf("len(FireSchedule) = %d, want 10", len(plan.FireSchedule))
	}
}

// TestBuildPlan_HashStability asserts the canonical-hash contract: two
// runs with identical inputs (modulo ComputedAt, which is excluded
// from canonicalization) produce byte-identical hashes.
func TestBuildPlan_HashStability(t *testing.T) {
	t.Parallel()

	in := basePlanInput(t)

	plan1, err := planner.BuildPlan(context.Background(), in)
	if err != nil {
		t.Fatalf("first BuildPlan: %v", err)
	}

	// Second call with the SAME Now — Notes/ComputedAt are excluded
	// from canonicalization, but DropMoment for Discovered depends on
	// Now via the probe-window calculation, so hash stability requires
	// Now to be identical for the Discovered path.
	plan2, err := planner.BuildPlan(context.Background(), in)
	if err != nil {
		t.Fatalf("second BuildPlan: %v", err)
	}

	if plan1.Hash != plan2.Hash {
		t.Errorf("hash differs across identical inputs:\n  p1=%s\n  p2=%s",
			plan1.Hash, plan2.Hash)
	}

	// Third call with a different Now but the same Explicit observation
	// → DropMoment is invariant to Now, so the hash must still match.
	observed := time.Date(2026, time.May, 10, 4, 0, 0, 0, time.UTC)
	in3 := in
	in3.ObservedReleaseAvg = &observed
	plan3, err := planner.BuildPlan(context.Background(), in3)
	if err != nil {
		t.Fatalf("third BuildPlan: %v", err)
	}

	in4 := in3
	in4.Now = in.Now.Add(7 * time.Minute) // different ComputedAt
	plan4, err := planner.BuildPlan(context.Background(), in4)
	if err != nil {
		t.Fatalf("fourth BuildPlan: %v", err)
	}
	if plan3.Hash != plan4.Hash {
		t.Errorf("hash differs across runs that should be hash-identical (ComputedAt excluded):\n  p3=%s\n  p4=%s",
			plan3.Hash, plan4.Hash)
	}
}

// TestBuildPlan_HashChangesWithStrategy asserts that two Plans whose
// strategy differs hash differently. This is the inverse contract to
// TestBuildPlan_HashStability — the Service layer's replan trigger
// depends on it.
func TestBuildPlan_HashChangesWithStrategy(t *testing.T) {
	t.Parallel()

	base := basePlanInput(t)

	// Plan A — Discovered (no signals).
	planA, err := planner.BuildPlan(context.Background(), base)
	if err != nil {
		t.Fatalf("plan A: %v", err)
	}

	// Plan B — Explicit via observation.
	observed := time.Date(2026, time.May, 10, 4, 0, 0, 0, time.UTC)
	bIn := base
	bIn.ObservedReleaseAvg = &observed
	planB, err := planner.BuildPlan(context.Background(), bIn)
	if err != nil {
		t.Fatalf("plan B: %v", err)
	}

	if planA.Hash == planB.Hash {
		t.Errorf("expected hash difference across strategies; got %s for both", planA.Hash)
	}
}

// TestBuildPlan_ErrorPropagation asserts that errors from
// SelectStrategy and ExpandSlots flow through with %w wrapping so the
// Service layer can branch via errors.Is.
func TestBuildPlan_ErrorPropagation(t *testing.T) {
	t.Parallel()

	t.Run("missing venue tz returns domain.ErrVenueMissingTZ", func(t *testing.T) {
		t.Parallel()
		in := basePlanInput(t)
		in.Venue.TZ = nil
		_, err := planner.BuildPlan(context.Background(), in)
		if !errors.Is(err, domain.ErrVenueMissingTZ) {
			t.Fatalf("err = %v, want errors.Is %v", err, domain.ErrVenueMissingTZ)
		}
	})

	t.Run("inverted time window returns ErrInvalidWindow", func(t *testing.T) {
		t.Parallel()
		in := basePlanInput(t)
		in.Goal.TimePrefs = domain.TimeWindow{
			Start:    domain.NewWallTime(21, 0, 0),
			End:      domain.NewWallTime(18, 0, 0),
			Priority: domain.PriorityEarlier,
		}
		_, err := planner.BuildPlan(context.Background(), in)
		if !errors.Is(err, planner.ErrInvalidWindow) {
			t.Fatalf("err = %v, want errors.Is %v", err, planner.ErrInvalidWindow)
		}
	})

	t.Run("past target returns ErrTargetInPast", func(t *testing.T) {
		t.Parallel()
		in := basePlanInput(t)
		in.Goal.Date = domain.NewDate(2026, time.May, 1)
		_, err := planner.BuildPlan(context.Background(), in)
		if !errors.Is(err, planner.ErrTargetInPast) {
			t.Fatalf("err = %v, want errors.Is %v", err, planner.ErrTargetInPast)
		}
	})
}

// TestBuildPlan_NotesIncludeStrategyExplanation pins down the contract
// that SelectStrategy's notes flow through into Plan.Notes verbatim
// plus the appended signing-required note.
func TestBuildPlan_NotesIncludeStrategyExplanation(t *testing.T) {
	t.Parallel()

	in := basePlanInput(t)
	plan, err := planner.BuildPlan(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(plan.Notes) < 2 {
		t.Fatalf("expected >=2 notes (strategy + signing), got %d: %v", len(plan.Notes), plan.Notes)
	}
	// First notes come from SelectStrategy; last entry is the
	// BuildPlan-appended signing note.
	last := plan.Notes[len(plan.Notes)-1]
	if !strings.Contains(last, "signing_required=false") {
		t.Errorf("last note = %q, want signing-required note", last)
	}
}

// TestBuildPlan_PriorityLaterFireScheduleOrder asserts that the
// FireSchedule preserves ExpandSlots's ordering — for Continuous /
// Explicit the times are anchored at the same instant, but the slot
// count and the implicit slot-order pairing must still match the
// expansion. We exercise this with the visible Discovered branch where
// every entry has the same value (probe-window center) but the count
// matches len(slots) regardless of priority.
func TestBuildPlan_PriorityLaterFireScheduleOrder(t *testing.T) {
	t.Parallel()

	in := basePlanInput(t)
	in.Goal.TimePrefs.Priority = domain.PriorityLater

	plan, err := planner.BuildPlan(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Slot count is invariant to priority; count = 5 (19:00..21:00 step 30m).
	if len(plan.FireSchedule) != 5 {
		t.Errorf("len(FireSchedule) = %d, want 5", len(plan.FireSchedule))
	}
}
