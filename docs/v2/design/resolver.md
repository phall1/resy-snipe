# Resolver

**Layer**: `internal/resolver` (new in v2 / M1)
**Status**: Design — implementation tracked under M1
**Related**: [ADR-0001](../adr/0001-goal-driven-architecture.md),
[ADR-0002](../adr/0002-resolver-planner-engine-layering.md),
[design/planner.md](planner.md),
[design/service-layer.md](service-layer.md)

## Purpose

The Resolver answers exactly one question: **"who is this venue?"**
Given a `Goal.VenueQuery` — a Resy URL, a `slug+city` pair, or a
freeform name — it returns a `domain.Venue` populated with the fields
the rest of the system relies on (provider id, ref, display name,
timezone, release-window config). It does not plan, does not check
inventory, does not book. It does not know about `Goal.Date` or party
size; identity is independent of intent.

The Resolver exists because v1 made the user the directory: venue ids
were hand-curated in `cmd/resy-snipe/intent.go` and any new venue
required a code change. v2 promotes "venue identity" to a first-class
concern with its own cache, its own failure modes, and its own
provider seam.

## Inputs: the `VenueQuery` union

```go
// internal/domain/venue_query.go
type VenueQuery struct {
    URL  string  // "https://resy.com/cities/dc/venues/astoria-dc?..."
    Slug string  // "astoria-dc"
    City string  // "washington-dc" (Resy's location code)
    Name string  // "Astoria"
}
```

Exactly one of `{URL, (Slug,City), Name}` is non-empty. The constructor
helpers `domain.VenueQueryURL(s)`, `domain.VenueQuerySlug(slug, city)`,
`domain.VenueQueryName(name)` are the only sanctioned ways to build
one; bare struct literals are a smell flagged in review.

### URL form

Canonical Resy venue URL:

```
https://resy.com/cities/<city>/venues/<slug>?date=...&seats=...
```

Resolver strips query and fragment, splits the path, and extracts
`(city, slug)`. Tracking parameters (`utm_*`, `date`, `seats`,
session ids) are discarded — they're noise from how the user got to
the page and have no bearing on identity. Trailing slashes,
URL-encoded slugs, and case in the host are all normalised.

A URL whose path doesn't match `/cities/{city}/venues/{slug}` is
rejected with `ErrVenueQueryMalformed` before any network call.

### `slug+city` form

Already canonical. Skip parsing; go straight to cache + `/3/venue`.
This is the form everything internal speaks: the cache is keyed on it,
the planner stores it on the Quest, and the Resy adapter passes it
verbatim to `url_slug=&location=`.

### Name form

Freeform — "astoria", "shion 69 leonard", "that pizza place in
brooklyn". Resolver hits `/3/venuesearch/search`, returns the top N
candidates ordered by Resy's relevance score. The Service layer
decides whether to auto-pick (single high-confidence hit) or surface
the list for disambiguation; the Resolver never auto-picks on the
caller's behalf.

## Output: `domain.Venue`

The existing `domain.Venue`
([internal/domain/venue.go](../../../internal/domain/venue.go)) carries
`Provider`, `Ref`, `Name`, `TZ`. v2 adds the release-window block the
Planner needs:

```go
// Proposed v2 additions to domain.Venue. TZ remains load-bearing.
type Venue struct {
    Provider     ProviderID
    Ref          string          // Resy numeric venue id, e.g. "49716"
    Slug         string          // "astoria-dc"
    City         string          // "washington-dc"
    Name         string
    TZ           *time.Location
    Release      ReleaseConfig   // NEW
    ResolvedAt   time.Time       // NEW — for cache TTL accounting
}

// ReleaseConfig is the venue's drop-window contract as advertised by
// /3/venue. The Planner consumes this to derive the snipe-time; the
// Resolver only transports it.
type ReleaseConfig struct {
    DaysAhead       int           // e.g. 28 for "books 28 days out"
    LocalTime       WallTime      // e.g. 09:00:00 venue-local
    Source          string        // "venue_config" | "observed" | "default"
}
```

`Source` records provenance because Resy's `/3/venue` response is the
authoritative case (`venue_config`), the observed-release table is the
fallback (`observed`), and the system default is the last resort
(`default`). The Planner branches on this to decide how aggressively
to schedule the drop.

## Resolution algorithm

