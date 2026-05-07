package resy_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resy"
)

func newCalendarClient(t *testing.T, srv *httptest.Server) *resy.Client {
	t.Helper()
	return resy.NewClient(slog.New(slog.DiscardHandler), clock.NewReal(),
		resy.WithBaseURL(srv.URL),
		resy.WithAPIKey("test"),
		resy.WithHTTPClient(srv.Client()),
		resy.WithUserAgent("test"),
	)
}

func TestCalendarParsesScheduledArray(t *testing.T) {
	t.Parallel()
	var captured *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		_, _ = io.WriteString(w, `{
			"scheduled": [
				{"date":"2026-06-01","inventory":{"reservation":"available"}},
				{"date":"2026-06-02","inventory":{"reservation":"sold-out"}},
				{"date":"2026-06-03","inventory":{"reservation":"available"}}
			]
		}`)
	}))
	defer srv.Close()

	c := newCalendarClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	cal, err := c.Calendar(ctx,
		domain.VenueRef{Provider: "resy", Ref: "38660"},
		providers.DateRange{
			Start:     domain.NewDate(2026, time.June, 1),
			End:       domain.NewDate(2026, time.June, 3),
			PartySize: 2,
		},
	)
	if err != nil {
		t.Fatalf("Calendar: %v", err)
	}

	if cal.Venue.Ref != "38660" {
		t.Errorf("venue ref: %s", cal.Venue.Ref)
	}
	if len(cal.Days) != 3 {
		t.Fatalf("expected 3 days, got %d", len(cal.Days))
	}
	want := []struct {
		date      domain.Date
		available bool
	}{
		{domain.NewDate(2026, time.June, 1), true},
		{domain.NewDate(2026, time.June, 2), false},
		{domain.NewDate(2026, time.June, 3), true},
	}
	for i, w := range want {
		if cal.Days[i].Date != w.date || cal.Days[i].Available != w.available {
			t.Errorf("day[%d]: got %+v want date=%v available=%v",
				i, cal.Days[i], w.date, w.available)
		}
	}

	// Far-edge invariant: last entry preserved.
	if cal.Days[len(cal.Days)-1].Date != domain.NewDate(2026, time.June, 3) {
		t.Errorf("far-edge wrong: %v", cal.Days[len(cal.Days)-1].Date)
	}

	// Query parameters round-trip.
	q := captured.URL.Query()
	if q.Get("venue_id") != "38660" {
		t.Errorf("venue_id: %s", q.Get("venue_id"))
	}
	if q.Get("num_seats") != "2" {
		t.Errorf("num_seats: %s", q.Get("num_seats"))
	}
	if q.Get("start_date") != "2026-06-01" {
		t.Errorf("start_date: %s", q.Get("start_date"))
	}
	if q.Get("end_date") != "2026-06-03" {
		t.Errorf("end_date: %s", q.Get("end_date"))
	}
}

func TestCalendarDefaultsPartySizeWhenUnspecified(t *testing.T) {
	t.Parallel()
	var seenSeats string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenSeats = r.URL.Query().Get("num_seats")
		_, _ = io.WriteString(w, `{"scheduled":[]}`)
	}))
	defer srv.Close()

	c := newCalendarClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.Calendar(ctx,
		domain.VenueRef{Provider: "resy", Ref: "38660"},
		providers.DateRange{
			Start: domain.NewDate(2026, time.June, 1),
			End:   domain.NewDate(2026, time.June, 7),
			// PartySize: 0 — should default
		},
	)
	if err != nil {
		t.Fatalf("Calendar: %v", err)
	}
	if seenSeats != "2" {
		t.Errorf("default num_seats: got %q want 2", seenSeats)
	}
}

func TestCalendarRejectsBadInputs(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"scheduled":[]}`)
	}))
	t.Cleanup(srv.Close)
	c := newCalendarClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)

	cases := []struct {
		name string
		ref  domain.VenueRef
		r    providers.DateRange
	}{
		{
			"wrong provider",
			domain.VenueRef{Provider: "opentable", Ref: "x"},
			providers.DateRange{Start: domain.NewDate(2026, 6, 1), End: domain.NewDate(2026, 6, 7)},
		},
		{
			"empty ref",
			domain.VenueRef{Provider: "resy", Ref: ""},
			providers.DateRange{Start: domain.NewDate(2026, 6, 1), End: domain.NewDate(2026, 6, 7)},
		},
		{
			"missing start",
			domain.VenueRef{Provider: "resy", Ref: "1"},
			providers.DateRange{End: domain.NewDate(2026, 6, 7)},
		},
		{
			"missing end",
			domain.VenueRef{Provider: "resy", Ref: "1"},
			providers.DateRange{Start: domain.NewDate(2026, 6, 1)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := c.Calendar(ctx, tc.ref, tc.r); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestCalendarMapsClassifierErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"signature expired"}`)
	}))
	defer srv.Close()
	c := newCalendarClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.Calendar(ctx,
		domain.VenueRef{Provider: "resy", Ref: "1"},
		providers.DateRange{Start: domain.NewDate(2026, 6, 1), End: domain.NewDate(2026, 6, 7)},
	)
	if !errors.Is(err, providers.ErrAuthExpired) {
		t.Fatalf("err = %v, want ErrAuthExpired", err)
	}
}
