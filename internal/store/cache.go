package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"resy-snipe/internal/domain"
)

// ErrCacheMiss is returned by Get*Cache when no row exists for the
// supplied key. Callers branch on errors.Is(err, ErrCacheMiss) — this
// is distinct from ErrNotFound because resolver cache misses are
// expected (and benign) whereas a missing snipe/session/venue row is
// always a logic error somewhere upstream.
var ErrCacheMiss = errors.New("store: cache miss")

// VenueCacheEntry is one row of the cross-tenant venues_cache table.
// The cache is global because Resy venue identity is public data: any
// homelab user benefits from a cached `(slug, city) -> Venue` lookup,
// and no per-user information rides on these rows. See
// docs/v2/design/multi-user.md §10 (cross-tenant tables) and
// docs/v2/design/resolver.md §caching-contract.
type VenueCacheEntry struct {
	Slug     string
	City     string
	Venue    domain.Venue
	CachedAt time.Time
}

// NameSearchCacheEntry is one row of the cross-tenant
// name_search_cache table. Same cross-tenant rationale as
// VenueCacheEntry: a name → []Venue lookup is public Resy data.
type NameSearchCacheEntry struct {
	Query    string
	Hits     []domain.Venue
	CachedAt time.Time
}

// venueJSON is the on-disk shape for a domain.Venue inside
// venues_cache.venue_json / name_search_cache.results_json. We do not
// use domain.Venue directly because its TZ field is a *time.Location
// (not JSON-serialisable) — the codec carries the IANA name and
// reconstructs the location on read.
type venueJSON struct {
	Provider domain.ProviderID `json:"provider"`
	Ref      string            `json:"ref"`
	Name     string            `json:"name"`
	TZ       string            `json:"tz"`
}

func encodeVenue(v domain.Venue) venueJSON {
	tz := ""
	if v.TZ != nil {
		tz = v.TZ.String()
	}
	return venueJSON{
		Provider: v.Provider,
		Ref:      v.Ref,
		Name:     v.Name,
		TZ:       tz,
	}
}

func (j venueJSON) decode() (domain.Venue, error) {
	v := domain.Venue{
		Provider: j.Provider,
		Ref:      j.Ref,
		Name:     j.Name,
	}
	if j.TZ != "" {
		loc, err := time.LoadLocation(j.TZ)
		if err != nil {
			return domain.Venue{}, fmt.Errorf("cache: load tz %q: %w", j.TZ, err)
		}
		v.TZ = loc
	}
	return v, nil
}

