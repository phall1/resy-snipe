package resy_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resy"
)

// venueFixture loads the canned /3/venue response captured from the
// live API. We keep it under testdata/ so the file shape is the
// authoritative reference for VenueResponse decoding.
func venueFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "venue_astoria.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func newResolveClient(t *testing.T, srv *httptest.Server) *resy.Client {
	t.Helper()
	return resy.NewClient(slog.New(slog.DiscardHandler), clock.NewReal(),
		resy.WithBaseURL(srv.URL),
		resy.WithAPIKey("test"),
		resy.WithHTTPClient(srv.Client()),
		resy.WithUserAgent("test"),
	)
}

func TestResolveVenueHappyPath(t *testing.T) {
	t.Parallel()
	body := venueFixture(t)

	var captured *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := newResolveClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	v, err := c.ResolveVenue(ctx, "astoria-dc", "washington-dc")
	if err != nil {
		t.Fatalf("ResolveVenue: %v", err)
	}

	// Path / query round-trip.
	if got := captured.URL.Path; got != "/3/venue" {
		t.Errorf("path = %q, want /3/venue", got)
	}
	q := captured.URL.Query()
	if q.Get("url_slug") != "astoria-dc" {
		t.Errorf("url_slug = %q", q.Get("url_slug"))
	}
	if q.Get("location") != "washington-dc" {
		t.Errorf("location = %q", q.Get("location"))
	}
	if got := captured.Header.Get("Authorization"); got == "" {
		t.Errorf("missing Authorization header")
	}

	// Field projection.
	if v.Provider != "resy" {
		t.Errorf("Provider = %q", v.Provider)
	}
	if v.Ref != "49716" {
		t.Errorf("Ref = %q, want 49716", v.Ref)
	}
	if v.Name != "Astoria DC" {
		t.Errorf("Name = %q", v.Name)
	}
	if v.TZ == nil {
		t.Fatal("TZ is nil")
	}
	if v.TZ.String() != "EST5EDT" {
		t.Errorf("TZ.String() = %q, want EST5EDT", v.TZ.String())
	}
}

func TestResolveVenueDeterministic(t *testing.T) {
	t.Parallel()
	body := venueFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := newResolveClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	a, err := c.ResolveVenue(ctx, "astoria-dc", "washington-dc")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	b, err := c.ResolveVenue(ctx, "astoria-dc", "washington-dc")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if a.Provider != b.Provider || a.Ref != b.Ref || a.Name != b.Name {
		t.Errorf("non-deterministic: a=%+v b=%+v", a, b)
	}
	if a.TZ.String() != b.TZ.String() {
		t.Errorf("non-deterministic TZ: a=%v b=%v", a.TZ, b.TZ)
	}
}

func TestResolveVenue404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"venue not found"}`)
	}))
	defer srv.Close()
	c := newResolveClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.ResolveVenue(ctx, "no-such-place", "ny")
	if !errors.Is(err, providers.ErrVenueNotFound) {
		t.Fatalf("err = %v, want ErrVenueNotFound", err)
	}
}

func TestResolveVenueEmptyBodyMaps404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 200 + empty body — Resy occasionally returns this for misses.
	}))
	defer srv.Close()
	c := newResolveClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.ResolveVenue(ctx, "x", "y")
	if !errors.Is(err, providers.ErrVenueNotFound) {
		t.Fatalf("empty body err = %v, want ErrVenueNotFound", err)
	}
}

func TestResolveVenueNullBodyMaps404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "null")
	}))
	defer srv.Close()
	c := newResolveClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.ResolveVenue(ctx, "x", "y")
	if !errors.Is(err, providers.ErrVenueNotFound) {
		t.Fatalf("null body err = %v, want ErrVenueNotFound", err)
	}
}

func TestResolveVenueMalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{not-json`)
	}))
	defer srv.Close()
	c := newResolveClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.ResolveVenue(ctx, "x", "y")
	if !errors.Is(err, providers.ErrParseFailure) {
		t.Fatalf("malformed err = %v, want ErrParseFailure", err)
	}
}

func TestResolveVenueMissingTZ(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":{"resy":1},"name":"X","url_slug":"x","locale":{"currency":"USD"}}`)
	}))
	defer srv.Close()
	c := newResolveClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.ResolveVenue(ctx, "x", "y")
	if !errors.Is(err, providers.ErrParseFailure) {
		t.Fatalf("missing tz err = %v, want ErrParseFailure", err)
	}
}

func TestResolveVenueMissingResyID(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":{},"name":"X","url_slug":"x","locale":{"time_zone":"UTC"}}`)
	}))
	defer srv.Close()
	c := newResolveClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.ResolveVenue(ctx, "x", "y")
	if !errors.Is(err, providers.ErrParseFailure) {
		t.Fatalf("missing id.resy err = %v, want ErrParseFailure", err)
	}
}

func TestResolveVenueRejectsBadInputs(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	c := newResolveClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)

	if _, err := c.ResolveVenue(ctx, "", "ny"); err == nil {
		t.Error("empty slug: expected error")
	}
	if _, err := c.ResolveVenue(ctx, "slug", ""); err == nil {
		t.Error("empty city: expected error")
	}
	if _, err := c.ResolveVenue(ctx, "  ", "ny"); err == nil {
		t.Error("whitespace slug: expected error")
	}
}

func TestResolveVenueClassifierPassthrough(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, `{"message":"signature expired"}`, providers.ErrAuthExpired},
		{"rate limited", http.StatusTooManyRequests, `{"message":"rate limit"}`, providers.ErrRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := newResolveClient(t, srv)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := c.ResolveVenue(ctx, "x", "y")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestResolveVenueTimeout(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-gate:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(gate)

	c := newResolveClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.ResolveVenue(ctx, "x", "y")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}
