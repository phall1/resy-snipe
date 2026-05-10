package planner

import "errors"

// Sentinel errors returned by SelectStrategy. The Service layer
// branches on these via errors.Is per laws.md §Errors; never replace
// with bare fmt.Errorf.
var (
	// ErrTargetInPast is returned when Goal.Date is before today as
	// observed from StrategyInput.Now in the venue's timezone. The
	// planner refuses to build a strategy for a past date; the Service
	// layer surfaces the error to the caller.
	ErrTargetInPast = errors.New("planner: target date is in the past")

	// ErrVenueClosed is returned when the resolved Venue carries a
	// signal indicating it is not currently accepting reservations
	// (e.g. seasonal closure, permanent shutdown). M1-06 surfaces this
	// when the calendar snapshot covers the target date and explicitly
	// marks the entire window as closed; later milestones may key on
	// richer signals from the resolver.
	ErrVenueClosed = errors.New("planner: venue is not accepting reservations")

	// ErrNoStrategyAvailable is the defensive catch-all returned when
	// none of the three rules in §strategy-selection apply. Under the
	// current rule set this should never fire — Discovered is the
	// fallback when no firmer signal exists — but the sentinel exists
	// so future rule changes can fail loudly rather than panic.
	ErrNoStrategyAvailable = errors.New("planner: no release strategy applies")
)
