package resolver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// DefaultTTL is the resolver-cache freshness window. Reads exceeding
// this age trigger an upstream refresh; on refresh failure the stale
// entry is served per stale-on-failure (design/resolver.md
// §stale-on-failure). Override at process start with
// RESY_SNIPE_RESOLVER_TTL (any value time.ParseDuration accepts).
const DefaultTTL = 24 * time.Hour

// resolverTTL reads RESY_SNIPE_RESOLVER_TTL from the environment. An
// empty or unparseable value falls back to DefaultTTL — the resolver
// chooses safety (a working default) over loud failure on boot.
//
// The env var is read exactly once per New() call so tests can swap
// it via t.Setenv; production wiring constructs the resolver once at
// startup so the cost is irrelevant.
func resolverTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv("RESY_SNIPE_RESOLVER_TTL"))
	if raw == "" {
		return DefaultTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultTTL
	}
	return d
}

// Resolver answers "who is this venue?" for the rest of the system.
// Resolve maps a domain.VenueQuery to a domain.Venue, cache-fronting
// the provider call per docs/v2/design/resolver.md.
type Resolver interface {
	Resolve(ctx context.Context, query domain.VenueQuery) (domain.Venue, error)
}

// CacheStore is the persistence seam the resolver depends on. It is
// defined at the consumer (per docs/laws.md Law 5) so the store
// package owns implementation and the resolver owns the contract.
// cmd/ wires a small adapter from *store.SQLiteStore to this
// interface.
//
// The four methods mirror the four store package-level functions
// (UpsertVenueCache/GetVenueCache/UpsertNameSearchCache/GetNameSearchCache)
// dropping the *sql.DB parameter — the adapter binds it.
type CacheStore interface {
	UpsertVenueCache(ctx context.Context, slug, city string, venue domain.Venue, cachedAt time.Time) error
	GetVenueCache(ctx context.Context, slug, city string) (cachedVenue CachedVenue, err error)
	UpsertNameSearchCache(ctx context.Context, query string, hits []domain.Venue, cachedAt time.Time) error
	GetNameSearchCache(ctx context.Context, query string) (cachedHits CachedNameSearch, err error)
}

// CachedVenue is the resolver's view of a venues_cache row. Mirrors
// store.VenueCacheEntry but stays in the resolver's domain so the
// resolver does not import internal/store directly (Law 5).
type CachedVenue struct {
	Venue    domain.Venue
	CachedAt time.Time
}

// CachedNameSearch is the resolver's view of a name_search_cache row.
type CachedNameSearch struct {
	Hits     []domain.Venue
	CachedAt time.Time
}

// ErrCacheMiss is what CacheStore implementations return when a row
// does not exist. Defined at the consumer so callers branch on a
// resolver-local sentinel; the store package emits its own
// store.ErrCacheMiss that the adapter translates.
var ErrCacheMiss = errors.New("resolver: cache miss")

// Option configures a StandardResolver at construction time.
type Option func(*StandardResolver)

// WithTTL pins a custom TTL, overriding DefaultTTL and the
// RESY_SNIPE_RESOLVER_TTL env var. Tests use this to keep cache
// freshness deterministic across the fake clock.
func WithTTL(ttl time.Duration) Option {
	return func(r *StandardResolver) {
		if ttl > 0 {
			r.ttl = ttl
		}
	}
}

// WithLogger pins the slog.Logger used for stale-served warnings and
// other resolver telemetry. A nil logger falls back to slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(r *StandardResolver) {
		if log != nil {
			r.log = log
		}
	}
}

// StandardResolver is the production Resolver. It owns:
//
//   - a providers.Provider for /3/venue and /3/venuesearch calls.
//   - a CacheStore for venues_cache and name_search_cache.
//   - a clock.Clock for cache freshness comparisons (Law 7 — never
//     calling time directly from this package).
//   - a TTL bound (resolved at New() time from the env or option).
//   - a slog.Logger for the single stale-served warning line per
//     design/resolver.md §stale-on-failure.
type StandardResolver struct {
	provider providers.Provider
	cache    CacheStore
	clock    clock.Clock
	ttl      time.Duration
	log      *slog.Logger
}

