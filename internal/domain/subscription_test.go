package domain_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"resy-snipe/internal/domain"
)

func TestSubscriptionStatus_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status domain.SubscriptionStatus
		want   string
	}{
		{domain.SubscriptionActive, "active"},
		{domain.SubscriptionPaused, "paused"},
		{domain.SubscriptionFulfilled, "fulfilled"},
		{domain.SubscriptionExpired, "expired"},
		{domain.SubscriptionCancelled, "cancelled"},
		{domain.SubscriptionStatus(99), "subscription_status(99)"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			t.Parallel()
			if got := c.status.String(); got != c.want {
				t.Errorf("%d.String() = %q, want %q", c.status, got, c.want)
			}
		})
	}
}

func TestCanTransitionSubscription(t *testing.T) {
	t.Parallel()
	// Allowed transitions
	allowed := []struct{ from, to domain.SubscriptionStatus }{
		{domain.SubscriptionActive, domain.SubscriptionPaused},
		{domain.SubscriptionActive, domain.SubscriptionFulfilled},
		{domain.SubscriptionActive, domain.SubscriptionExpired},
		{domain.SubscriptionActive, domain.SubscriptionCancelled},
		{domain.SubscriptionPaused, domain.SubscriptionActive},
		{domain.SubscriptionPaused, domain.SubscriptionCancelled},
	}
	for _, c := range allowed {
		t.Run(fmt.Sprintf("%s->%s", c.from, c.to), func(t *testing.T) {
			t.Parallel()
			if !domain.CanTransitionSubscription(c.from, c.to) {
				t.Errorf("CanTransitionSubscription(%s, %s) = false, want true", c.from, c.to)
			}
		})
	}

	// Forbidden transitions
	forbidden := []struct{ from, to domain.SubscriptionStatus }{
		{domain.SubscriptionFulfilled, domain.SubscriptionActive},
		{domain.SubscriptionExpired, domain.SubscriptionActive},
		{domain.SubscriptionCancelled, domain.SubscriptionActive},
		{domain.SubscriptionFulfilled, domain.SubscriptionPaused},
		{domain.SubscriptionPaused, domain.SubscriptionFulfilled},
		{domain.SubscriptionPaused, domain.SubscriptionExpired},
	}
	for _, c := range forbidden {
		t.Run(fmt.Sprintf("%s->%s", c.from, c.to), func(t *testing.T) {
			t.Parallel()
			if domain.CanTransitionSubscription(c.from, c.to) {
				t.Errorf("CanTransitionSubscription(%s, %s) = true, want false", c.from, c.to)
			}
		})
	}
}

func TestSubscription_Transition(t *testing.T) {
	t.Parallel()
	t.Run("active to fulfilled", func(t *testing.T) {
		t.Parallel()
		s := domain.Subscription{Status: domain.SubscriptionActive}
		if err := s.Transition(domain.SubscriptionFulfilled); err != nil {
			t.Fatalf("transition failed: %v", err)
		}
		if s.Status != domain.SubscriptionFulfilled {
			t.Errorf("status = %s, want fulfilled", s.Status)
		}
	})
	t.Run("fulfilled to active should fail", func(t *testing.T) {
		t.Parallel()
		s := domain.Subscription{Status: domain.SubscriptionFulfilled}
		err := s.Transition(domain.SubscriptionActive)
		if err == nil {
			t.Fatal("fulfilled -> active should fail")
		}
		var invalidErr domain.InvalidSubscriptionTransitionError
		if !errors.As(err, &invalidErr) {
			t.Errorf("expected InvalidSubscriptionTransitionError, got %T", err)
		}
		// Status must not have changed on a rejected transition.
		if s.Status != domain.SubscriptionFulfilled {
			t.Fatalf("Status mutated after rejected transition: %s", s.Status)
		}
	})
}

func TestSubscription_Validate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	goal := makeValidGoal(now)
	validSub := domain.Subscription{
		ID:           "sub-1",
		UserID:       "user-1",
		Goal:         goal,
		Status:       domain.SubscriptionActive,
		PollInterval: time.Minute,
		NextPollAt:   now.Add(time.Hour),
	}

	if err := validSub.Validate(now); err != nil {
		t.Fatalf("valid subscription.Validate() unexpected error: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*domain.Subscription)
		wantErr error
	}{
		{
			name: "missing ID",
			mutate: func(s *domain.Subscription) {
				s.ID = ""
			},
			wantErr: domain.ErrSubscriptionIDMissing,
		},
		{
			name: "missing UserID",
			mutate: func(s *domain.Subscription) {
				s.UserID = ""
			},
			wantErr: domain.ErrSubscriptionUserMissing,
		},
		{
			name: "invalid goal",
			mutate: func(s *domain.Subscription) {
				s.Goal.Party = 0
			},
			wantErr: domain.ErrGoalPartyNonPositive,
		},
		{
			name: "invalid status",
			mutate: func(s *domain.Subscription) {
				s.Status = domain.SubscriptionStatus(99)
			},
			wantErr: domain.ErrSubscriptionStatusInvalid,
		},
		{
			name: "negative PollInterval",
			mutate: func(s *domain.Subscription) {
				s.PollInterval = -time.Minute
			},
			wantErr: domain.ErrSubscriptionPollIntervalNegative,
		},
		{
			name: "missing NextPollAt",
			mutate: func(s *domain.Subscription) {
				s.NextPollAt = time.Time{}
			},
			wantErr: domain.ErrSubscriptionNextPollAtMissing,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := validSub
			c.mutate(&s)
			err := s.Validate(now)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, c.wantErr) {
				t.Errorf("expected %v, got %v", c.wantErr, err)
			}
		})
	}
}

func makeValidGoal(now time.Time) domain.Goal {
	return domain.Goal{
		VenueQuery: domain.VenueQuerySlug{Slug: "test-venue", City: "nyc"},
		Date:       domain.NewDate(now.Year(), now.Month(), now.Day()+1),
		Party:      2,
		TimePrefs: domain.TimeWindow{
			Start: domain.NewWallTime(18, 0, 0),
			End:   domain.NewWallTime(20, 0, 0),
		},
		AccountID: "acct-1",
	}
}