```
                 ┌───────────────────┐
   VenueQuery ──►│   Resolver.Resolve│
                 └─────────┬─────────┘
                           │
              ┌────────────┼────────────┐
              ▼            ▼            ▼
            URL?        Slug+City?     Name?
              │            │            │
              ▼            │            ▼
        parse URL ────────►│   /3/venuesearch ──► [Venue, ...]
        (slug+city)        │   (or cache)        │
                           ▼                     ▼
                    venues_cache          (return list to caller)
                    hit?
                    ├─ yes & fresh ──► return cached Venue
                    ├─ yes & stale ──► try upstream; on failure
                    │                  return cached + log "stale"
                    └─ miss ─────────► /3/venue?url_slug&location
                                        │
                                        ├─ 200 ──► persist + return
                                        ├─ 404 ──► ErrVenueNotFound
                                        ├─ 429 ──► ErrUpstreamUnavailable
                                        └─ 5xx ──► fall back to stale
                                                   if present, else
                                                   ErrUpstreamUnavailable
```

URL and `slug+city` paths converge at the cache lookup. Name path is
distinct: it has its own cache (`name_search_cache`, see below) and
returns a list rather than a single venue. A caller that wants a
`domain.Venue` from a name must call `Resolver.Search` then
`Resolver.Resolve` with the chosen `slug+city`.

### Why two passes for name

The Service layer treats name disambiguation as a UI concern. Folding
"pick one" into the Resolver would force every caller (CLI, MCP,
future web frontend) to inherit the same picker policy. Instead, the
Resolver returns evidence; the Service layer returns a decision.

## Caching contract

Persistence lives in the existing SQLite store via two new tables.
Schema lives in `internal/store/migrate.go`; access is gated by a
small interface `resolver.Cache` that the resolver consumes.

```sql
CREATE TABLE venues_cache (
    slug         TEXT NOT NULL,
    city         TEXT NOT NULL,
    venue_json   TEXT NOT NULL,   -- serialised domain.Venue
    resolved_at  INTEGER NOT NULL, -- unix seconds, clock.Now
    PRIMARY KEY (slug, city)
);

CREATE TABLE name_search_cache (
    query        TEXT PRIMARY KEY,
    results_json TEXT NOT NULL,   -- serialised []domain.Venue
    resolved_at  INTEGER NOT NULL
);
```

### TTL

Default 24h, configurable via `RESY_SNIPE_RESOLVER_TTL`. Rationale:

- Resy's release-time config does change (venues experiment with drop
  windows). 24h is short enough to catch the change before the next
  Quest fires, long enough that 99% of resolves are cache hits.
- Name-search results are noisier; same TTL, but the Service layer
  may pass `Force=true` to bypass when the user explicitly retries
  with a clarifying query.

### Stale-on-failure

When the upstream is unreachable (timeout, 5xx, network error), the
Resolver returns the cached entry regardless of TTL and emits a
single `slog.Warn` log line:

```
resolver.stale_served slug=astoria-dc city=washington-dc
    age=3h12m upstream_err="dial tcp: i/o timeout"
```

This is the only place in the system where stale data is preferred
over an error. Justification: a Quest that already booked at venue X
yesterday should not fail to *replan* today because Resy's edge is
flaky. The cached `ReleaseConfig` is good enough until the network
recovers.

`ErrVenueNotFound` is *not* a "failure" for stale-on-failure purposes
— a 404 is a definitive negative, not a transport blip, and the
cache entry (if any) is invalidated.

## API client contract

The provider seam grows two methods. Existing `SearchVenues` is
already on `providers.Provider`; the v1 implementation is a stub that
returns `ErrInventoryEmpty`.

```go
// internal/providers/provider.go (additions)
type Provider interface {
    // ... existing methods ...

    // ResolveVenue returns the canonical Venue for a (slug, city)
    // pair. Returns ErrVenueNotFound for 404, ErrUpstreamUnavailable
    // for 429/5xx/transport, ErrAntiBotChallenge if the response is
    // a challenge page.
    ResolveVenue(ctx context.Context, slug, city string) (domain.Venue, error)

    // SearchVenues returns a relevance-ordered list of candidates for
    // a freeform name query. Capped at 25 entries by the adapter.
    SearchVenues(ctx context.Context, q Query) ([]domain.Venue, error)
}
```

The Resy adapter implements `ResolveVenue` against
`GET https://api.resy.com/3/venue?url_slug=<slug>&location=<city>`,
using the public web-app API key
`VbWk7s3L4KiK5fzlO7JD3Q5EYolJI7n5` plus standard browser headers.
`SearchVenues` calls `/3/venuesearch/search` with the same auth.
Both go through `client.do()` so x-request-id, structured logging,
and the error classifier all apply uniformly.

The Resolver depends on `providers.Provider` (the interface), never
on `internal/resy`. Tests use a fake provider that returns canned
`domain.Venue` values.

