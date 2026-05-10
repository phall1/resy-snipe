package resy_test

import (
	"context"
	"encoding/json"
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

// searchFixture loads the canned /3/venuesearch/search response
// captured from the live API. The file shape is the authoritative
// reference for SearchResponse decoding.
func searchFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "search_astoria.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func newSearchClient(t *testing.T, srv *httptest.Server) *resy.Client {
	t.Helper()
	return resy.NewClient(slog.New(slog.DiscardHandler), clock.NewReal(),
		resy.WithBaseURL(srv.URL),
		resy.WithAPIKey("test"),
		resy.WithHTTPClient(srv.Client()),
		resy.WithUserAgent("test"),
	)
}

func TestSearchVenuesByNameHappyPath(t *testing.T) {
	t.Parallel()
	body := searchFixture(t)

	var captured *http.Request
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := newSearchClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	vs, err := c.SearchVenuesByName(ctx, "astoria")
	if err != nil {
		t.Fatalf("SearchVenuesByName: %v", err)
	}

	// HTTP shape.
	if got, want := captured.Method, http.MethodPost; got != want {
		t.Errorf("method = %q, want %q", got, want)
	}
	if got := captured.URL.Path; got != "/3/venuesearch/search" {
		t.Errorf("path = %q, want /3/venuesearch/search", got)
	}
	if got := captured.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := captured.Header.Get("Authorization"); got == "" {
		t.Errorf("missing Authorization header")
	}

	// Request body should carry the query verbatim, plus the venue
	// type filter and a per-page cap.
	var sent map[string]any
	if err := json.Unmarshal(capturedBody, &sent); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if sent["query"] != "astoria" {
		t.Errorf("body.query = %v, want astoria", sent["query"])
	}
	if _, ok := sent["per_page"]; !ok {
		t.Errorf("body missing per_page: %v", sent)
	}
	if got, ok := sent["types"].([]any); !ok || len(got) == 0 || got[0] != "venue" {
		t.Errorf("body.types = %v, want [\"venue\"]", sent["types"])
	}

	// Response projection: at least one venue, fields populated,
	// relevance order preserved against the fixture.
	if len(vs) == 0 {
		t.Fatalf("expected hits, got 0")
	}
	if vs[0].Provider != "resy" {
		t.Errorf("Provider = %q", vs[0].Provider)
	}
	if vs[0].Ref != "67463" {
		t.Errorf("first Ref = %q, want 67463 (Mayahuel)", vs[0].Ref)
	}
	if vs[0].Name != "Mayahuel" {
		t.Errorf("first Name = %q, want Mayahuel", vs[0].Name)
	}
	// TZ is intentionally nil for search hits.
	if vs[0].TZ != nil {
		t.Errorf("TZ = %v, want nil (search responses do not carry tz)", vs[0].TZ)
	}
}

func TestSearchVenuesByNamePreservesOrder(t *testing.T) {
	t.Parallel()
	body := searchFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	c := newSearchClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	vs, err := c.SearchVenuesByName(ctx, "astoria")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	wantOrder := []string{"67463", "50923", "65245"}
	if len(vs) < len(wantOrder) {
		t.Fatalf("len(vs) = %d, want >= %d", len(vs), len(wantOrder))
	}
	for i, ref := range wantOrder {
		if vs[i].Ref != ref {
			t.Errorf("vs[%d].Ref = %q, want %q (relevance order from fixture)", i, vs[i].Ref, ref)
		}
	}
}

func TestSearchVenuesByNameRejectsEmptyQuery(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be hit on empty query")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newSearchClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	for _, in := range []string{"", "   ", "\t\n"} {
		if _, err := c.SearchVenuesByName(ctx, in); err == nil {
			t.Errorf("query %q: expected input-validation error, got nil", in)
		}
	}
}

func TestSearchVenuesByNameEmptyHits(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"search":{"hits":[]}}`)
	}))
	defer srv.Close()
	c := newSearchClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	vs, err := c.SearchVenuesByName(ctx, "no-such-place-anywhere")
	if err != nil {
		t.Fatalf("empty hits should not error, got %v", err)
	}
	if len(vs) != 0 {
		t.Errorf("len(vs) = %d, want 0", len(vs))
	}
}

func TestSearchVenuesByNameMalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{not json`)
	}))
	defer srv.Close()
	c := newSearchClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.SearchVenuesByName(ctx, "astoria")
	if !errors.Is(err, providers.ErrParseFailure) {
		t.Fatalf("err = %v, want ErrParseFailure", err)
	}
}

func TestSearchVenuesByNameClassifierPassthrough(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, `{"message":"signature expired"}`, providers.ErrAuthExpired},
		{"rate limited", http.StatusTooManyRequests, `{"message":"rate limit"}`, providers.ErrRateLimited},
		{"not found", http.StatusNotFound, `{"message":"unknown"}`, providers.ErrVenueNotFound},
		{"anti-bot", http.StatusForbidden, `{"message":"px-captcha required"}`, providers.ErrAntiBotChallenge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := newSearchClient(t, srv)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := c.SearchVenuesByName(ctx, "astoria")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSearchVenuesByNameTimeout(t *testing.T) {
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

	c := newSearchClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.SearchVenuesByName(ctx, "astoria")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestSearchVenuesByNameSkipsMalformedHits(t *testing.T) {
	t.Parallel()
	// One hit with no id, one with no slug, one valid. The defensive
	// skip means the valid hit still surfaces.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"search":{"hits":[
			{"id":{},"name":"NoID","url_slug":"x"},
			{"id":{"resy":1},"name":"NoSlug","url_slug":""},
			{"id":{"resy":42},"name":"Good","url_slug":"good","location":{"code":"ny"}}
		]}}`)
	}))
	defer srv.Close()
	c := newSearchClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	vs, err := c.SearchVenuesByName(ctx, "astoria")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("len(vs) = %d, want 1 (other hits malformed)", len(vs))
	}
	if vs[0].Ref != "42" || vs[0].Name != "Good" {
		t.Errorf("survivor = %+v, want Good/42", vs[0])
	}
}
