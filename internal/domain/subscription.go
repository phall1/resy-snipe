package domain

import (
	"errors"
	"fmt"
	"time"
)

type SubscriptionStatus int

const (
	SubscriptionActive SubscriptionStatus = iota
	SubscriptionPaused
	SubscriptionFulfilled
	SubscriptionExpired
	SubscriptionCancelled
)

func AllSubscriptionStatuses() []SubscriptionStatus {
	return []SubscriptionStatus{
		SubscriptionActive,
		SubscriptionPaused,
		SubscriptionFulfilled,
		SubscriptionExpired,
		SubscriptionCancelled,
	}
}

func (s SubscriptionStatus) IsTerminal() bool {
	switch s {
	case SubscriptionFulfilled, SubscriptionExpired, SubscriptionCancelled:
		return true
	default:
		return false
	}
}

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

func CanTransitionSubscription(from, to SubscriptionStatus) bool {
	allowed, ok := allowedSubscriptionTransitions[from]
	if !ok {
		return false
	}
	_, ok = allowed[to]
	return ok
}

type InvalidSubscriptionTransitionError struct {
	From, To SubscriptionStatus
}

func (e InvalidSubscriptionTransitionError) Error() string {
	return fmt.Sprintf("invalid subscription transition: %s -> %s", e.From, e.To)
}

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
			found := false
			for _, s := range statuses {
				if s == to {
					found = true
					break
				}
			}
			if !found {
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

type CompromisePolicy struct {
	TimeWindowMin time.Duration `json:"time_window_min"`
	TimeWindowMax time.Duration `json:"time_window_max"`
	PartySizeFlex int           `json:"party_size_flex"`
	TableTypeAny  bool          `json:"table_type_any"`
}

func (p CompromisePolicy) IsZero() bool {
	return p == CompromisePolicy{}
}

type Subscription struct {
	ID           SubscriptionID
	UserID       UserID
	Goal         Goal
	Status       SubscriptionStatus
	CreatedAt    time.Time
	ExpiresAt    *time.Time
	FulfilledBy  *QuestID
	Compromise   *CompromisePolicy
	PollInterval time.Duration
	NextPollAt   time.Time
}

func (s *Subscription) Transition(to SubscriptionStatus) error {
	if !CanTransitionSubscription(s.Status, to) {
		return InvalidSubscriptionTransitionError{From: s.Status, To: to}
	}
	s.Status = to
	return nil
}

func (s Subscription) Validate(now time.Time) error {
	if s.ID == "" {
		return errors.New("subscription: ID is required")
	}
	if s.UserID == "" {
		return errors.New("subscription: UserID is required")
	}
	if err := s.Goal.Validate(now); err != nil {
		return fmt.Errorf("subscription: %w", err)
	}
	if s.Status.String() == fmt.Sprintf("subscription_status(%d)", int(s.Status)) {
		return errors.New("subscription: invalid status")
	}
	if s.PollInterval < 0 {
		return errors.New("subscription: PollInterval must be non-negative")
	}
	if s.NextPollAt.IsZero() {
		return errors.New("subscription: NextPollAt is required")
	}
	return nil
}