## Error model

Sentinels live in `internal/resolver/errors.go`:

```go
var (
    // ErrVenueNotFound: /3/venue returned 404 (closed venue, wrong
    // slug, or private listing). Terminal for this VenueQuery.
    ErrVenueNotFound = errors.New("resolver: venue not found")

    // ErrVenueAmbiguous: name search returned multiple
    // similar-confidence hits and the caller did not pre-filter.
    // The error wraps a list of candidates via errors.As.
    ErrVenueAmbiguous = errors.New("resolver: venue ambiguous")

    // ErrUpstreamUnavailable: transport, 429, or 5xx, and no cached
    // entry was usable. Retryable.
    ErrUpstreamUnavailable = errors.New("resolver: upstream unavailable")

    // ErrVenueQueryMalformed: input failed validation before any
    // network call. Caller bug.
    ErrVenueQueryMalformed = errors.New("resolver: venue query malformed")
)

// AmbiguousError carries the candidate list. Service layer extracts
// it via errors.As to render a picker.
type AmbiguousError struct {
    Query      string
    Candidates []domain.Venue
}
func (e *AmbiguousError) Error() string { /* ... */ }
func (e *AmbiguousError) Is(target error) bool {
    return target == ErrVenueAmbiguous
}
```

CLI and MCP map these to surface text:

| Sentinel                  | CLI message                                             | MCP behaviour                       |
|---------------------------|---------------------------------------------------------|-------------------------------------|
| `ErrVenueNotFound`        | "couldn't find that venue on Resy"                      | tool error, no retry                |
| `ErrVenueAmbiguous`       | numbered picker on stdin                                | structured `candidates` field       |
| `ErrUpstreamUnavailable`  | "Resy unreachable; cached data shown if available"      | retry with backoff                  |
| `ErrVenueQueryMalformed`  | "that doesn't look like a Resy URL or slug"             | tool error, no retry                |

## Edge cases

1. **URL with tracking params** — strip everything after the path.
   `?utm_source=email`, `?date=2026-06-01`, `#reservation` all gone.
   Identity is path-only.

