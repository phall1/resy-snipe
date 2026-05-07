package domain

import (
	"fmt"
	"slices"
)

// Status enumerates the lifecycle states of a SnipeState. The set is
// closed; new states require corresponding additions to allTransitions
// and the totality assertion in init().
type Status int

const (
	StatusSubmitted Status = iota
	StatusScheduled
	StatusDiscovering
	StatusAwaiting
	StatusFinding
	StatusBooking
	StatusBooked
	StatusFailed
	StatusCanceled
	StatusExpired
)

// AllStatuses returns every defined status, ordered by lifecycle.
// Used both internally (totality check) and externally (admin /
// debug tooling, exhaustiveness tests in store/engine).
func AllStatuses() []Status {
	return []Status{
		StatusSubmitted,
		StatusScheduled,
		StatusDiscovering,
		StatusAwaiting,
		StatusFinding,
		StatusBooking,
		StatusBooked,
		StatusFailed,
		StatusCanceled,
		StatusExpired,
	}
}

// IsTerminal reports whether s admits no outgoing transitions.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusBooked, StatusFailed, StatusCanceled, StatusExpired:
		return true
	default:
		return false
	}
}

// String returns the lowercase name (used in slog keys, store rows).
func (s Status) String() string {
	switch s {
	case StatusSubmitted:
		return "submitted"
	case StatusScheduled:
		return "scheduled"
	case StatusDiscovering:
		return "discovering"
	case StatusAwaiting:
		return "awaiting"
	case StatusFinding:
		return "finding"
	case StatusBooking:
		return "booking"
	case StatusBooked:
		return "booked"
	case StatusFailed:
		return "failed"
	case StatusCanceled:
		return "canceled"
	case StatusExpired:
		return "expired"
	default:
		return fmt.Sprintf("status(%d)", int(s))
	}
}

// allowedTransitions describes the directed graph of legal status
// changes. The outer key is "from"; the inner set is the legal set of
// "to" statuses. Terminal states are present with empty sets so the
// totality check in init() can verify every status is a key.
var allowedTransitions = map[Status]map[Status]struct{}{
	StatusSubmitted: {
		StatusScheduled:   {},
		StatusDiscovering: {},
		StatusCanceled:    {},
		StatusFailed:      {},
	},
	StatusScheduled: {
		StatusAwaiting:    {},
		StatusDiscovering: {},
		StatusCanceled:    {},
		StatusFailed:      {},
		StatusExpired:     {},
	},
	StatusDiscovering: {
		StatusAwaiting: {},
		StatusCanceled: {},
		StatusFailed:   {},
		StatusExpired:  {},
	},
	StatusAwaiting: {
		StatusFinding:  {},
		StatusCanceled: {},
		StatusFailed:   {},
		StatusExpired:  {},
	},
	StatusFinding: {
		StatusBooking:  {},
		StatusAwaiting: {},
		StatusCanceled: {},
		StatusFailed:   {},
		StatusExpired:  {},
	},
	StatusBooking: {
		StatusBooked:   {},
		StatusFinding:  {},
		StatusCanceled: {},
		StatusFailed:   {},
	},
	StatusBooked:   {},
	StatusFailed:   {},
	StatusCanceled: {},
	StatusExpired:  {},
}

// CanTransition reports whether moving from -> to is legal under the
// transition table. It is total: any (from, to) status pair returns a
// definite answer.
func CanTransition(from, to Status) bool {
	allowed, ok := allowedTransitions[from]
	if !ok {
		return false
	}
	_, ok = allowed[to]
	return ok
}

// init asserts that the transition table is total over the closed
// status set. A new status must be added to AllStatuses AND given an
// entry (possibly empty) in allowedTransitions, or this panics at
// program start.
func init() {
	statuses := AllStatuses()
	if len(allowedTransitions) != len(statuses) {
		panic(fmt.Sprintf(
			"domain: allowedTransitions has %d from-keys, expected %d",
			len(allowedTransitions), len(statuses),
		))
	}
	for _, from := range statuses {
		if _, ok := allowedTransitions[from]; !ok {
			panic(fmt.Sprintf("domain: allowedTransitions missing from-state %s", from))
		}
		for to := range allowedTransitions[from] {
			if !validStatus(to, statuses) {
				panic(fmt.Sprintf(
					"domain: allowedTransitions[%s] references unknown to-state %v",
					from, to,
				))
			}
		}
	}
	// Terminal statuses must have empty out-sets.
	for _, s := range statuses {
		if s.IsTerminal() && len(allowedTransitions[s]) != 0 {
			panic(fmt.Sprintf("domain: terminal status %s must have no outgoing transitions", s))
		}
	}
}

func validStatus(s Status, all []Status) bool {
	return slices.Contains(all, s)
}

// InvalidTransitionError is returned by SnipeState.Transition when the
// requested move is not allowed.
type InvalidTransitionError struct {
	From, To Status
}

func (e InvalidTransitionError) Error() string {
	return fmt.Sprintf("invalid status transition: %s -> %s", e.From, e.To)
}
