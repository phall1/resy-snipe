package providers_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// stubProvider exists only to compile-check the Provider interface
// shape. Engine code is built against this interface; if a method
// signature drifts, this file fails to compile.
type stubProvider struct{}

func (stubProvider) ID() domain.ProviderID { return "stub" }
func (stubProvider) Login(context.Context, providers.Credentials) (providers.Session, error) {
	return nil, providers.ErrAuthExpired
}
func (stubProvider) Ping(context.Context, providers.Session) error { return nil }
func (stubProvider) SearchVenues(context.Context, providers.Query) ([]domain.Venue, error) {
	return nil, nil
}
func (stubProvider) Calendar(context.Context, domain.VenueRef, providers.DateRange) (providers.Calendar, error) {
	return providers.Calendar{}, nil
}
func (stubProvider) Find(context.Context, providers.FindRequest) ([]providers.Slot, error) {
	return nil, providers.ErrInventoryEmpty
}
func (stubProvider) Book(context.Context, providers.Slot, providers.Session) (providers.Confirmation, error) {
	return providers.Confirmation{}, providers.ErrSlotTaken
}

var _ providers.Provider = stubProvider{}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	t.Parallel()
	all := []error{
		providers.ErrAuthExpired,
		providers.ErrMFARequired,
		providers.ErrBookTokenExpired,
		providers.ErrSlotTaken,
		providers.ErrRateLimited,
		providers.ErrAntiBotChallenge,
		providers.ErrInventoryEmpty,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("sentinel collision: %v Is %v", a, b)
			}
		}
	}
}

func TestSentinelClassificationThroughWrap(t *testing.T) {
	t.Parallel()
	cases := []error{
		providers.ErrAuthExpired,
		providers.ErrMFARequired,
		providers.ErrBookTokenExpired,
		providers.ErrSlotTaken,
		providers.ErrRateLimited,
		providers.ErrAntiBotChallenge,
		providers.ErrInventoryEmpty,
	}
	for _, sentinel := range cases {
		wrapped := fmt.Errorf("calling /3/book: %w", sentinel)
		if !errors.Is(wrapped, sentinel) {
			t.Errorf("errors.Is(wrapped, %v) = false", sentinel)
		}
		// Double-wrap also classifies.
		twice := fmt.Errorf("provider call: %w", wrapped)
		if !errors.Is(twice, sentinel) {
			t.Errorf("errors.Is(twice-wrapped, %v) = false", sentinel)
		}
	}
}

func TestStubProviderClassifies(t *testing.T) {
	t.Parallel()
	p := stubProvider{}
	if _, err := p.Login(context.Background(), nil); !errors.Is(err, providers.ErrAuthExpired) {
		t.Errorf("Login err not classifiable: %v", err)
	}
	if _, err := p.Find(context.Background(), providers.FindRequest{}); !errors.Is(err, providers.ErrInventoryEmpty) {
		t.Errorf("Find err not classifiable: %v", err)
	}
	if _, err := p.Book(context.Background(), providers.Slot{}, nil); !errors.Is(err, providers.ErrSlotTaken) {
		t.Errorf("Book err not classifiable: %v", err)
	}
}

type fakeSession struct {
	provider domain.ProviderID
	user     domain.UserID
	exp      time.Time
}

func (f fakeSession) Provider() domain.ProviderID { return f.provider }
func (f fakeSession) User() domain.UserID         { return f.user }
func (f fakeSession) ExpiresAt() time.Time        { return f.exp }

func TestIsSessionExpired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		exp     time.Time
		now     time.Time
		expired bool
	}{
		{"future", now.Add(time.Hour), now, false},
		{"past", now.Add(-time.Hour), now, true},
		{"exactly now", now, now, true},
		{"zero exp = never expires", time.Time{}, now, false},
	}
	for _, c := range cases {
		s := fakeSession{provider: "resy", exp: c.exp}
		if got := providers.IsSessionExpired(s, c.now); got != c.expired {
			t.Errorf("%s: got %v want %v", c.name, got, c.expired)
		}
	}
}
