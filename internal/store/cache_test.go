package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/store"
)

func sampleVenue(t *testing.T, name, ref string) domain.Venue {
	t.Helper()
	tz, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return domain.Venue{
		Provider: "resy",
		Ref:      ref,
		Name:     name,
		TZ:       tz,
	}
}

func TestUpsertVenueCacheRoundTrip(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	ctx := context.Background()

	v := sampleVenue(t, "Astoria DC", "49716")
	cachedAt := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	entry := store.VenueCacheEntry{
		Slug:     "astoria-dc",
		City:     "washington-dc",
		Venue:    v,
		CachedAt: cachedAt,
	}
	if err := store.UpsertVenueCache(ctx, db, entry); err != nil {
		t.Fatalf("UpsertVenueCache: %v", err)
	}

	got, err := store.GetVenueCache(ctx, db, "astoria-dc", "washington-dc")
	if err != nil {
		t.Fatalf("GetVenueCache: %v", err)
	}
	if got.Slug != entry.Slug || got.City != entry.City {
		t.Errorf("key drift: got (%q,%q) want (%q,%q)", got.Slug, got.City, entry.Slug, entry.City)
	}
	if got.Venue.Provider != v.Provider || got.Venue.Ref != v.Ref || got.Venue.Name != v.Name {
		t.Errorf("venue drift: got %+v want %+v", got.Venue, v)
	}
	if got.Venue.TZ == nil || got.Venue.TZ.String() != v.TZ.String() {
		t.Errorf("tz drift: got %v want %v", got.Venue.TZ, v.TZ)
	}
	if !got.CachedAt.Equal(cachedAt) {
		t.Errorf("cachedAt drift: got %v want %v", got.CachedAt, cachedAt)
	}
}

func TestGetVenueCacheMiss(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	ctx := context.Background()

	_, err := store.GetVenueCache(ctx, db, "no-such", "nowhere")
	if !errors.Is(err, store.ErrCacheMiss) {
		t.Fatalf("err = %v, want ErrCacheMiss", err)
	}
}

func TestUpsertVenueCacheIdempotent(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	ctx := context.Background()

	v1 := sampleVenue(t, "Astoria DC", "49716")
	t0 := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(2 * time.Hour)

	if err := store.UpsertVenueCache(ctx, db, store.VenueCacheEntry{
		Slug: "astoria-dc", City: "washington-dc", Venue: v1, CachedAt: t0,
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Re-upsert with same key, newer CachedAt, slightly different name.
	v2 := sampleVenue(t, "Astoria DC (renamed)", "49716")
	if err := store.UpsertVenueCache(ctx, db, store.VenueCacheEntry{
		Slug: "astoria-dc", City: "washington-dc", Venue: v2, CachedAt: t1,
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	// Only one row should exist.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM venues_cache`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("row count = %d, want 1", n)
	}
	got, err := store.GetVenueCache(ctx, db, "astoria-dc", "washington-dc")
	if err != nil {
		t.Fatalf("GetVenueCache: %v", err)
	}
	if got.Venue.Name != v2.Name {
		t.Errorf("name = %q, want %q", got.Venue.Name, v2.Name)
	}
	if !got.CachedAt.Equal(t1) {
		t.Errorf("cachedAt = %v, want %v", got.CachedAt, t1)
	}
}

func TestUpsertNameSearchCacheRoundTrip(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	ctx := context.Background()

	hits := []domain.Venue{
		sampleVenue(t, "Astoria DC", "49716"),
		sampleVenue(t, "Astoria NYC", "12345"),
	}
	t0 := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	if err := store.UpsertNameSearchCache(ctx, db, "astoria", hits, t0); err != nil {
		t.Fatalf("UpsertNameSearchCache: %v", err)
	}

	got, err := store.GetNameSearchCache(ctx, db, "astoria")
	if err != nil {
		t.Fatalf("GetNameSearchCache: %v", err)
	}
	if got.Query != "astoria" {
		t.Errorf("query = %q, want astoria", got.Query)
	}
	if len(got.Hits) != 2 {
		t.Fatalf("len(Hits) = %d, want 2", len(got.Hits))
	}
	if got.Hits[0].Ref != "49716" || got.Hits[1].Ref != "12345" {
		t.Errorf("hits order drifted: %+v", got.Hits)
	}
	if got.Hits[0].TZ == nil || got.Hits[0].TZ.String() != "America/New_York" {
		t.Errorf("hit[0] tz = %v", got.Hits[0].TZ)
	}
	if !got.CachedAt.Equal(t0) {
		t.Errorf("cachedAt = %v, want %v", got.CachedAt, t0)
	}
}

func TestGetNameSearchCacheMiss(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	ctx := context.Background()

	_, err := store.GetNameSearchCache(ctx, db, "no-such-query")
	if !errors.Is(err, store.ErrCacheMiss) {
		t.Fatalf("err = %v, want ErrCacheMiss", err)
	}
}

func TestUpsertNameSearchCacheIdempotent(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	ctx := context.Background()

	hits1 := []domain.Venue{sampleVenue(t, "Astoria DC", "49716")}
	hits2 := []domain.Venue{
		sampleVenue(t, "Astoria DC", "49716"),
		sampleVenue(t, "Astoria NYC", "12345"),
	}
	t0 := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)

	if err := store.UpsertNameSearchCache(ctx, db, "astoria", hits1, t0); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := store.UpsertNameSearchCache(ctx, db, "astoria", hits2, t1); err != nil {
		t.Fatalf("second: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM name_search_cache`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("row count = %d, want 1", n)
	}
	got, err := store.GetNameSearchCache(ctx, db, "astoria")
	if err != nil {
		t.Fatalf("GetNameSearchCache: %v", err)
	}
	if len(got.Hits) != 2 {
		t.Errorf("len(Hits) = %d, want 2", len(got.Hits))
	}
	if !got.CachedAt.Equal(t1) {
		t.Errorf("cachedAt = %v, want %v", got.CachedAt, t1)
	}
}

func TestUpsertNameSearchCacheEmptyHits(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)
	ctx := context.Background()

	t0 := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)
	if err := store.UpsertNameSearchCache(ctx, db, "nonexistent venue", nil, t0); err != nil {
		t.Fatalf("UpsertNameSearchCache: %v", err)
	}
	got, err := store.GetNameSearchCache(ctx, db, "nonexistent venue")
	if err != nil {
		t.Fatalf("GetNameSearchCache: %v", err)
	}
	if len(got.Hits) != 0 {
		t.Errorf("len(Hits) = %d, want 0", len(got.Hits))
	}
}
