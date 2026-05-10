package main

import (
	"context"
	"errors"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/resolver"
	"resy-snipe/internal/store"
)

// resolverCacheAdapter binds a *store.SQLiteStore to the
// resolver.CacheStore interface defined-at-the-consumer per Law 5.
// The resolver package owns the CacheStore contract; the store
// package owns the four package-level cache CRUD functions; this
// adapter — owned by the cmd/ wiring layer — bridges them.
//
// The adapter translates store.ErrCacheMiss into resolver.ErrCacheMiss
// on every read path so resolver code branches on its own sentinel.
// Other store errors propagate verbatim (write failures, JSON decode
// errors, raw SQL transport problems) because the resolver layer has
// no opinion on them — it logs and returns.
type resolverCacheAdapter struct {
	store *store.SQLiteStore
}

// newResolverCacheAdapter constructs the adapter. The store must be
// non-nil; nil here is a programming error and panics rather than
// surfacing as a confusing nil-deref on first Resolve.
func newResolverCacheAdapter(s *store.SQLiteStore) *resolverCacheAdapter {
	if s == nil {
		panic("newResolverCacheAdapter: nil store")
	}
	return &resolverCacheAdapter{store: s}
}

// Compile-time check.
var _ resolver.CacheStore = (*resolverCacheAdapter)(nil)

// UpsertVenueCache delegates to the package-level store function,
// repackaging the (slug, city, venue, cachedAt) parameters into the
// store's VenueCacheEntry struct. The store layer enforces the
// (slug, city) uniqueness on the underlying table.
func (a *resolverCacheAdapter) UpsertVenueCache(
	ctx context.Context, slug, city string, venue domain.Venue, cachedAt time.Time,
) error {
	return store.UpsertVenueCache(ctx, a.store.DB(), store.VenueCacheEntry{
		Slug:     slug,
		City:     city,
		Venue:    venue,
		CachedAt: cachedAt,
	})
}

// GetVenueCache translates store.ErrCacheMiss → resolver.ErrCacheMiss
// so resolver callers branch on the resolver-local sentinel. Per
// Law 5, the two sentinels are intentionally distinct and the adapter
// is the seam.
func (a *resolverCacheAdapter) GetVenueCache(
	ctx context.Context, slug, city string,
) (resolver.CachedVenue, error) {
	entry, err := store.GetVenueCache(ctx, a.store.DB(), slug, city)
	if err != nil {
		if errors.Is(err, store.ErrCacheMiss) {
			return resolver.CachedVenue{}, resolver.ErrCacheMiss
		}
		return resolver.CachedVenue{}, err
	}
	return resolver.CachedVenue{
		Venue:    entry.Venue,
		CachedAt: entry.CachedAt,
	}, nil
}

// UpsertNameSearchCache delegates to the package-level store function.
func (a *resolverCacheAdapter) UpsertNameSearchCache(
	ctx context.Context, query string, hits []domain.Venue, cachedAt time.Time,
) error {
	return store.UpsertNameSearchCache(ctx, a.store.DB(), query, hits, cachedAt)
}

// GetNameSearchCache translates store.ErrCacheMiss → resolver.ErrCacheMiss.
func (a *resolverCacheAdapter) GetNameSearchCache(
	ctx context.Context, query string,
) (resolver.CachedNameSearch, error) {
	entry, err := store.GetNameSearchCache(ctx, a.store.DB(), query)
	if err != nil {
		if errors.Is(err, store.ErrCacheMiss) {
			return resolver.CachedNameSearch{}, resolver.ErrCacheMiss
		}
		return resolver.CachedNameSearch{}, err
	}
	return resolver.CachedNameSearch{
		Hits:     entry.Hits,
		CachedAt: entry.CachedAt,
	}, nil
}
