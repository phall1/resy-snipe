package domain

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// SubscriptionStatus enumerates the lifecycle states of a Subscription.
// The set is closed; new states require corresponding additions to
// allowedSubscriptionTransitions and the totality assertion in init().
type SubscriptionStatus int

const (
	// SubscriptionActive means the subscription is live and polling.
	SubscriptionActive SubscriptionStatus = iota
	// SubscriptionPaused means polling is temporarily stopped.
	SubscriptionPaused
	// SubscriptionFulfilled means the goal was achieved.
	SubscriptionFulfilled
	// SubscriptionExpired means the subscription reached its deadline
	// without fulfillment.
	SubscriptionExpired
	// SubscriptionCancelled means the user explicitly cancelled.
	SubscriptionCancelled
)

// AllSubscriptionStatuses returns every defined status, ordered by
// lifecycle. Used both internally (totality check) and externally
// (admin / debug tooling, exhaustiveness tests).
func AllSubscriptionStatuses() []SubscriptionStatus {
	return []SubscriptionStatus{
		SubscriptionActive,
		SubscriptionPaused,
		SubscriptionFulfilled,
		SubscriptionExpired,
		SubscriptionCancelled,
	}
}

// IsTerminal reports whether s admits no outgoing transitions.
func (s SubscriptionStatus) IsTerminal() bool {
	switch s {
	case SubscriptionFulfilled, SubscriptionExpired, SubscriptionCancelled:
		return true
	default:
		return false
	}
}

// String returns the lowercase name (used in slog keys, store rows).
func (s SubscriptionStatus) String() string {
	switch s {
	case SubscriptionActive:
		return "active"
	case SubscriptionPaused:
		return "paused"
	case SubscriptionFulfilled:
		return "fulfilled"
	case SubscriptionExpired:
		return "expired"
	case SubscriptionCancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("subscription_status(%d)", int(s))
	}
}

// allowedSubscriptionTransitions describes the directed graph of legal
// subscription status changes. The outer key is "from"; the inner set
// is the legal set of "to" statuses. Terminal states are present with
// empty sets so the totality check in init() can verify every status is
// a key.
var allowedSubscriptionTransitions = map[SubscriptionStatus]map[SubscriptionStatus]struct{}{
	SubscriptionActive: {
		SubscriptionPaused:    {},
		SubscriptionFulfilled: {},
		SubscriptionExpired:   {},
		SubscriptionCancelled: {},
	},
	SubscriptionPaused: {
		SubscriptionActive:    {},
		SubscriptionCancelled: {},
	},
	SubscriptionFulfilled: {},
	SubscriptionExpired:   {},
	SubscriptionCancelled: {},
}

// CanTransitionSubscription reports whether moving from -> to is legal
// under the transition table. It is total: any (from, to) status pair
// returns a definite answer.
func CanTransitionSubscription(from, to SubscriptionStatus) bool {
	allowed, ok := allowedSubscriptionTransitions[from]
	if !ok {
		return false
	}
	_, ok = allowed[to]
	return ok
}

// Sentinel validation errors for Subscription. The Service layer relies
// on errors.Is to branch; never replace with a bare fmt.Errorf.
var (
	ErrSubscriptionIDMissing            = errors.New("subscription: ID is required")
	ErrSubscriptionUserMissing          = errors.New("subscription: UserID is required")
	ErrSubscriptionStatusInvalid        = errors.New("subscription: invalid status")
	ErrSubscriptionPollIntervalNegative = errors.New("subscription: PollInterval must be non-negative")
	ErrSubscriptionNextPollAtMissing    = errors.New("subscription: NextPollAt is required")
)

// InvalidSubscriptionTransitionError is returned by
// Subscription.Transition when the requested move is not allowed.
type InvalidSubscriptionTransitionError struct {
	From, To SubscriptionStatus
}

// Error returns a human-readable description of the invalid transition.
func (e InvalidSubscriptionTransitionError) Error() string {
	return fmt.Sprintf("invalid subscription transition: %s -> %s", e.From, e.To)
}

