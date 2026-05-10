package resolver_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resolver"
)

// ---- test doubles ----------------------------------------------------------

// fakeProvider satisfies providers.Provider. Tests pre-populate the
// resolve/search maps and the error fields; calls increment counters
// for assertion.
type fakeProvider struct {
	mu sync.Mutex

	// ResolveVenue plumbing
	resolveVenue map[string]domain.Venue
	resolveErr   error
	resolveCalls int

	// SearchVenuesByName plumbing
	searchHits  map[string][]domain.Venue
	searchErr   error
	searchCalls int
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		resolveVenue: map[string]domain.Venue{},
		searchHits:   map[string][]domain.Venue{},
	}
}

func (f *fakeProvider) ID() domain.ProviderID { return "resy" }

func (f *fakeProvider) Login(_ context.Context, _ providers.Credentials) (providers.Session, error) {
	return nil, errors.New("unused")
}

func (f *fakeProvider) Ping(_ context.Context, _ providers.Session) error {
	return errors.New("unused")
}

func (f *fakeProvider) SearchVenues(_ context.Context, _ providers.Query) ([]domain.Venue, error) {
	return nil, errors.New("unused")
}

func (f *fakeProvider) ResolveVenue(_ context.Context, slug, city string) (domain.Venue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveCalls++
	if f.resolveErr != nil {
		return domain.Venue{}, f.resolveErr
	}
	v, ok := f.resolveVenue[slug+"|"+city]
	if !ok {
		return domain.Venue{}, providers.ErrVenueNotFound
	}
	return v, nil
}

func (f *fakeProvider) SearchVenuesByName(_ context.Context, query string) ([]domain.Venue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchCalls++
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.searchHits[strings.ToLower(strings.TrimSpace(query))], nil
}

func (f *fakeProvider) Calendar(_ context.Context, _ domain.VenueRef, _ providers.DateRange) (providers.Calendar, error) {
	return providers.Calendar{}, errors.New("unused")
}

func (f *fakeProvider) Find(_ context.Context, _ providers.FindRequest) ([]providers.Slot, error) {
	return nil, errors.New("unused")
}

func (f *fakeProvider) Book(_ context.Context, _ providers.Slot, _ providers.Session) (providers.Confirmation, error) {
	return providers.Confirmation{}, errors.New("unused")
}

func (f *fakeProvider) calls() (resolve, search int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolveCalls, f.searchCalls
}

// memCache is an in-memory CacheStore. Tests inject canned cache
// rows and inspect post-Resolve state.
type memCache struct {
	mu     sync.Mutex
	venues map[string]resolver.CachedVenue      // key: slug|city
	names  map[string]resolver.CachedNameSearch // key: normalized query
}

func newMemCache() *memCache {
	return &memCache{
		venues: map[string]resolver.CachedVenue{},
		names:  map[string]resolver.CachedNameSearch{},
	}
}

func (m *memCache) UpsertVenueCache(_ context.Context, slug, city string, venue domain.Venue, cachedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.venues[slug+"|"+city] = resolver.CachedVenue{Venue: venue, CachedAt: cachedAt}
	return nil
}

func (m *memCache) GetVenueCache(_ context.Context, slug, city string) (resolver.CachedVenue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.venues[slug+"|"+city]
	if !ok {
		return resolver.CachedVenue{}, resolver.ErrCacheMiss
	}
	return v, nil
}

func (m *memCache) UpsertNameSearchCache(_ context.Context, query string, hits []domain.Venue, cachedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// copy hits to defend against caller mutation
	cp := make([]domain.Venue, len(hits))
	copy(cp, hits)
	m.names[query] = resolver.CachedNameSearch{Hits: cp, CachedAt: cachedAt}
	return nil
}

func (m *memCache) GetNameSearchCache(_ context.Context, query string) (resolver.CachedNameSearch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.names[query]
	if !ok {
		return resolver.CachedNameSearch{}, resolver.ErrCacheMiss
	}
	return v, nil
}

// ---- helpers ---------------------------------------------------------------

