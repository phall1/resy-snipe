package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"resy-snipe/internal/domain"
)

func validGoal() domain.Goal {
	return domain.Goal{
		VenueQuery: domain.VenueQueryURL{URL: "https://resy.com/cities/ny/astoria-dc"},
		Date:       domain.NewDate(2026, time.June, 1),
		Party:      2,
		TimePrefs: domain.TimeWindow{
			Start:    domain.NewWallTime(18, 30, 0),
			End:      domain.NewWallTime(21, 0, 0),
			Priority: domain.PriorityEarlier,
		},
		AccountID: "acct-1",
	}
}

func TestGoalValidate_OK(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 10, 12, 0, 0, 0, time.UTC)
	if err := validGoal().Validate(now); err != nil {
		t.Fatalf("expected valid goal, got %v", err)
	}
}

func TestGoalValidate_Cases(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 10, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		mutate  func(*domain.Goal)
		wantErr error
	}{
		{
			name: "past date",
			mutate: func(g *domain.Goal) {
				g.Date = domain.NewDate(2026, time.May, 9)
			},
			wantErr: domain.ErrGoalDateInPast,
		},
		{
			name:    "zero date",
			mutate:  func(g *domain.Goal) { g.Date = domain.Date{} },
			wantErr: domain.ErrGoalDateZero,
		},
		{
			name:    "party zero",
			mutate:  func(g *domain.Goal) { g.Party = 0 },
			wantErr: domain.ErrGoalPartyNonPositive,
		},
		{
			name:    "party negative",
			mutate:  func(g *domain.Goal) { g.Party = -3 },
			wantErr: domain.ErrGoalPartyNonPositive,
		},
		{
			name:    "empty time window",
			mutate:  func(g *domain.Goal) { g.TimePrefs = domain.TimeWindow{} },
			wantErr: domain.ErrGoalTimeWindowEmpty,
		},
		{
			name: "inverted time window",
			mutate: func(g *domain.Goal) {
				g.TimePrefs = domain.TimeWindow{
					Start:    domain.NewWallTime(21, 0, 0),
					End:      domain.NewWallTime(18, 0, 0),
					Priority: domain.PriorityNone,
				}
			},
			wantErr: domain.ErrGoalTimeWindowInverted,
		},
		{
			name:    "nil venue query",
			mutate:  func(g *domain.Goal) { g.VenueQuery = nil },
			wantErr: domain.ErrGoalVenueQueryMissing,
		},
		{
			name: "empty url",
			mutate: func(g *domain.Goal) {
				g.VenueQuery = domain.VenueQueryURL{URL: ""}
			},
			wantErr: domain.ErrGoalVenueQueryMissing,
		},
		{
			name: "non-resy host",
			mutate: func(g *domain.Goal) {
				g.VenueQuery = domain.VenueQueryURL{URL: "https://opentable.com/r/astoria"}
			},
			wantErr: domain.ErrGoalVenueQueryHost,
		},
		{
			name: "empty slug",
			mutate: func(g *domain.Goal) {
				g.VenueQuery = domain.VenueQuerySlug{Slug: "", City: "ny"}
			},
			wantErr: domain.ErrGoalVenueQueryMissing,
		},
		{
			name: "empty name",
			mutate: func(g *domain.Goal) {
				g.VenueQuery = domain.VenueQueryName{Name: "  "}
			},
			wantErr: domain.ErrGoalVenueQueryMissing,
		},
		{
			name:    "missing account",
			mutate:  func(g *domain.Goal) { g.AccountID = "" },
			wantErr: domain.ErrGoalAccountMissing,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := validGoal()
			tc.mutate(&g)
			err := g.Validate(now)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestGoalValidate_AcceptsResyHosts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 10, 12, 0, 0, 0, time.UTC)
	for _, u := range []string{
		"https://resy.com/cities/ny/astoria-dc",
		"https://www.resy.com/cities/ny/astoria-dc",
		"HTTPS://Resy.com/Cities/NY/astoria-dc",
	} {
		g := validGoal()
		g.VenueQuery = domain.VenueQueryURL{URL: u}
		if err := g.Validate(now); err != nil {
			t.Errorf("expected %q to validate, got %v", u, err)
		}
	}
}

func TestGoalValidate_SlugAndNameVariants(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 10, 12, 0, 0, 0, time.UTC)

	g := validGoal()
	g.VenueQuery = domain.VenueQuerySlug{Slug: "astoria-dc", City: "ny"}
	if err := g.Validate(now); err != nil {
		t.Errorf("slug query: %v", err)
	}

	g.VenueQuery = domain.VenueQueryName{Name: "Astoria DC"}
	if err := g.Validate(now); err != nil {
		t.Errorf("name query: %v", err)
	}
}

func TestGoalString_ContainsKeyFields(t *testing.T) {
	t.Parallel()
	g := validGoal()
	g.Constraints = domain.Constraints{TableTypes: []string{"Bar", "Dining Room"}}
	s := g.String()
	for _, want := range []string{
		"Goal{",
		"venue:",
		"date:    2026-06-01",
		"party:   2",
		"window:",
		"account: acct-1",
		"tables:  Bar,Dining Room",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("String() missing %q\n%s", want, s)
		}
	}
}

func TestTimePriority_String(t *testing.T) {
	t.Parallel()
	cases := map[domain.TimePriority]string{
		domain.PriorityNone:    "none",
		domain.PriorityEarlier: "earlier",
		domain.PriorityLater:   "later",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("TimePriority(%d).String() = %q want %q", p, got, want)
		}
	}
}

// Compile-time check: the three concrete venue-query variants satisfy
// VenueQuery. A new variant must be added here so the union stays
// closed in code review.
var (
	_ domain.VenueQuery = domain.VenueQueryURL{}
	_ domain.VenueQuery = domain.VenueQuerySlug{}
	_ domain.VenueQuery = domain.VenueQueryName{}
)
