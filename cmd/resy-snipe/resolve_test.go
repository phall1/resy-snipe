package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resolver"
)

// ---- test doubles ----------------------------------------------------------

// resolveFakeProvider satisfies providers.Provider for the resolve
// subcommand tests. Only ResolveVenue and SearchVenuesByName are
// exercised by the resolver; the other methods return canned "unused"
// errors so a regression that routes the resolver through them surfaces
// loudly. Counters let the cache-bypass test assert that -no-cache
// actually triggered a provider call.
type resolveFakeProvider struct {
	mu sync.Mutex

	resolveVenue map[string]domain.Venue // key: slug|city
	resolveErr   error
	resolveCalls atomic.Int32

	searchHits  map[string][]domain.Venue // key: lowercase trimmed query
	searchErr   error
	searchCalls atomic.Int32
}

func newResolveFakeProvider() *resolveFakeProvider {
	return &resolveFakeProvider{
		resolveVenue: map[string]domain.Venue{},
		searchHits:   map[string][]domain.Venue{},
	}
}

func (*resolveFakeProvider) ID() domain.ProviderID { return "resy" }

func (*resolveFakeProvider) Login(context.Context, providers.Credentials) (providers.Session, error) {
	return nil, errors.New("resolveFakeProvider: Login unused")
}

func (*resolveFakeProvider) Ping(context.Context, providers.Session) error {
	return errors.New("resolveFakeProvider: Ping unused")
}

func (*resolveFakeProvider) SearchVenues(context.Context, providers.Query) ([]domain.Venue, error) {
	return nil, errors.New("resolveFakeProvider: SearchVenues unused")
}

func (f *resolveFakeProvider) ResolveVenue(_ context.Context, slug, city string) (domain.Venue, error) {
	f.resolveCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolveErr != nil {
		return domain.Venue{}, f.resolveErr
	}
	v, ok := f.resolveVenue[slug+"|"+city]
	if !ok {
		return domain.Venue{}, providers.ErrVenueNotFound
	}
	return v, nil
}

func (f *resolveFakeProvider) SearchVenuesByName(_ context.Context, q string) ([]domain.Venue, error) {
	f.searchCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.searchHits[strings.ToLower(strings.TrimSpace(q))], nil
}

func (*resolveFakeProvider) Calendar(context.Context, domain.VenueRef, providers.DateRange) (providers.Calendar, error) {
	return providers.Calendar{}, errors.New("resolveFakeProvider: Calendar unused")
}

func (*resolveFakeProvider) Find(context.Context, providers.FindRequest) ([]providers.Slot, error) {
	return nil, errors.New("resolveFakeProvider: Find unused")
}

func (*resolveFakeProvider) Book(context.Context, providers.Slot, providers.Session) (providers.Confirmation, error) {
	return providers.Confirmation{}, errors.New("resolveFakeProvider: Book unused")
}

// memResolveCache is an in-memory resolver.CacheStore. The resolver
// tests in internal/resolver use the same shape; we duplicate it here
// rather than depend on the test package to avoid an import cycle.
type memResolveCache struct {
	mu     sync.Mutex
	venues map[string]resolver.CachedVenue      // slug|city
	names  map[string]resolver.CachedNameSearch // normalized query
}

func newMemResolveCache() *memResolveCache {
	return &memResolveCache{
		venues: map[string]resolver.CachedVenue{},
		names:  map[string]resolver.CachedNameSearch{},
	}
}

func (m *memResolveCache) UpsertVenueCache(_ context.Context, slug, city string, v domain.Venue, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.venues[slug+"|"+city] = resolver.CachedVenue{Venue: v, CachedAt: t}
	return nil
}

func (m *memResolveCache) GetVenueCache(_ context.Context, slug, city string) (resolver.CachedVenue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.venues[slug+"|"+city]
	if !ok {
		return resolver.CachedVenue{}, resolver.ErrCacheMiss
	}
	return v, nil
}

func (m *memResolveCache) UpsertNameSearchCache(_ context.Context, q string, hits []domain.Venue, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]domain.Venue, len(hits))
	copy(cp, hits)
	m.names[q] = resolver.CachedNameSearch{Hits: cp, CachedAt: t}
	return nil
}

func (m *memResolveCache) GetNameSearchCache(_ context.Context, q string) (resolver.CachedNameSearch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.names[q]
	if !ok {
		return resolver.CachedNameSearch{}, resolver.ErrCacheMiss
	}
	return v, nil
}

// ---- helpers ---------------------------------------------------------------

