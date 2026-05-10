package planner

import (
	"errors"
	"fmt"

	"resy-snipe/internal/domain"
)

// SlotStepV1 is the fixed slot-time interval the v1 planner uses when
// expanding a TimeWindow into discrete SlotPreference candidates. Every
// 30 minutes between window.Start and window.End (inclusive on both
// ends) becomes a candidate time.
//
// The design (docs/v2/design/planner.md §slot-expansion) anticipates a
// future Step field on domain.TimeWindow that would let users override
// this. Until that field exists the v1 planner is intentionally rigid:
// 30 minutes matches Resy's own slot grid for the overwhelming majority
// of venues, and a fixed step keeps Plan.Hash deterministic across SDK
// upgrades. Bumping this value is a hash-breaking change and would need
// a Plan canonicalization version bump (PlanHashVersion).
const SlotStepV1 = 30

// ErrInvalidWindow is returned by ExpandSlots when the supplied
// TimeWindow has Start > End. The Service layer's input validation
// catches this at Goal construction (domain.ErrGoalTimeWindowInverted),
// but the planner enforces the invariant a second time so the function
// is safe to call directly with operator-supplied data.
var ErrInvalidWindow = errors.New("planner: invalid time window")

// ExpandSlots turns a TimeWindow plus the user's table-type constraints
// into the ordered slice of SlotPreference values an Intent will carry.
// The order is the engine's race order: head-to-tail in
// Intent.SlotPrefs maps to first-attempt-first in /3/details and /3/book
// per the planner.md §slot-expansion contract.
//
// Algorithm (per design/planner.md §slot-expansion):
//
//  1. Generate slot candidate times every SlotStepV1 minutes from
//     window.Start to window.End inclusive of both endpoints.
//  2. Cross-product each time with the table-type list:
//     - constraints.AnyTable=true       → one entry, TableType="".
//     - constraints.TableTypes empty    → one entry, TableType="".
//     - constraints.TableTypes non-empty → one entry per table type, in
//     the user-supplied order.
//  3. Order the time axis according to window.Priority:
//     - PriorityEarlier or PriorityNone → ascending by time (earliest first).
//     - PriorityLater                   → descending by time (latest first).
//
// Edge cases:
//   - Start == End emits a single candidate time at that wall clock.
//   - Start  > End returns ErrInvalidWindow (wrapped with the offending
//     bounds for debuggability).
//   - A 30-minute step that would step past End is truncated; the final
//     candidate is the largest multiple of 30 minutes from Start that
//     is ≤ End. Combined with the inclusive-on-End rule, a window of
//     18:30..21:00 emits 18:30, 19:00, 19:30, 20:00, 20:30, 21:00.
//
// ExpandSlots is a pure function: no I/O, no clock reads, no goroutines.
func ExpandSlots(window domain.TimeWindow, constraints domain.Constraints) ([]domain.SlotPreference, error) {
	if wallTimeAfterPub(window.Start, window.End) {
		return nil, fmt.Errorf("%w: start=%s end=%s",
			ErrInvalidWindow, window.Start, window.End)
	}

	times := enumerateTimes(window)

	// Normalize the table-type axis to a non-empty slice. Empty or
	// AnyTable both collapse to a single anonymous entry so the
	// cross-product below is uniform.
	tables := normalizeTables(constraints)

	prefs := make([]domain.SlotPreference, 0, len(times)*len(tables))
	for _, t := range times {
		for _, tbl := range tables {
			prefs = append(prefs, domain.SlotPreference{
				Time:      t,
				TableType: tbl,
			})
		}
	}
	return prefs, nil
}

// enumerateTimes walks Start..End at SlotStepV1-minute intervals and
// orders the result per window.Priority. The walk uses minute-of-day
// arithmetic so it is independent of any time.Location.
func enumerateTimes(window domain.TimeWindow) []domain.WallTime {
	startMin := minutesOfDay(window.Start)
	endMin := minutesOfDay(window.End)

	// Single-point window: Start == End produces exactly one candidate.
	if startMin == endMin {
		return []domain.WallTime{wallTimeFromMinutes(startMin, window.Start.Second)}
	}

	// Walk forward in 30-minute steps; truncate at End. We work in
	// ascending order regardless of Priority and reverse afterward when
	// PriorityLater is requested — keeping the walk monotonic avoids
	// off-by-one errors when (End - Start) is not a multiple of the
	// step.
	count := (endMin-startMin)/SlotStepV1 + 1
	asc := make([]domain.WallTime, 0, count)
	for m := startMin; m <= endMin; m += SlotStepV1 {
		// Seconds are inherited from window.Start so an operator setting
		// 18:30:30..21:00:30 still gets sub-minute fidelity. WallTime
		// does not currently carry sub-second precision.
		asc = append(asc, wallTimeFromMinutes(m, window.Start.Second))
	}

	switch window.Priority {
	case domain.PriorityLater:
		// Reverse in place — we own the slice.
		for i, j := 0, len(asc)-1; i < j; i, j = i+1, j-1 {
			asc[i], asc[j] = asc[j], asc[i]
		}
		return asc
	default:
		// PriorityEarlier and PriorityNone both produce ascending order;
		// the design (planner.md §slot-expansion) treats PriorityNone as
		// a planner-default hint and the v1 default is "ascending by
		// time" to match the most common user expectation.
		return asc
	}
}

// normalizeTables collapses Constraints.TableTypes plus the AnyTable
// override into the table-type axis ExpandSlots iterates. AnyTable=true
// always wins regardless of TableTypes — it is the user's explicit
// "ignore my prior preferences" knob. An empty TableTypes list also
// falls back to a single anonymous entry so the cross-product is
// uniform.
func normalizeTables(c domain.Constraints) []string {
	if c.AnyTable || len(c.TableTypes) == 0 {
		return []string{""}
	}
	// Preserve user-supplied order. Defensive copy so callers cannot
	// observe slice aliasing through the returned SlotPreferences.
	out := make([]string, len(c.TableTypes))
	copy(out, c.TableTypes)
	return out
}

// minutesOfDay converts a WallTime to the integer "minutes since
// 00:00", ignoring the seconds component for stepping purposes. The
// caller threads seconds through wallTimeFromMinutes so sub-minute
// precision is preserved.
func minutesOfDay(w domain.WallTime) int { return w.Hour*60 + w.Minute }

// wallTimeFromMinutes is the inverse of minutesOfDay; it reconstructs a
// WallTime from a minute offset and an explicit seconds value.
func wallTimeFromMinutes(m, seconds int) domain.WallTime {
	return domain.WallTime{
		Hour:   m / 60,
		Minute: m % 60,
		Second: seconds,
	}
}

// wallTimeAfterPub mirrors the unexported domain.wallTimeAfter helper.
// We re-derive it here so the planner does not import an unexported
// domain helper; the comparison is a small, stable predicate.
func wallTimeAfterPub(a, b domain.WallTime) bool {
	if a.Hour != b.Hour {
		return a.Hour > b.Hour
	}
	if a.Minute != b.Minute {
		return a.Minute > b.Minute
	}
	return a.Second > b.Second
}
