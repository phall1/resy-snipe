package domain_test

import (
	"testing"
	"time"

	"resy-snipe/internal/domain"
)

func TestIntentHashStable(t *testing.T) {
	t.Parallel()
	mk := func() domain.Intent {
		return domain.Intent{
			User:      "u1",
			Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
			Date:      domain.NewDate(2026, time.June, 1),
			PartySize: 2,
			SlotPrefs: []domain.SlotPreference{
				{Time: domain.NewWallTime(19, 30, 0), TableType: ""},
				{Time: domain.NewWallTime(20, 0, 0), TableType: "Bar"},
			},
		}
	}
	h1 := mk().Hash()
	h2 := mk().Hash()
	if h1 != h2 {
		t.Fatalf("hash unstable: %s vs %s", h1, h2)
	}
	if h1 == "" {
		t.Fatal("empty hash")
	}
}

func TestIntentHashDiffersOnFieldChange(t *testing.T) {
	t.Parallel()
	base := domain.Intent{
		User:      "u1",
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
		SlotPrefs: []domain.SlotPreference{
			{Time: domain.NewWallTime(19, 30, 0)},
		},
	}

	cases := []struct {
		name string
		mut  func(domain.Intent) domain.Intent
	}{
		{"user", func(i domain.Intent) domain.Intent { i.User = "u2"; return i }},
		{"venue ref", func(i domain.Intent) domain.Intent {
			i.Venue.Ref = "99999"
			return i
		}},
		{"date", func(i domain.Intent) domain.Intent {
			i.Date = domain.NewDate(2026, time.June, 2)
			return i
		}},
		{"party", func(i domain.Intent) domain.Intent {
			i.PartySize = 4
			return i
		}},
		{"slot time", func(i domain.Intent) domain.Intent {
			i.SlotPrefs = []domain.SlotPreference{{Time: domain.NewWallTime(19, 45, 0)}}
			return i
		}},
		{"slot table type", func(i domain.Intent) domain.Intent {
			i.SlotPrefs = []domain.SlotPreference{{Time: domain.NewWallTime(19, 30, 0), TableType: "Bar"}}
			return i
		}},
	}

	original := base.Hash()
	for _, c := range cases {
		mutated := c.mut(base).Hash()
		if mutated == original {
			t.Errorf("hash did not change after mutating %s", c.name)
		}
	}
}

func TestVenueValidate(t *testing.T) {
	t.Parallel()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	good := domain.Venue{Provider: "resy", Ref: "38660", Name: "Carbone", TZ: ny}
	if err := good.Validate(); err != nil {
		t.Fatalf("good venue: %v", err)
	}
	bad := good
	bad.TZ = nil
	if err := bad.Validate(); err == nil {
		t.Fatal("missing TZ must error")
	}
}

func TestDateRoundTrip(t *testing.T) {
	t.Parallel()
	d, err := domain.ParseDate("2026-06-01")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.String() != "2026-06-01" {
		t.Fatalf("got %s want 2026-06-01", d)
	}
	if d.IsZero() {
		t.Fatal("non-zero date reported zero")
	}
	if (domain.Date{}).IsZero() == false {
		t.Fatal("zero date reported non-zero")
	}
}

func TestWallTimeParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want domain.WallTime
	}{
		{"19:30", domain.NewWallTime(19, 30, 0)},
		{"19:30:45", domain.NewWallTime(19, 30, 45)},
	}
	for _, c := range cases {
		got, err := domain.ParseWallTime(c.in)
		if err != nil {
			t.Errorf("ParseWallTime(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseWallTime(%q) = %v want %v", c.in, got, c.want)
		}
	}
	if _, err := domain.ParseWallTime("garbage"); err == nil {
		t.Fatal("garbage should fail to parse")
	}
}

func TestVenueRefString(t *testing.T) {
	t.Parallel()
	r := domain.VenueRef{Provider: "resy", Ref: "38660"}
	if r.String() != "resy:38660" {
		t.Fatalf("got %s", r)
	}
}
