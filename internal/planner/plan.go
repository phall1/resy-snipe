package planner

import (
	"context"
	"fmt"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// PlanInput bundles every signal BuildPlan reads. It mirrors
// StrategyInput but folds in the Goal-level fields (TimePrefs,
// Constraints) that slot expansion needs and the wall-clock data the
// FireSchedule projection needs.
//
// The Service layer is responsible for fetching CalendarSnapshot and
// looking up ObservedReleaseAvg / ReleaseConfig from its caches; the
// planner is pure given these inputs (laws.md §Layering rule 7 — no
// time.Now() outside internal/clock; the caller supplies Now).
type PlanInput struct {
	// Goal is the user's target. The planner echoes it into Plan.Goal
	// verbatim — Goal normalization is the Service layer's job.
	Goal domain.Goal
	// Venue is the resolved venue; Venue.TZ must be non-nil.
	Venue domain.Venue
	// Now is the wall-clock instant the decision is taken at. Production
	// code passes clock.Clock.Now(); tests pass a fixed instant.
	Now time.Time
	// CalendarSnapshot is the result of providers.Provider.Calendar for
	// a date range covering Goal.Date. The zero value means "no data".
	CalendarSnapshot providers.Calendar
	// ObservedReleaseAvg is the prior-observed release instant for this
	// venue (UTC). When non-nil it tilts strategy selection toward
	// Explicit.
	ObservedReleaseAvg *time.Time
	// ReleaseConfig, when non-zero, is the operator-supplied prior on
	// the venue's release_days_ahead / release_time_local.
	ReleaseConfig *VenueReleaseConfig
	// ContinuousUntil, when non-zero, caps the engine's
	// cancellation-chase window. Forwarded to SelectStrategy.
	ContinuousUntil time.Time
}

// BuildPlan composes strategy selection, slot expansion, and the
// FireSchedule projection into a single hash-pinned domain.Plan, per
// design/planner.md §plan-shape.
//
// Algorithm:
//
//  1. Call SelectStrategy with the input's strategy-relevant fields.
//     A nil/error return propagates immediately.
//  2. Expand the goal's TimeWindow + Constraints into the slot list via
//     ExpandSlots.
//  3. Project the slot list onto wall-clock fire times in
//     Goal.Date+venue.TZ → UTC. The projection rule depends on the
//     strategy variant (see fireScheduleFor).
//  4. Compute SigningRequired. v1 lacks a domain.Venue field that would
//     surface peak-venue / signing hints, so the planner defaults to
//     false and emits a Note acknowledging the limitation.
//  5. Assemble the Plan, set PlanHashVersion=PlanHashVersionV1 and
//     ComputedAt=Now, and call Plan.WithHash to compute the
//     content-addressable Hash.
//
// BuildPlan is a pure function — no I/O, no clock reads beyond the
// supplied Now, no goroutines. The hash is reproducible: re-running
// BuildPlan with the same input (same Now or any later instant whose
// strategy decision matches) yields the same Plan.Hash, because
// CanonicalBytes excludes the volatile Notes/ComputedAt/Hash fields
// (see domain/plan.go canonicalPayload).
func BuildPlan(ctx context.Context, in PlanInput) (domain.Plan, error) {
	// Step 1 — strategy selection.
	stratIn := StrategyInput{
		Goal:               in.Goal,
		Venue:              in.Venue,
		Now:                in.Now,
		CalendarSnapshot:   in.CalendarSnapshot,
		ObservedReleaseAvg: in.ObservedReleaseAvg,
		ContinuousUntil:    in.ContinuousUntil,
	}
	if in.ReleaseConfig != nil {
		stratIn.ReleaseConfig = *in.ReleaseConfig
	}

	strategy, notes, err := SelectStrategy(ctx, stratIn)
	if err != nil {
		return domain.Plan{}, fmt.Errorf("planner.BuildPlan: %w", err)
	}

	// Step 2 — slot expansion.
	slots, err := ExpandSlots(in.Goal.TimePrefs, in.Goal.Constraints)
	if err != nil {
		return domain.Plan{}, fmt.Errorf("planner.BuildPlan: %w", err)
	}

	// Step 3 — fire-schedule projection.
	dropMoment, fireSchedule := fireScheduleFor(strategy, slots, in.Goal.Date, in.Venue.TZ, in.Now)

	// Step 4 — signing-required heuristic. domain.Venue currently has no
	// peak-venue indicator; fall back to false and surface the
	// limitation in Notes per the design's "best-effort hint" stance.
	signingRequired := false
	notes = append(notes,
		"signing_required=false (v1: domain.Venue carries no peak-venue indicator; signer adapter is consulted live at fire time)")

	// Step 5 — assemble Plan + compute hash.
	plan := domain.Plan{
		Goal:            in.Goal,
		Venue:           in.Venue,
		DropMoment:      dropMoment.UTC(),
		Strategy:        strategy,
		FireSchedule:    fireSchedule,
		SigningRequired: signingRequired,
		Notes:           notes,
		PlanHashVersion: domain.PlanHashVersionV1,
		ComputedAt:      in.Now.UTC(),
	}

	hashed, err := plan.WithHash()
	if err != nil {
		return domain.Plan{}, fmt.Errorf("planner.BuildPlan: %w", err)
	}
	return hashed, nil
}

// fireScheduleFor returns (DropMoment, FireSchedule) for the chosen
// strategy. The DropMoment is what the engine wakes up on; the
// FireSchedule is the ordered sequence of wall-clock attempts the
// engine will issue once the DropMoment passes.
//
// Strategy-specific projections (per design/planner.md §plan-shape and
// §strategy-selection):
//
//   - ExplicitRelease: every slot fires at strategy.At. The engine's
//     race fans them out per BookingPolicy from that single instant.
//   - ContinuousRelease: DropMoment = now (we are polling immediately).
//     For v1 the FireSchedule mirrors the slot list with each entry's
//     fire time set to DropMoment as a placeholder — the design notes
//     that for Continuous the engine fires on every Find hit, so the
//     schedule is approximate. A Note in BuildPlan documents this.
//   - DiscoveredRelease: DropMoment = ProbeFrom (the moment the engine
//     starts polling). FireSchedule mirrors the slot list at the
//     midpoint of the probe window; the engine records the actual
//     observed instant when it hits.
func fireScheduleFor(
	strategy domain.ReleaseStrategy,
	slots []domain.SlotPreference,
	date domain.Date,
	tz *time.Location,
	now time.Time,
) (dropMoment time.Time, schedule []time.Time) {
	switch v := strategy.(type) {
	case domain.ExplicitRelease:
		// Explicit: every slot fires at the same drop instant. We still
		// project each slot's wall-clock time onto Goal.Date+tz so a
		// future engine version that wants per-slot offsets has the
		// information; v1 callers ignore individual entries beyond their
		// count. The schedule's *length* equals len(slots) so the
		// engine's race-ordering invariant is preserved.
		_ = projectSlots(slots, date, tz) // computed for symmetry; unused below
		schedule := make([]time.Time, len(slots))
		for i := range slots {
			schedule[i] = v.At.UTC()
		}
		return v.At.UTC(), schedule

	case domain.ContinuousRelease:
		// Continuous: DropMoment is "now" (we begin polling
		// immediately). FireSchedule is approximate — the engine fires
		// on every Find hit, not on a precomputed schedule — so we emit
		// each slot at the same DropMoment to convey the expected
		// fan-out shape.
		drop := now.UTC()
		schedule := make([]time.Time, len(slots))
		for i := range slots {
			schedule[i] = drop
		}
		return drop, schedule

	case domain.DiscoveredRelease:
		// Discovered: DropMoment is the start of the probe window (the
		// moment the engine begins polling the calendar). FireSchedule
		// is approximate — the engine records the actual observed
		// instant when it sees the target date open — so we anchor each
		// entry at the probe-window center as a documented placeholder.
		center := v.ProbeFrom.Add(v.ProbeUntil.Sub(v.ProbeFrom) / 2).UTC()
		schedule := make([]time.Time, len(slots))
		for i := range slots {
			schedule[i] = center
		}
		return v.ProbeFrom.UTC(), schedule

	default:
		// Defensive: an unknown strategy variant should never reach
		// BuildPlan — SelectStrategy is exhaustive over the sealed
		// ReleaseStrategy union — but we degrade gracefully by emitting
		// an empty schedule rather than panicking.
		return now.UTC(), nil
	}
}

// projectSlots maps each SlotPreference's wall-clock Time onto Goal.Date
// in the venue's timezone, returning UTC instants. Used by the Explicit
// branch of fireScheduleFor as a per-slot anchor; v1 callers do not
// observe the result, but keeping the helper exported (within the
// package) means a future BookingPolicy that wants per-slot offsets has
// a single conversion site.
func projectSlots(slots []domain.SlotPreference, date domain.Date, tz *time.Location) []time.Time {
	if tz == nil {
		return nil
	}
	out := make([]time.Time, len(slots))
	for i, s := range slots {
		out[i] = time.Date(
			date.Year, date.Month, date.Day,
			s.Time.Hour, s.Time.Minute, s.Time.Second,
			0, tz,
		).UTC()
	}
	return out
}