// Compile-time interface assertion.
var _ Resolver = (*StandardResolver)(nil)

// New constructs a StandardResolver. provider, cache, and clock are
// required; nil arguments cause New to panic at startup rather than
// surface a confusing nil-pointer panic at first Resolve.
//
// The TTL is sourced from RESY_SNIPE_RESOLVER_TTL (or DefaultTTL on
// unset/invalid). WithTTL overrides both.
//
// The logger defaults to slog.Default(); WithLogger overrides.
func New(provider providers.Provider, cache CacheStore, clk clock.Clock, opts ...Option) *StandardResolver {
	if provider == nil {
		panic("resolver.New: provider is required")
	}
	if cache == nil {
		panic("resolver.New: cache is required")
	}
	if clk == nil {
		panic("resolver.New: clock is required")
	}
	r := &StandardResolver{
		provider: provider,
		cache:    cache,
		clock:    clk,
		ttl:      resolverTTL(),
		log:      slog.Default(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Resolve dispatches on the VenueQuery variant and returns the
// canonical domain.Venue.
//
// The algorithm follows docs/v2/design/resolver.md §resolution-algorithm:
//
//   - VenueQueryURL is parsed into (slug, city); the URL and slug
//     paths converge at the cache lookup.
//   - VenueQuerySlug skips parsing and goes straight to the cache.
//   - VenueQueryName consults name_search_cache, then
//     SearchVenuesByName; 0 hits → ErrVenueNotFound, 1 hit → return
//     it, >1 hits → *AmbiguousError.
//
// Stale-on-failure: if the upstream call fails AND a cache row
// exists (even past TTL), the cached row is returned with a single
// "resolver.stale_served" WARN log line.
func (r *StandardResolver) Resolve(ctx context.Context, query domain.VenueQuery) (domain.Venue, error) {
	if query == nil {
		return domain.Venue{}, ErrInvalidURL
	}
	switch q := query.(type) {
	case domain.VenueQueryURL:
		return r.resolveURL(ctx, q.URL)
	case domain.VenueQuerySlug:
		return r.resolveSlug(ctx, q.Slug, q.City)
	case domain.VenueQueryName:
		return r.resolveName(ctx, q.Name)
	default:
		return domain.Venue{}, fmt.Errorf("resolver: unsupported VenueQuery variant %T", query)
	}
}

// resolveURL parses the URL and delegates to resolveSlug. A malformed
// URL surfaces the ParseResyURL sentinel verbatim — callers branch
// on errors.Is(err, resolver.ErrInvalidURL) etc.
func (r *StandardResolver) resolveURL(ctx context.Context, raw string) (domain.Venue, error) {
	_, hints, err := ParseResyURL(raw)
	if err != nil {
		return domain.Venue{}, err
	}
	return r.resolveSlug(ctx, hints.Slug, hints.City)
}

// resolveSlug is the cache-fronted ResolveVenue path. It handles the
// three cache states: fresh hit (return cached), stale hit (refresh,
// fall back to cached on failure), miss (fetch + populate).
func (r *StandardResolver) resolveSlug(ctx context.Context, slug, city string) (domain.Venue, error) {
	slug = strings.TrimSpace(slug)
	city = strings.TrimSpace(city)
	if slug == "" || city == "" {
		return domain.Venue{}, fmt.Errorf("%w: slug and city are required", ErrInvalidURL)
	}

	cached, cacheErr := r.cache.GetVenueCache(ctx, slug, city)
	hasCache := cacheErr == nil

	if hasCache && !r.isStale(cached.CachedAt) {
		return cached.Venue, nil
	}

	// Either we have a stale entry or a clean miss. Either way the
	// provider call is the same. Stale-on-failure: if the provider
	// fails AND we have a stale entry, serve the stale entry.
	fetched, fetchErr := r.provider.ResolveVenue(ctx, slug, city)
	if fetchErr != nil {
		if hasCache && !errors.Is(fetchErr, providers.ErrVenueNotFound) {
			r.log.Warn("resolver.stale_served",
				slog.String("slug", slug),
				slog.String("city", city),
				slog.Duration("age", r.clock.Now().Sub(cached.CachedAt)),
				slog.String("upstream_err", fetchErr.Error()),
			)
			return cached.Venue, nil
		}
		if errors.Is(fetchErr, providers.ErrVenueNotFound) {
			return domain.Venue{}, fmt.Errorf("slug=%s city=%s: %w", slug, city, ErrVenueNotFound)
		}
		return domain.Venue{}, fetchErr
	}

	// Write-through. A write failure is non-fatal for the resolve —
	// log it and return the fresh venue anyway.
	if err := r.cache.UpsertVenueCache(ctx, slug, city, fetched, r.clock.Now()); err != nil {
		r.log.Warn("resolver.cache_write_failed",
			slog.String("slug", slug),
			slog.String("city", city),
			slog.String("err", err.Error()),
		)
	}
	return fetched, nil
}

// resolveName is the name_search_cache-fronted SearchVenuesByName
// path. 0 hits → ErrVenueNotFound. 1 hit → return it. >1 hits →
// *AmbiguousError with the candidate list attached.
func (r *StandardResolver) resolveName(ctx context.Context, name string) (domain.Venue, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return domain.Venue{}, fmt.Errorf("%w: name is required", ErrInvalidURL)
	}
	key := normalizeNameQuery(trimmed)

	cached, cacheErr := r.cache.GetNameSearchCache(ctx, key)
	hasCache := cacheErr == nil

	if hasCache && !r.isStale(cached.CachedAt) {
		return r.pickFromHits(trimmed, cached.Hits)
	}

	hits, fetchErr := r.provider.SearchVenuesByName(ctx, trimmed)
	if fetchErr != nil {
		if hasCache {
			r.log.Warn("resolver.stale_served",
				slog.String("query", trimmed),
				slog.Duration("age", r.clock.Now().Sub(cached.CachedAt)),
				slog.String("upstream_err", fetchErr.Error()),
			)
			return r.pickFromHits(trimmed, cached.Hits)
		}
		return domain.Venue{}, fetchErr
	}

	if err := r.cache.UpsertNameSearchCache(ctx, key, hits, r.clock.Now()); err != nil {
		r.log.Warn("resolver.cache_write_failed",
			slog.String("query", trimmed),
			slog.String("err", err.Error()),
		)
	}
	return r.pickFromHits(trimmed, hits)
}

// pickFromHits applies the name-resolution policy: 0 hits = not
// found, 1 hit = winner, ≥2 hits = AmbiguousError carrying all
// candidates. The original (untrimmed-but-trimmed) query is stamped
// onto the error so the Service layer can render a picker prompt.
func (r *StandardResolver) pickFromHits(query string, hits []domain.Venue) (domain.Venue, error) {
	switch len(hits) {
	case 0:
		return domain.Venue{}, fmt.Errorf("name=%q: %w", query, ErrVenueNotFound)
	case 1:
		return hits[0], nil
	default:
		// Copy to defend against later mutation of the cache slice.
		cands := make([]domain.Venue, len(hits))
		copy(cands, hits)
		return domain.Venue{}, &AmbiguousError{Query: query, Candidates: cands}
	}
}

// isStale reports whether a cache entry written at cachedAt is past
// the configured TTL relative to the resolver's clock.
func (r *StandardResolver) isStale(cachedAt time.Time) bool {
	return r.clock.Now().Sub(cachedAt) > r.ttl
}

// normalizeNameQuery folds the name query into a stable cache key:
// lower-case, whitespace-collapsed. The store layer treats the key
// as opaque so two callers typing "Astoria " and "astoria" land on
// the same row.
func normalizeNameQuery(q string) string {
	lower := strings.ToLower(q)
	return strings.Join(strings.Fields(lower), " ")
}