// init asserts that the transition table is total over the closed status
// set. A new status must be added to AllSubscriptionStatuses AND given
// an entry (possibly empty) in allowedSubscriptionTransitions, or this
// panics at program start.
func init() {
	statuses := AllSubscriptionStatuses()
	if len(allowedSubscriptionTransitions) != len(statuses) {
		panic(fmt.Sprintf(
			"domain: allowedSubscriptionTransitions has %d from-keys, expected %d",
			len(allowedSubscriptionTransitions), len(statuses),
		))
	}
	for _, from := range statuses {
		if _, ok := allowedSubscriptionTransitions[from]; !ok {
			panic(fmt.Sprintf("domain: allowedSubscriptionTransitions missing from-state %s", from))
		}
		for to := range allowedSubscriptionTransitions[from] {
			if !validSubscriptionStatus(to, statuses) {
				panic(fmt.Sprintf(
					"domain: allowedSubscriptionTransitions[%s] references unknown to-state %v",
					from, to,
				))
			}
		}
	}
	for _, s := range statuses {
		if s.IsTerminal() && len(allowedSubscriptionTransitions[s]) != 0 {
			panic(fmt.Sprintf("domain: terminal subscription status %s must have no outgoing transitions", s))
		}
	}
}

// CompromisePolicy controls how strictly the engine matches a goal when
// the ideal reservation is unavailable.
type CompromisePolicy struct {
	// TimeWindowMin is the smallest acceptable time window expansion.
	TimeWindowMin time.Duration `json:"time_window_min"`
	// TimeWindowMax is the largest acceptable time window expansion.
	TimeWindowMax time.Duration `json:"time_window_max"`
	// PartySizeFlex is the maximum party size deviation allowed.
	PartySizeFlex int `json:"party_size_flex"`
	// TableTypeAny, when true, allows any table type regardless of
	// Constraints.TableTypes.
	TableTypeAny bool `json:"table_type_any"`
}

// Subscription is the persisted record of a user's ongoing reservation
// hunt. The engine polls for matching slots until the subscription is
// fulfilled, expires, or is cancelled.
type Subscription struct {
	// ID is the unique subscription identifier.
	ID SubscriptionID
	// UserID is the owner of the subscription.
	UserID UserID
	// Goal is the reservation criteria being hunted.
	Goal Goal
	// Status is the current lifecycle state.
	Status SubscriptionStatus
	// CreatedAt is when the subscription was first persisted.
	CreatedAt time.Time
	// ExpiresAt, when non-nil, is the deadline after which the
	// subscription may transition to Expired.
	ExpiresAt *time.Time
	// FulfilledBy references the Quest that satisfied the goal.
	FulfilledBy *QuestID
	// Compromise, when non-nil, enables relaxed matching.
	Compromise *CompromisePolicy
	// PollInterval is the minimum duration between polls.
	PollInterval time.Duration
	// NextPollAt is the scheduled time of the next poll.
	NextPollAt time.Time
}

// Transition moves the subscription to a new status if the transition is
// legal. It returns an InvalidSubscriptionTransitionError when the move
// is disallowed.
func (s *Subscription) Transition(to SubscriptionStatus) error {
	if !CanTransitionSubscription(s.Status, to) {
		return InvalidSubscriptionTransitionError{From: s.Status, To: to}
	}
	s.Status = to
	return nil
}

func validSubscriptionStatus(s SubscriptionStatus, all []SubscriptionStatus) bool {
	return slices.Contains(all, s)
}

// Validate enforces the invariants the engine and store expect. The
// caller supplies "now" so domain remains time-source-agnostic.
func (s Subscription) Validate(now time.Time) error {
	if s.ID == "" {
		return ErrSubscriptionIDMissing
	}
	if s.UserID == "" {
		return ErrSubscriptionUserMissing
	}
	if err := s.Goal.Validate(now); err != nil {
		return fmt.Errorf("subscription: %w", err)
	}
	if !validSubscriptionStatus(s.Status, AllSubscriptionStatuses()) {
		return ErrSubscriptionStatusInvalid
	}
	if s.PollInterval < 0 {
		return ErrSubscriptionPollIntervalNegative
	}
	if s.NextPollAt.IsZero() {
		return ErrSubscriptionNextPollAtMissing
	}
	return nil
}