// UpsertVenueCache writes a venues_cache row keyed on (slug, city).
// An existing row with the same key is overwritten — the cache is
// write-last-wins per docs/v2/design/resolver.md §caching-contract.
//
// Cross-tenant: no userID parameter. venues_cache is global because
// Resy venue identity is public data (multi-user.md §10).
func UpsertVenueCache(ctx context.Context, db *sql.DB, entry VenueCacheEntry) error {
	if db == nil {
		return errors.New("UpsertVenueCache: nil db")
	}
	if entry.Slug == "" || entry.City == "" {
		return errors.New("UpsertVenueCache: slug and city are required")
	}
	payload, err := json.Marshal(encodeVenue(entry.Venue))
	if err != nil {
		return fmt.Errorf("UpsertVenueCache: encode venue: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO venues_cache (slug, city, venue_json, resolved_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(slug, city) DO UPDATE SET
			venue_json  = excluded.venue_json,
			resolved_at = excluded.resolved_at`,
		entry.Slug, entry.City, string(payload), entry.CachedAt.Unix(),
	); err != nil {
		return fmt.Errorf("UpsertVenueCache: %w", err)
	}
	return nil
}

// GetVenueCache returns the cached entry for (slug, city) or
// ErrCacheMiss if no row exists. The returned entry's freshness is
// the caller's call — TTL is enforced at the resolver layer.
//
// Cross-tenant: no userID parameter. See UpsertVenueCache.
func GetVenueCache(ctx context.Context, db *sql.DB, slug, city string) (VenueCacheEntry, error) {
	if db == nil {
		return VenueCacheEntry{}, errors.New("GetVenueCache: nil db")
	}
	var (
		venueJSONStr string
		resolvedAt   int64
	)
	err := db.QueryRowContext(ctx, `
		SELECT venue_json, resolved_at
		FROM venues_cache
		WHERE slug = ? AND city = ?`,
		slug, city,
	).Scan(&venueJSONStr, &resolvedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return VenueCacheEntry{}, fmt.Errorf("venue %s/%s: %w", slug, city, ErrCacheMiss)
		}
		return VenueCacheEntry{}, fmt.Errorf("GetVenueCache: %w", err)
	}
	var raw venueJSON
	if err := json.Unmarshal([]byte(venueJSONStr), &raw); err != nil {
		return VenueCacheEntry{}, fmt.Errorf("GetVenueCache: decode venue: %w", err)
	}
	venue, err := raw.decode()
	if err != nil {
		return VenueCacheEntry{}, fmt.Errorf("GetVenueCache: %w", err)
	}
	return VenueCacheEntry{
		Slug:     slug,
		City:     city,
		Venue:    venue,
		CachedAt: time.Unix(resolvedAt, 0).UTC(),
	}, nil
}

// UpsertNameSearchCache writes a name_search_cache row keyed on the
// (already-normalized) query string. An existing row is overwritten.
//
// Cross-tenant: no userID parameter.
func UpsertNameSearchCache(ctx context.Context, db *sql.DB, query string, hits []domain.Venue, cachedAt time.Time) error {
	if db == nil {
		return errors.New("UpsertNameSearchCache: nil db")
	}
	if query == "" {
		return errors.New("UpsertNameSearchCache: query is required")
	}
	encoded := make([]venueJSON, 0, len(hits))
	for _, h := range hits {
		encoded = append(encoded, encodeVenue(h))
	}
	payload, err := json.Marshal(encoded)
	if err != nil {
		return fmt.Errorf("UpsertNameSearchCache: encode hits: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO name_search_cache (query, results_json, resolved_at)
		VALUES (?, ?, ?)
		ON CONFLICT(query) DO UPDATE SET
			results_json = excluded.results_json,
			resolved_at  = excluded.resolved_at`,
		query, string(payload), cachedAt.Unix(),
	); err != nil {
		return fmt.Errorf("UpsertNameSearchCache: %w", err)
	}
	return nil
}

// GetNameSearchCache returns the cached entry for the supplied query
// or ErrCacheMiss if no row exists. TTL enforcement lives at the
// resolver layer.
//
// Cross-tenant: no userID parameter.
func GetNameSearchCache(ctx context.Context, db *sql.DB, query string) (NameSearchCacheEntry, error) {
	if db == nil {
		return NameSearchCacheEntry{}, errors.New("GetNameSearchCache: nil db")
	}
	var (
		resultsJSON string
		resolvedAt  int64
	)
	err := db.QueryRowContext(ctx, `
		SELECT results_json, resolved_at
		FROM name_search_cache
		WHERE query = ?`,
		query,
	).Scan(&resultsJSON, &resolvedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NameSearchCacheEntry{}, fmt.Errorf("name %q: %w", query, ErrCacheMiss)
		}
		return NameSearchCacheEntry{}, fmt.Errorf("GetNameSearchCache: %w", err)
	}
	var raw []venueJSON
	if err := json.Unmarshal([]byte(resultsJSON), &raw); err != nil {
		return NameSearchCacheEntry{}, fmt.Errorf("GetNameSearchCache: decode hits: %w", err)
	}
	hits := make([]domain.Venue, 0, len(raw))
	for _, r := range raw {
		v, err := r.decode()
		if err != nil {
			return NameSearchCacheEntry{}, fmt.Errorf("GetNameSearchCache: %w", err)
		}
		hits = append(hits, v)
	}
	return NameSearchCacheEntry{
		Query:    query,
		Hits:     hits,
		CachedAt: time.Unix(resolvedAt, 0).UTC(),
	}, nil
}