2. **City mismatch in URL** — Resy occasionally moves a venue
   (rebrand, relocation) and the *path* city is stale while the
   `/3/venue` *response* city is the new one. **Trust the URL** for
   the lookup key (it's what the user pasted), but record the
   response's city in the resolved `Venue.City`. The cache entry
   stores both: the lookup is `(path_slug, path_city)`, the value
   carries the response's `(canonical_slug, canonical_city)`. A
   subsequent resolve via the canonical pair lands on a *separate*
   cache row pointing at the same venue — wasteful but correct, and
   a periodic dedup migration can collapse them.

3. **Private/closed venues** — Resy returns 404 (sometimes 403). The
   adapter classifies both as `ErrVenueNotFound`; the cache is
   *invalidated* on this outcome, not populated, because a venue may
   come back.

4. **Anti-bot challenge** — `/3/venue` is normally an unauthenticated
   GET, but Resy's edge has been observed to challenge it under
   suspicious load. Adapter classifies into `ErrAntiBotChallenge`
   and the Resolver propagates it; the Service layer surfaces the
   `RESY_SNIPE_SIGNER_BIN` configuration hint
   ([anti-bot.md](../../anti-bot.md)).

5. **Slug with non-ASCII or URL-encoding** — Resy's slugs are
   ASCII-only by convention, but URL-encoded variants (`caf%C3%A9`)
   are decoded once and re-validated. A slug that fails ASCII after
   decoding is `ErrVenueQueryMalformed`.

6. **Empty name query** — `ErrVenueQueryMalformed`. We do not call
   `/3/venuesearch/search` with an empty string; Resy accepts it but
   returns garbage.

7. **Concurrent resolves of the same `(slug, city)`** — the cache is
   write-last-wins; both calls hit the network and both write. This
   is fine: the value is idempotent over the TTL window, and
   single-flighting adds complexity for negligible savings (the
   typical Quest resolves a venue exactly once at submission time).

## Dependency rules

Per [Law 1–4](../../laws.md):

- `internal/resolver` imports `internal/domain`, `internal/providers`,
  `internal/clock`, and a slim `resolver.Cache` interface that
  `cmd/` adapts the real `*store.SQLiteStore` to.
- It does **not** import `internal/resy`. Anything Resy-specific (the
  `/3/venue` URL, the API key, the response shape) lives in the
  adapter; the Resolver sees only `providers.Provider` and
  `domain.Venue`.
- It does not import `internal/engine`, `internal/notify`,
  `internal/planner`. The dependency arrow is one-way; the Service
  layer composes Resolver and Planner above the Engine.
- It does not call `time.Now()` ([Law 7](../../laws.md)). All
  timestamps come from the injected `clock.Clock`.

```
            cmd/resy-snipe (wiring)
              │     │      │
              ▼     ▼      ▼
        service  resolver  planner ──┐
              │     │      │         │
              │     ▼      ▼         │
              │   providers ◄────────┘
              │     │
              ▼     ▼
            store domain
                    │
                    ▼
                  clock
```

## Test plan

Tests live in `internal/resolver/*_test.go`. Single-rooted at the
public API ([Law 20](../../laws.md)).

### Fixture-driven adapter contract tests

`testdata/fixtures/venue_astoria-dc.json` — captured `/3/venue` body
for Astoria DC. `testdata/fixtures/venuesearch_astoria.json` —
captured `/3/venuesearch/search` body. The fake provider replays
these. Resolver tests assert on the resulting `domain.Venue`,
including `Release` parsing.

Refresh procedure: a `just refresh-resolver-fixtures` recipe re-runs
the captures against the live Resy API, using
`RESY_SNIPE_FIXTURE_REFRESH=1` to opt in. CI never refreshes; it
runs against the checked-in fixtures and fails on diff.

### URL parsing — property test

```go
func TestVenueQueryURL_RoundTrip(t *testing.T) {
    rapid.Check(t, func(t *rapid.T) {
        slug := rapid.StringMatching(`[a-z][a-z0-9-]{2,40}`).Draw(t, "slug")
        city := rapid.SampledFrom([]string{"new-york", "washington-dc",
            "los-angeles", "chicago"}).Draw(t, "city")
        u := "https://resy.com/cities/" + city + "/venues/" + slug + "?utm_source=x"
        gotSlug, gotCity, err := resolver.ParseURL(u)
        require.NoError(t, err)
        require.Equal(t, slug, gotSlug)
        require.Equal(t, city, gotCity)
    })
}
```

Plus negative cases (wrong host, missing `/venues/`, empty slug).

### Cache eviction — snapshot test

A `clock.NewFake` drives time across the TTL boundary. Sequence:
populate cache, advance fake clock to `TTL - 1s`, expect cache hit;
advance to `TTL + 1s`, expect upstream call. Stale-on-failure
exercised by injecting a fake provider that returns
`ErrUpstreamUnavailable` past the TTL — assert the stale entry is
returned and the warn log line is emitted (captured via
`slog.NewTextHandler` into a `bytes.Buffer`).

### Error classification

Table-driven test asserting that:

- `ResolveVenue` returning HTTP 404 → `ErrVenueNotFound` and the
  cache row (if any) is deleted.
- HTTP 429 with no cache → `ErrUpstreamUnavailable`, no cache write.
- HTTP 429 with stale cache → cached `Venue`, `ErrUpstreamUnavailable`
  *not* returned, warn log emitted.
- Body-shaped anti-bot challenge → `ErrAntiBotChallenge`.
- Malformed URL → `ErrVenueQueryMalformed`, no provider call (assert
  via fake provider's call count == 0).

### Concurrency

`-race` test that fires 32 concurrent `Resolve(slug, city)` calls
against a fake provider that sleeps 50ms and counts calls. Asserts
no data race; does *not* assert single-flight (we don't claim it).

## Cross-references

- [ADR-0001](../adr/0001-goal-driven-architecture.md) — defines
  `Goal.VenueQuery` and the Goal/Intent split that motivates a
  separate Resolver.
- [ADR-0002](../adr/0002-resolver-planner-engine-layering.md) — the
  three-layer decision; this doc is the contract for the first layer.
- [design/planner.md](planner.md) — consumes
  `(Goal, Venue) → Intent`; `Venue.Release` is the load-bearing field.
- [design/service-layer.md](service-layer.md) — composes Resolver and
  Planner, owns the name-disambiguation UI policy.
- [docs/architecture.md](../../architecture.md) — package layout this
  doc extends; Resolver slots in between `cmd/` and `providers`.
- [docs/laws.md](../../laws.md) — layering, time, error, and test
  rules cited above.
- [docs/anti-bot.md](../../anti-bot.md) — `ErrAntiBotChallenge`
  surface for `/3/venue`.
- [internal/domain/venue.go](../../../internal/domain/venue.go) —
  the `Venue` type this doc proposes extending.
- [internal/providers/provider.go](../../../internal/providers/provider.go)
  — the interface gaining `ResolveVenue`.