func nyTZ(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

func mkVenue(t *testing.T, ref, name string) domain.Venue {
	t.Helper()
	return domain.Venue{
		Provider: "resy",
		Ref:      ref,
		Name:     name,
		TZ:       nyTZ(t),
	}
}

// captureLogger returns a slog.Logger that writes JSON lines into the
// returned buffer; tests assert on the buffer contents.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// ---- tests -----------------------------------------------------------------

func TestResolveSlug_FreshCacheHit(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	want := mkVenue(t, "49716", "Astoria DC")
	// Seed a fresh cache entry.
	_ = cache.UpsertVenueCache(context.Background(), "astoria-dc", "washington-dc",
		want, clk.Now().Add(-time.Hour))

	r := resolver.New(prov, cache, clk, resolver.WithTTL(24*time.Hour))
	got, err := r.Resolve(context.Background(),
		domain.VenueQuerySlug{Slug: "astoria-dc", City: "washington-dc"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Ref != want.Ref {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if rc, _ := prov.calls(); rc != 0 {
		t.Errorf("expected 0 provider calls on fresh hit, got %d", rc)
	}
}

func TestResolveSlug_CacheMissPopulates(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	want := mkVenue(t, "49716", "Astoria DC")
	prov.resolveVenue["astoria-dc|washington-dc"] = want

	r := resolver.New(prov, cache, clk)
	got, err := r.Resolve(context.Background(),
		domain.VenueQuerySlug{Slug: "astoria-dc", City: "washington-dc"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Ref != want.Ref {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if rc, _ := prov.calls(); rc != 1 {
		t.Errorf("expected 1 provider call, got %d", rc)
	}
	// Cache should now hold the freshly-fetched row.
	cached, err := cache.GetVenueCache(context.Background(), "astoria-dc", "washington-dc")
	if err != nil {
		t.Fatalf("post-resolve cache: %v", err)
	}
	if cached.Venue.Ref != want.Ref {
		t.Errorf("cached %+v, want %+v", cached.Venue, want)
	}
	if !cached.CachedAt.Equal(clk.Now()) {
		t.Errorf("cachedAt = %v, want %v", cached.CachedAt, clk.Now())
	}
}

func TestResolveSlug_StaleCacheRefreshes(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	old := mkVenue(t, "49716", "Astoria DC (old name)")
	fresh := mkVenue(t, "49716", "Astoria DC")
	_ = cache.UpsertVenueCache(context.Background(), "astoria-dc", "washington-dc",
		old, clk.Now().Add(-48*time.Hour)) // stale
	prov.resolveVenue["astoria-dc|washington-dc"] = fresh

	r := resolver.New(prov, cache, clk, resolver.WithTTL(24*time.Hour))
	got, err := r.Resolve(context.Background(),
		domain.VenueQuerySlug{Slug: "astoria-dc", City: "washington-dc"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != fresh.Name {
		t.Errorf("got name = %q, want %q (refresh did not happen)", got.Name, fresh.Name)
	}
	if rc, _ := prov.calls(); rc != 1 {
		t.Errorf("expected 1 provider call on stale refresh, got %d", rc)
	}
}

func TestResolveSlug_StaleOnFailureServesCache(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	cached := mkVenue(t, "49716", "Astoria DC")
	_ = cache.UpsertVenueCache(context.Background(), "astoria-dc", "washington-dc",
		cached, clk.Now().Add(-48*time.Hour)) // stale
	prov.resolveErr = providers.ErrRateLimited

	log, buf := captureLogger()
	r := resolver.New(prov, cache, clk,
		resolver.WithTTL(24*time.Hour),
		resolver.WithLogger(log),
	)
	got, err := r.Resolve(context.Background(),
		domain.VenueQuerySlug{Slug: "astoria-dc", City: "washington-dc"})
	if err != nil {
		t.Fatalf("Resolve: %v, want nil (stale served)", err)
	}
	if got.Ref != cached.Ref {
		t.Errorf("got %+v, want %+v", got, cached)
	}
	if !strings.Contains(buf.String(), "resolver.stale_served") {
		t.Errorf("expected stale_served log, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "astoria-dc") {
		t.Errorf("log missing slug: %s", buf.String())
	}
}

func TestResolveSlug_NotFoundErrorPassesThrough(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	// No matching entry; default error from fakeProvider.ResolveVenue
	// is providers.ErrVenueNotFound.

	r := resolver.New(prov, cache, clk)
	_, err := r.Resolve(context.Background(),
		domain.VenueQuerySlug{Slug: "no-such", City: "nowhere"})
	if !errors.Is(err, resolver.ErrVenueNotFound) {
		t.Fatalf("err = %v, want ErrVenueNotFound", err)
	}
}

func TestResolveURL_GarbagePassesThroughURLError(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r := resolver.New(prov, cache, clk)
	_, err := r.Resolve(context.Background(),
		domain.VenueQueryURL{URL: "https://opentable.com/cities/x/venues/y"})
	if !errors.Is(err, resolver.ErrNotResyHost) {
		t.Fatalf("err = %v, want ErrNotResyHost", err)
	}
	if rc, _ := prov.calls(); rc != 0 {
		t.Errorf("expected 0 provider calls on malformed URL, got %d", rc)
	}
}

func TestResolveURL_HappyPath(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	want := mkVenue(t, "49716", "Astoria DC")
	prov.resolveVenue["astoria-dc|washington-dc"] = want

	r := resolver.New(prov, cache, clk)
	got, err := r.Resolve(context.Background(),
		domain.VenueQueryURL{URL: "https://resy.com/cities/washington-dc/venues/astoria-dc?utm_source=x"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Ref != want.Ref {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveName_SingleHit(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	want := mkVenue(t, "49716", "Astoria DC")
	prov.searchHits["astoria"] = []domain.Venue{want}

	r := resolver.New(prov, cache, clk)
	got, err := r.Resolve(context.Background(),
		domain.VenueQueryName{Name: "Astoria"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Ref != want.Ref {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveName_AmbiguousReturnsCandidates(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	a := mkVenue(t, "49716", "Astoria DC")
	b := mkVenue(t, "12345", "Astoria NYC")
	prov.searchHits["astoria"] = []domain.Venue{a, b}

	r := resolver.New(prov, cache, clk)
	_, err := r.Resolve(context.Background(),
		domain.VenueQueryName{Name: "Astoria"})
	if !errors.Is(err, resolver.ErrVenueAmbiguous) {
		t.Fatalf("err = %v, want ErrVenueAmbiguous", err)
	}
	var amb *resolver.AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v, want *AmbiguousError", err)
	}
	if len(amb.Candidates) != 2 {
		t.Errorf("len(Candidates) = %d, want 2", len(amb.Candidates))
	}
	if amb.Query != "Astoria" {
		t.Errorf("Query = %q, want %q", amb.Query, "Astoria")
	}
}

func TestResolveName_NoHitsReturnsNotFound(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	// No seeded hits.

	r := resolver.New(prov, cache, clk)
	_, err := r.Resolve(context.Background(),
		domain.VenueQueryName{Name: "nonexistent venue xyz"})
	if !errors.Is(err, resolver.ErrVenueNotFound) {
		t.Fatalf("err = %v, want ErrVenueNotFound", err)
	}
}

func TestResolveName_StaleOnFailureServesCache(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	cachedHit := mkVenue(t, "49716", "Astoria DC")
	_ = cache.UpsertNameSearchCache(context.Background(), "astoria",
		[]domain.Venue{cachedHit}, clk.Now().Add(-48*time.Hour))
	prov.searchErr = providers.ErrRateLimited

	log, buf := captureLogger()
	r := resolver.New(prov, cache, clk,
		resolver.WithTTL(24*time.Hour),
		resolver.WithLogger(log),
	)
	got, err := r.Resolve(context.Background(),
		domain.VenueQueryName{Name: "Astoria"})
	if err != nil {
		t.Fatalf("Resolve: %v, want nil (stale served)", err)
	}
	if got.Ref != cachedHit.Ref {
		t.Errorf("got %+v, want %+v", got, cachedHit)
	}
	if !strings.Contains(buf.String(), "resolver.stale_served") {
		t.Errorf("expected stale_served log, got: %s", buf.String())
	}
}

func TestResolveName_FreshCacheHitSkipsProvider(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	cachedHit := mkVenue(t, "49716", "Astoria DC")
	_ = cache.UpsertNameSearchCache(context.Background(), "astoria",
		[]domain.Venue{cachedHit}, clk.Now().Add(-time.Hour))

	r := resolver.New(prov, cache, clk, resolver.WithTTL(24*time.Hour))
	got, err := r.Resolve(context.Background(),
		domain.VenueQueryName{Name: "Astoria"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Ref != cachedHit.Ref {
		t.Errorf("got %+v, want %+v", got, cachedHit)
	}
	if _, sc := prov.calls(); sc != 0 {
		t.Errorf("expected 0 search calls on fresh hit, got %d", sc)
	}
}

func TestResolverTTL_EnvOverride(t *testing.T) {
	t.Setenv("RESY_SNIPE_RESOLVER_TTL", "1h")

	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	// Seed cache with a 2h-old entry — past the 1h env TTL.
	old := mkVenue(t, "49716", "Astoria DC (old)")
	fresh := mkVenue(t, "49716", "Astoria DC")
	_ = cache.UpsertVenueCache(context.Background(), "astoria-dc", "washington-dc",
		old, clk.Now().Add(-2*time.Hour))
	prov.resolveVenue["astoria-dc|washington-dc"] = fresh

	r := resolver.New(prov, cache, clk) // no WithTTL — pulls from env
	got, err := r.Resolve(context.Background(),
		domain.VenueQuerySlug{Slug: "astoria-dc", City: "washington-dc"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != fresh.Name {
		t.Errorf("got name = %q, want %q (env TTL not respected)", got.Name, fresh.Name)
	}
}

func TestResolverTTL_DefaultOnUnsetEnv(t *testing.T) {
	t.Setenv("RESY_SNIPE_RESOLVER_TTL", "") // unset

	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	// 12h old — fresh under 24h default.
	cached := mkVenue(t, "49716", "Astoria DC")
	_ = cache.UpsertVenueCache(context.Background(), "astoria-dc", "washington-dc",
		cached, clk.Now().Add(-12*time.Hour))

	r := resolver.New(prov, cache, clk)
	if _, err := r.Resolve(context.Background(),
		domain.VenueQuerySlug{Slug: "astoria-dc", City: "washington-dc"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rc, _ := prov.calls(); rc != 0 {
		t.Errorf("expected 0 provider calls under default TTL, got %d", rc)
	}
}

func TestResolveURL_NilQueryRejected(t *testing.T) {
	t.Parallel()
	prov := newFakeProvider()
	cache := newMemCache()
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r := resolver.New(prov, cache, clk)
	if _, err := r.Resolve(context.Background(), nil); err == nil {
		t.Fatal("Resolve(nil) returned nil error")
	}
}
