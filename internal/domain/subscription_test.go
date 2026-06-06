package domain

import (
	"errors"
	"testing"
	"time"
)

func TestSubscriptionStatus_String(t *testing.T) {
	cases := []struct {
		status SubscriptionStatus
		want   string
	}{
		{SubscriptionActive, "active"},
		{SubscriptionPaused, "paused"},
		{SubscriptionFulfilled, "fulfilled"},
		{SubscriptionExpired, "expired"},
		{SubscriptionCancelled, "cancelled"},
		{SubscriptionStatus(99), "subscription_status(99)"},
	}
	for _, c := range cases {
		if got := c.status.String(); got != c.want {
			t.Errorf("%d.String() = %q, want %q", c.status, got, c.want)
		}
	}
}

func TestCanTransitionSubscription(t *testing.T) {
	// Allowed transitions
	allowed := []struct{ from, to SubscriptionStatus }{
		{SubscriptionActive, SubscriptionPaused},
		{SubscriptionActive, SubscriptionFulfilled},
		{SubscriptionActive, SubscriptionExpired},
		{SubscriptionActive, SubscriptionCancelled},
		{SubscriptionPaused, SubscriptionActive},
		{SubscriptionPaused, SubscriptionCancelled},
	}
	for _, c := range allowed {
		if !CanTransitionSubscription(c.from, c.to) {
			t.Errorf("CanTransitionSubscription(%s, %s) = false, want true", c.from, c.to)
		}
	}

	// Forbidden transitions
	forbidden := []struct{ from, to SubscriptionStatus }{
		{SubscriptionFulfilled, SubscriptionActive},
		{SubscriptionExpired, SubscriptionActive},
		{SubscriptionCancelled, SubscriptionActive},
		{SubscriptionFulfilled, SubscriptionPaused},
		{SubscriptionPaused, SubscriptionFulfilled},
		{SubscriptionPaused, SubscriptionExpired},
	}
	for _, c := range forbidden {
		if CanTransitionSubscription(c.from, c.to) {
			t.Errorf("CanTransitionSubscription(%s, %s) = true, want false", c.from, c.to)
		}
	}
}

func TestSubscription_Transition(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	goal := validGoal(now)

	sub := &Subscription{
		ID:         "sub-1",
		UserID:     "user-1",
		Goal:       goal,
		Status:     SubscriptionActive,
		NextPollAt: now.Add(time.Hour),
	}

	// Success: active -> paused
	if err := sub.Transition(SubscriptionPaused); err != nil {
		t.Fatalf("Transition(Active->Paused) unexpected error: %v", err)
	}
	if sub.Status != SubscriptionPaused {
		t.Errorf("Status = %s, want %s", sub.Status, SubscriptionPaused)
	}

	// Success: paused -> active
	if err := sub.Transition(SubscriptionActive); err != nil {
		t.Fatalf("Transition(Paused->Active) unexpected error: %v", err)
	}

	// Failure: active -> fulfilled -> active
	if err := sub.Transition(SubscriptionFulfilled); err != nil {
		t.Fatalf("Transition(Active->Fulfilled) unexpected error: %v", err)
	}
	err := sub.Transition(SubscriptionActive)
	var invErr InvalidSubscriptionTransitionError
	if !errors.As(err, &invErr) {
		t.Fatalf("Transition(Fulfilled->Active) error type = %T, want InvalidSubscriptionTransitionError", err)
	}
	if invErr.From != SubscriptionFulfilled || invErr.To != SubscriptionActive {
		t.Errorf("InvalidSubscriptionTransitionError = %s -> %s, want %s -> %s",
			invErr.From, invErr.To, SubscriptionFulfilled, SubscriptionActive)
	}
}

func TestSubscription_Validate(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	goal := validGoal(now)
	validSub := Subscription{
		ID:           "sub-1",
		UserID:       "user-1",
		Goal:         goal,
		Status:       SubscriptionActive,
		PollInterval: time.Minute,
		NextPollAt:   now.Add(time.Hour),
	}

	if err := validSub.Validate(now); err != nil {
		t.Fatalf("valid subscription.Validate() unexpected error: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*Subscription)
		wantErr string
	}{
		{
			name: "missing ID",
			mutate: func(s *Subscription) {
				s.ID = ""
			},
			wantErr: "subscription: ID is required",
		},
		{
			name: "missing UserID",
			mutate: func(s *Subscription) {
				s.UserID = ""
			},
			wantErr: "subscription: UserID is required",
		},
		{
			name: "invalid goal",
			mutate: func(s *Subscription) {
				s.Goal.Party = 0
			},
			wantErr: "subscription: goal: party must be positive",
		},
		{
			name: "invalid status",
			mutate: func(s *Subscription) {
				s.Status = SubscriptionStatus(99)
			},
			wantErr: "subscription: invalid status",
		},
		{
			name: "negative PollInterval",
			mutate: func(s *Subscription) {
				s.PollInterval = -time.Minute
			},
			wantErr: "subscription: PollInterval must be non-negative",
		},
		{
			name: "missing NextPollAt",
			mutate: func(s *Subscription) {
				s.NextPollAt = time.Time{}
			},
			wantErr: "subscription: NextPollAt is required",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validSub
			c.mutate(&s)
			err := s.Validate(now)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if err.Error() != c.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), c.wantErr)
			}
		})
	}
}

func validGoal(now time.Time) Goal {
	return Goal{
		VenueQuery: VenueQuerySlug{Slug: "test-venue", City: "nyc"},
		Date:       NewDate(now.Year(), now.Month(), now.Day()+1),
		Party:      2,
		TimePrefs: TimeWindow{
			Start: NewWallTime(18, 0, 0),
			End:   NewWallTime(20, 0, 0),
		},
		AccountID: "acct-1",
	}
}