// mustNYTZ loads the America/New_York zone or fatals — every Venue
// produced by these tests needs a real TZ so domain.Venue.Validate
// stays satisfied if the path ever calls it.
func mustNYTZ(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

// ---- tests -----------------------------------------------------------------

func TestRunResolveCmd_URL_HappyPath(t *testing.T) { //nolint:paralleltest // mutates the package-level openResolveBackendFn seam — parallel runs would race.
	prov := newResolveFakeProvider()
	cache := newMemResolveCache()
	want := domain.Venue{Provider: "resy", Ref: "49716", Name: "Astoria DC", TZ: mustNYTZ(t)}
	prov.resolveVenue["astoria-dc|washington-dc"] = want

	swapBackend(t, prov, cache)

	out := &bytes.Buffer{}
	err := runResolveCmd(
		context.Background(),
		[]string{"https://resy.com/cities/washington-dc/venues/astoria-dc?utm=x"},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err != nil {
		t.Fatalf("runResolveCmd: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "Astoria DC") || !strings.Contains(body, "49716") {
		t.Errorf("output missing expected fields: %q", body)
	}
}

func TestRunResolveCmd_SlugCity(t *testing.T) { //nolint:paralleltest // shared openResolveBackendFn global; see URL_HappyPath note.
	prov := newResolveFakeProvider()
	cache := newMemResolveCache()
	want := domain.Venue{Provider: "resy", Ref: "49716", Name: "Astoria DC", TZ: mustNYTZ(t)}
	prov.resolveVenue["astoria-dc|washington-dc"] = want

	swapBackend(t, prov, cache)

	out := &bytes.Buffer{}
	err := runResolveCmd(
		context.Background(),
		[]string{"astoria-dc+washington-dc"},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err != nil {
		t.Fatalf("runResolveCmd: %v", err)
	}
	if !strings.Contains(out.String(), "Astoria DC") {
		t.Errorf("output missing venue name: %q", out.String())
	}
}

func TestRunResolveCmd_FreeformName_SingleHit(t *testing.T) { //nolint:paralleltest // shared openResolveBackendFn global.
	prov := newResolveFakeProvider()
	cache := newMemResolveCache()
	want := domain.Venue{Provider: "resy", Ref: "49716", Name: "Astoria DC", TZ: mustNYTZ(t)}
	prov.searchHits["astoria dc"] = []domain.Venue{want}

	swapBackend(t, prov, cache)

	out := &bytes.Buffer{}
	err := runResolveCmd(
		context.Background(),
		[]string{"Astoria DC"},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err != nil {
		t.Fatalf("runResolveCmd: %v", err)
	}
	if !strings.Contains(out.String(), "Astoria DC") {
		t.Errorf("output missing venue name: %q", out.String())
	}
}

func TestRunResolveCmd_FreeformName_Ambiguous(t *testing.T) { //nolint:paralleltest // shared openResolveBackendFn global.
	prov := newResolveFakeProvider()
	cache := newMemResolveCache()
	a := domain.Venue{Provider: "resy", Ref: "49716", Name: "Astoria DC", TZ: mustNYTZ(t)}
	b := domain.Venue{Provider: "resy", Ref: "12345", Name: "Astoria NYC", TZ: mustNYTZ(t)}
	prov.searchHits["astoria"] = []domain.Venue{a, b}

	swapBackend(t, prov, cache)

	out := &bytes.Buffer{}
	err := runResolveCmd(
		context.Background(),
		[]string{"Astoria"},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err == nil {
		t.Fatal("expected non-nil error on ambiguous resolve")
	}
	body := out.String()
	if !strings.Contains(body, "Multiple matches") {
		t.Errorf("output missing 'Multiple matches' header: %q", body)
	}
	if !strings.Contains(body, "Astoria DC") || !strings.Contains(body, "Astoria NYC") {
		t.Errorf("output missing candidates: %q", body)
	}
}

func TestRunResolveCmd_FreeformName_NotFound(t *testing.T) { //nolint:paralleltest // shared openResolveBackendFn global.
	prov := newResolveFakeProvider()
	cache := newMemResolveCache()
	// No seeded hits.

	swapBackend(t, prov, cache)

	out := &bytes.Buffer{}
	err := runResolveCmd(
		context.Background(),
		[]string{"nonexistent venue xyz"},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err == nil {
		t.Fatal("expected non-nil error on zero hits")
	}
	if !errors.Is(err, resolver.ErrVenueNotFound) {
		t.Fatalf("err = %v, want ErrVenueNotFound", err)
	}
	if !strings.Contains(out.String(), "not found") {
		t.Errorf("output missing 'not found' message: %q", out.String())
	}
}

func TestRunResolveCmd_FormatJSON(t *testing.T) { //nolint:paralleltest // shared openResolveBackendFn global.
	prov := newResolveFakeProvider()
	cache := newMemResolveCache()
	want := domain.Venue{Provider: "resy", Ref: "49716", Name: "Astoria DC", TZ: mustNYTZ(t)}
	prov.resolveVenue["astoria-dc|washington-dc"] = want

	swapBackend(t, prov, cache)

	out := &bytes.Buffer{}
	err := runResolveCmd(
		context.Background(),
		[]string{"-format=json", "astoria-dc+washington-dc"},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err != nil {
		t.Fatalf("runResolveCmd: %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v\nbody=%s", err, out.String())
	}
	if decoded["ref"] != "49716" || decoded["name"] != "Astoria DC" {
		t.Errorf("decoded = %+v, want ref=49716 name='Astoria DC'", decoded)
	}
	if decoded["tz"] != "America/New_York" {
		t.Errorf("decoded tz = %q, want America/New_York", decoded["tz"])
	}
}

func TestRunResolveCmd_NoCacheForcesProviderCall(t *testing.T) { //nolint:paralleltest // shared openResolveBackendFn global.
	prov := newResolveFakeProvider()
	cache := newMemResolveCache()
	want := domain.Venue{Provider: "resy", Ref: "49716", Name: "Astoria DC", TZ: mustNYTZ(t)}
	prov.resolveVenue["astoria-dc|washington-dc"] = want

	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	// Seed a FRESH cache row that would otherwise satisfy the resolve
	// without a provider call.
	_ = cache.UpsertVenueCache(context.Background(), "astoria-dc", "washington-dc",
		want, clk.Now().Add(-time.Minute))

	swapBackend(t, prov, cache)

	out := &bytes.Buffer{}
	err := runResolveCmd(
		context.Background(),
		[]string{"-no-cache", "astoria-dc+washington-dc"},
		strings.NewReader(""),
		out,
		clk,
	)
	if err != nil {
		t.Fatalf("runResolveCmd: %v", err)
	}
	if got := prov.resolveCalls.Load(); got != 1 {
		t.Errorf("provider ResolveVenue calls = %d, want 1 (cache bypass failed)", got)
	}
}

func TestRunResolveCmd_MissingQueryReturnsError(t *testing.T) { //nolint:paralleltest // sibling tests in this file mutate a shared global; serialize to be safe.
	out := &bytes.Buffer{}
	err := runResolveCmd(
		context.Background(),
		[]string{},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err == nil {
		t.Fatal("expected error on missing query")
	}
}

func TestRunResolveCmd_InvalidFormatReturnsError(t *testing.T) { //nolint:paralleltest // sibling tests in this file mutate a shared global; serialize to be safe.
	out := &bytes.Buffer{}
	err := runResolveCmd(
		context.Background(),
		[]string{"-format=xml", "astoria-dc+washington-dc"},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err == nil {
		t.Fatal("expected error on invalid -format")
	}
}

func TestBuildVenueQuery_Variants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want domain.VenueQuery
	}{
		{
			name: "url",
			in:   "https://resy.com/cities/washington-dc/venues/astoria-dc",
			want: domain.VenueQueryURL{URL: "https://resy.com/cities/washington-dc/venues/astoria-dc"},
		},
		{
			name: "slug plus city",
			in:   "astoria-dc+washington-dc",
			want: domain.VenueQuerySlug{Slug: "astoria-dc", City: "washington-dc"},
		},
		{
			name: "slug colon city",
			in:   "astoria-dc:washington-dc",
			want: domain.VenueQuerySlug{Slug: "astoria-dc", City: "washington-dc"},
		},
		{
			name: "freeform name",
			in:   "Astoria DC",
			want: domain.VenueQueryName{Name: "Astoria DC"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := buildVenueQuery(tc.in)
			if err != nil {
				t.Fatalf("buildVenueQuery: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// ---- helpers (test-only) ---------------------------------------------------

// swapBackend installs the fake backend for the duration of the test.
// It's a small wrapper so each test can stay one line of setup.
func swapBackend(t *testing.T, prov providers.Provider, cache resolver.CacheStore) {
	t.Helper()
	prev := openResolveBackendFn
	openResolveBackendFn = func(_ context.Context, _ *slog.Logger, _ clock.Clock) (resolverBackend, error) {
		return resolverBackend{
			provider: prov,
			cache:    cache,
			cleanup:  func() error { return nil },
		}, nil
	}
	t.Cleanup(func() { openResolveBackendFn = prev })
}
