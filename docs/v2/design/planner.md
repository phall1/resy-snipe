# Planner

`internal/planner` is the layer that turns **what the user wants** into
**what we'll do**. It consumes a `domain.Goal` plus a resolved
`domain.Venue` and produces a `Plan` — a serializable, hash-pinned
artifact a human or agent can review — alongside the `domain.Intent`
the engine eventually consumes.

The split between Goal and Intent is mandated by
[ADR-0001](../adr/0001-goal-driven-architecture.md). The split between
Resolver, Planner, and Engine is mandated by
[ADR-0002](../adr/0002-resolver-planner-engine-layering.md). The Plan
artifact is mandated by
[ADR-0012](../adr/0012-plan-first-ux.md). This document specifies the
planner's contract: pure function shape, strategy-selection rules,
drop-moment math, slot expansion, replan semantics, hash
canonicalization, and test plan.

## Position in the stack

```
                    cmd/      mcp/
                      \       /
                       Service                ← composes the three layers
                          │
        ┌─────────────────┼─────────────────┐
        ▼                 ▼                 ▼
    Resolver           Planner            Engine
  (Goal → Venue)   (Goal,Venue → Plan)  (Intent → exec)
        │                 │                 │
        └─────────────────┴─────────────────┘
                          │
                          ▼
               providers.Provider
                  (Find, Calendar)
```

The planner imports `domain`, `providers`, `clock`. It does **not**
import `internal/resy`, `internal/store`, `internal/engine`, or
`internal/notify`. Calls into the network go exclusively through
`providers.Provider.Find` and `providers.Provider.Calendar`. See
[laws.md](../../laws.md) §Layering and §Interfaces for the dependency
rules this enforces.

## Purpose

```go
// Plan is the pure function from a Goal + resolved Venue + clock to a
// hash-pinned, user-approvable Plan. The single network seam is the
// providers.Provider passed via PlannerOpts; no store, no engine, no
// notifier. A planner.Plan call is replayable: given the same inputs
// it returns byte-identical output.
func (p *Planner) Plan(ctx context.Context, goal domain.Goal, v domain.Venue) (Plan, error)
```

The planner is a function, not a service. It has no in-memory state
that survives a call. It does not persist, log to user surfaces, or
emit events. Anything the user needs to know about the plan goes into
`Plan.Notes`; anything the system needs to know goes into the typed
fields. See §Anti-patterns at the bottom.

## Plan shape

The struct is fixed by [ADR-0012](../adr/0012-plan-first-ux.md). It is
reproduced here verbatim, with a field-by-field elaboration the ADR
intentionally left to this document.

```go
type Plan struct {
    Goal            Goal             // echoed back, normalized
    Venue           Venue            // resolved venue, including tz
    DropMoment      time.Time        // when we'll fire (UTC)
    Strategy        ReleaseStrategy  // Explicit/Discovered/Continuous
    FireSchedule    []time.Time      // each (slot,time) attempt the engine
                                     // will issue, expanded
    SigningRequired bool             // best-guess based on venue + drop
    Hash            string           // sha256(canonicalized Plan)
    Notes           []string         // human-readable caveats from planner
}
```

| Field             | Source                            | Notes                                                                                                                                        |
|-------------------|-----------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------|
| `Goal`            | echoed from input, normalized     | `TimePrefs` collapsed into ascending priority order; `Account` lower-cased; `Constraints.TableTypes` deduplicated.                           |
| `Venue`           | from Resolver                     | Carries `TZ`. The planner refuses a `Venue` with `TZ == nil` (`domain.ErrVenueMissingTZ`).                                                   |
| `DropMoment`      | computed                          | UTC. For `Continuous` this is `now` (we start polling immediately). For `Explicit` and `Discovered`, see §Drop-moment math.                  |
| `Strategy`        | chosen                            | One of the three `domain.ReleaseStrategy` variants. Selection rules in §Strategy selection.                                                  |
| `FireSchedule`    | expanded from `Goal.TimePrefs`    | The wall-clock attempts the engine will issue once `DropMoment` passes. Empty for `Continuous` (the engine fires on every Find hit).         |
| `SigningRequired` | derived from `Venue` + heuristics | Best-effort hint to the Service layer for warm-up, not a contract. False negatives degrade gracefully; the signer adapter is consulted live. |
| `Hash`            | computed last                     | Sha256 over canonicalized JSON minus `Hash` itself. See §Plan hash canonicalization.                                                         |
| `Notes`           | accumulated during planning       | Strings like `"strategy=Discovered because Resy hasn't observed a release for venue 49716 yet"`. The MCP tool and CLI surface them.          |

`Plan` is JSON-encodable (every field has a well-defined codec) and
human-printable: the CLI emits a table for `quest plan`, the MCP server
returns the struct directly to the agent.

## Inputs the planner observes

```go
type PlannerOpts struct {
    Provider providers.Provider     // for Find + Calendar
    Clock    clock.Clock            // for now()
    Cache    ObservedReleaseCache   // (venue, days_offset) → local time
    Logger   *slog.Logger           // dev-only, never user-facing
}
```

The `ObservedReleaseCache` interface is defined at the planner consumer
site ([laws.md](../../laws.md) §Interfaces, rule 5):

```go
type ObservedReleaseCache interface {
    Lookup(ctx context.Context, v domain.VenueRef, daysOffset int) (LocalReleaseTime, bool, error)
}

type LocalReleaseTime struct {
    Time       domain.WallTime  // venue-local
    DaysOffset int              // e.g. 30
    Confidence ObservedConfidence
    AsOf       time.Time
}
```

The Service layer adapts `*store.SQLiteStore` to this interface. The
planner does not import `internal/store`.

## Strategy selection

The selection algorithm is a three-way decision keyed on (a) whether
the target date is currently in Resy's release window for the venue,
(b) whether the planner has an observed-release-time for the venue,
and (c) the venue's configured release horizon (`release_days_ahead`,
`release_time_local`, `tz`).

```
                      ┌──────────────────────────────────┐
                      │ /4/find venue=V day=Goal.Date    │
                      │ party=Goal.Party                 │
                      └─────────────────┬────────────────┘
                                        │
              ┌─────────────────────────┼─────────────────────────┐
              ▼                         ▼                         ▼
       slots returned            book_on_date returned     no slots, no
       (date is open NOW)        (date opens later)        book_on_date
              │                         │                         │
              ▼                         ▼                         ▼
        Continuous          observed_release_times          Discovered
        Drop=now            cache lookup?                   (fail-safe;
                            ┌───────┴────────┐              poll the
                            ▼                ▼              calendar)
                          hit             miss
                            │                │
                            ▼                ▼
                        Explicit         Discovered
                        Drop=computed    Drop=guess from
                                         release_days_ahead
                                         midnight venue-local
```

### Rules

1. **Goal.Date is in window now** (`/4/find` returned slots, or
   `book_on_date <= now`) → `domain.ContinuousRelease`.
   `DropMoment = now`. The engine starts polling Find at `PollFloor`
   immediately; this is the cancellation-chase mode.
   *Note:* `"strategy=Continuous because Resy is already serving slots
   for {date}; we'll race cancellations until {until}"`.

2. **Goal.Date will be in window later AND we have a cached
   observed-release-time** (`ObservedReleaseCache.Lookup` hits with
   confidence ≥ `ConfidenceObserved`) → `domain.ExplicitRelease`.
   `DropMoment = ComputeDrop(Goal.Date, daysOffset, observedLocal,
   Venue.TZ)`. See §Drop-moment math.
   *Note:* `"strategy=Explicit at {drop} (observed {observedLocal} on
   {asOf})"`.

3. **Goal.Date will be in window later AND no observed-release cache
   hit** → `domain.DiscoveredRelease`. The planner uses the venue's
   advertised `release_days_ahead` (from `/4/find`'s `book_on_date`
   field) to bracket a probe window:
   - `ProbeFrom = bookOnDate − probeLead` (default `probeLead = 90s`).
   - `ProbeUntil = ProbeFrom + probeWidth` (default `probeWidth = 5m`).
   `DropMoment = bookOnDate` (best guess; engine will record the actual
   observed instant when it hits).
   *Note:* `"strategy=Discovered because Resy hasn't observed a release
   for venue {ref} yet; probing {probeFrom}..{probeUntil}"`.

`probeLead` and `probeWidth` are configurable per `PlannerOpts` and
default to values empirically validated against the v1
`defaultRetryWindow` (30m). The narrower defaults reflect that
`book_on_date` is a tight prior; the v1 default existed because v1
had no prior at all.

### Edge cases the algorithm handles

| Case                                                                               | Outcome                                                                                                                                                  |
|------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Goal.Date` is in the past                                                         | Return `ErrGoalDateInPast` from `Plan`. The Service layer surfaces this; no Plan is built.                                                               |
| `Goal.Date == today` and `/4/find` returns no slots and no `book_on_date`          | Continuous with `Until = end-of-venue-day`. Note explains why.                                                                                           |
| `/4/find` returns `book_on_date` *and* a non-empty slot list                       | Resy sometimes serves both during the moment of a release. Treat as Continuous (date is already open).                                                   |
| Provider returns `ErrAntiBotChallenge` during planning                             | Surface as `ErrPlannerProviderUnavailable`. Planner does not retry. Service decides.                                                                     |
| Cached observed-release-time has `Confidence == ConfidenceWeak` (single past hit)  | Strategy = `Explicit` with `DropMoment − jitter` and a Note advising re-discovery. Configurable: `PlannerOpts.WeakObservedFallback = StrategyDiscovered` flips to Discovered. |
| Venue has no `tz` populated                                                        | Planner returns `domain.ErrVenueMissingTZ`. Resolver is responsible for filling it; planner refuses to fabricate.                                        |

## Drop-moment math

`ComputeDrop` converts (target reservation date, days-offset,
local-time-of-day, venue tz) into a `time.Time` in UTC.

```
ComputeDrop(targetDate, releaseDaysAhead, releaseTimeLocal, tz):
    # 1. Anchor the release date to the venue's calendar, not ours.
    anchorDate = targetDate.In(tz).AddDate(0, 0, -releaseDaysAhead)

    # 2. Compose a wall-clock instant at the venue's local time-of-day.
    drop = time.Date(
        anchorDate.Year(), anchorDate.Month(), anchorDate.Day(),
        releaseTimeLocal.Hour, releaseTimeLocal.Minute, releaseTimeLocal.Second,
        0,                  # nanos
        tz,                 # location
    )

    # 3. Project to UTC; the engine's wake-up uses UTC throughout.
    return drop.UTC()
```

### Worked example

| Input                  | Value                            |
|------------------------|----------------------------------|
| `targetDate`           | `2026-06-09`                     |
| `releaseDaysAhead`     | `30`                             |
| `releaseTimeLocal`     | `00:00:00`                       |
| `tz`                   | `America/New_York` (EDT, UTC-4)  |
| → `anchorDate (in tz)` | `2026-05-10`                     |
| → `drop (in tz)`       | `2026-05-10T00:00:00 EDT`        |
| → `drop.UTC()`         | `2026-05-10T04:00:00Z`           |

The math is intentionally trivial. The non-obvious correctness
condition is *anchoring in the venue's location first*: if the operator
is in `America/Los_Angeles` and the venue is in `America/New_York`,
subtracting 30 days from `targetDate.In(operatorLocal)` is wrong by
hours when the date crosses a calendar boundary. The Resolver
guarantees `Venue.TZ` is non-nil; the planner uses it unconditionally.

DST is a non-issue because we anchor to the venue-local *date* and
construct the wall-clock instant inside `tz` — Go's stdlib
`time.Date` resolves the zone at construction.

## Slot expansion

`Goal.TimePrefs` is a `domain.TimeWindow`:

```go
type TimeWindow struct {
    Earliest WallTime         // 18:30
    Latest   WallTime         // 21:00
    Step     time.Duration    // 15m
    Priority TimePriority     // CenterOut | Earliest | Latest | List
    List     []WallTime       // explicit list when Priority == List
    Tables   []string         // ordered table-type prefs (empty => any)
}
```

The planner expands this into the `Intent.SlotPrefs` slice the engine
walks during the booking race. The expansion is deterministic and
reversible.

```
Expand(window):
    times = []
    if window.Priority == List:
        times = window.List
    else:
        for t := window.Earliest; t <= window.Latest; t += window.Step:
            times.append(t)
        sort(times, by=window.Priority)

    prefs = []
    for t in times:
        if window.Tables is empty:
            prefs.append(SlotPreference{Time: t, TableType: ""})
        else:
            for tbl in window.Tables:
                prefs.append(SlotPreference{Time: t, TableType: tbl})
    return prefs
```

`Priority` orderings:

| Variant      | Meaning                                                                                                  |
|--------------|----------------------------------------------------------------------------------------------------------|
| `CenterOut`  | Center of the window first, then alternating outward. `[19:00, 19:15, 18:45, 19:30, 18:30, ...]`.        |
| `Earliest`   | Ascending from `Earliest`.                                                                               |
| `Latest`     | Descending from `Latest`.                                                                                |
| `List`       | The explicit list, in user-supplied order.                                                               |

`FireSchedule` mirrors `Intent.SlotPrefs` projected onto wall-clock
moments. For `ExplicitRelease` and `DiscoveredRelease`, the schedule is
all attempts queued at `DropMoment` (the engine's race fans them out
according to `BookingPolicy`); for `ContinuousRelease` the schedule is
empty (every `/4/find` hit may produce a different attempt set).

The engine's race ordering invariant is preserved: walk
`Intent.SlotPrefs` head-to-tail, fetch `/3/details` per
`BookingPolicy.DetailsSerial`, then race `/3/book`. The planner's only
contribution is the *order*.

## Replan contract

A `Plan` produced at time `t1` can be re-derived at time `t2 > t1` from
the same `Goal` and a fresh `Venue` snapshot, producing a possibly
different `DropMoment`. The daemon's quest scheduler invokes the
planner periodically — every `replanTick` (default `5m` for active
quests, `1h` for distant quests) — to detect drift.

A replan is **applied** when:

1. The new Plan's `Hash` differs from the current Plan's `Hash`, AND
2. The new `DropMoment` is at least `replanThreshold` (default `30s`)
   different from the current `DropMoment`, OR the `Strategy` variant
   has changed.

The threshold prevents thrash from clock-skew or millisecond Resy
config wobble. A replan that crosses thresholds is logged as
`Event{Kind: EventReplanned, OldHash, NewHash, Reason}` (the audit log
is owned by the Service layer; planner returns the new Plan).

### Worked example

```
t1: 2026-05-08T15:00Z  Goal{date=2026-06-09, party=2, venue=astoria-dc}
    Resolver returns Venue{ref=49716, tz=America/New_York,
                            release_days_ahead=30,
                            release_time_local=00:00}
    Planner observes /4/find returns book_on_date=2026-05-10T04:00Z
    Cache miss → strategy=Discovered
    DropMoment=2026-05-10T04:00Z
    ProbeFrom=2026-05-10T03:58:30Z, ProbeUntil=2026-05-10T04:03:30Z
    Hash=A

t2: 2026-05-09T18:00Z  (replanTick fires)
    Resolver venue cache still valid (TTL 24h)
    Planner re-runs /4/find
    Resy now reports book_on_date=2026-05-10T05:00Z
      (operator changed release window from 00:00 ET to 01:00 ET)
    Strategy still Discovered
    DropMoment=2026-05-10T05:00Z   ← differs by 1h
    Hash=B
    Service: B != A AND |Δ| > 30s → apply replan, emit EventReplanned
```

A replan that arrives *during* engine execution (e.g. between
`Discovering` and `Awaiting`) cancels the in-flight engine run via the
existing context, then schedules a fresh run from the new Plan. The
engine never mutates a Plan in flight; replan is always a fresh start.

## Plan hash canonicalization

`Plan.Hash` is a content-addressable id over the Plan's typed fields.
Stable across SDK versions; stable across map iteration order; stable
across float/int width. Inspired by RFC 8785 (JSON Canonicalization
Scheme).

### Algorithm

1. Marshal the Plan to a JSON object.
2. Drop volatile fields:
   - `Hash` (we are computing it).
   - `Notes` (human-readable; not part of the contract).
   - Any field flagged `planner:"volatile"` (currently none).
3. Sort all object keys lexicographically (UTF-8 byte order), recursively.
4. Normalize numbers:
   - Times → RFC 3339 nanos in UTC.
   - Durations → integer nanoseconds.
   - Floats are not used; if added, they round-trip via
     `strconv.FormatFloat(f, 'g', -1, 64)`.
5. Strings → UTF-8, no escape variation.
6. Prepend a fixed envelope:
   ```
   {"plan_hash_version":1,"payload":<canonicalized>}
   ```
7. `Hash = "sha256:" + hex(sha256(envelope))`.

The `plan_hash_version` is bumped whenever the canonicalization rules
change. A daemon that loads a Plan with an older version recomputes
the hash before comparing; mismatch triggers a forced replan. See
[ADR-0012](../adr/0012-plan-first-ux.md) §Notes.

### What is *in* the hash

```
plan_hash_version
goal.account
goal.constraints.*  (sorted)
goal.date
goal.party
goal.time_prefs.*
goal.venue_query
venue.provider
venue.ref
venue.tz                 (IANA name, e.g. "America/New_York")
strategy.tag             ("explicit" | "discovered" | "continuous")
strategy.fields.*        (At, ProbeFrom/ProbeUntil, Until)
drop_moment              (UTC RFC 3339 nanos)
fire_schedule            (UTC RFC 3339 nanos, in order)
signing_required
```

### What is *not* in the hash

```
notes
hash
provider response cache metadata
operator clock observations (now, planner.AsOf)
```

## Confidence and Notes

`Plan.Notes` is the user-visible commentary surface. The planner emits
a Note for every non-trivial decision: strategy choice, fallback to
Discovered, weak observed-release confidence, narrow probe windows,
DST-adjacent dates, etc. Tools render Notes verbatim:

- CLI: appended to the Plan table under a `Notes:` heading.
- MCP: returned as a `notes: []string` field in the tool result.
- Daemon logs: emitted at `INFO` keyed under `domain.LogKeyQuestID`.

A Note has no machine-readable structure on purpose. If a consumer
wants typed signals (e.g. "did we use the cache?"), it goes into a
typed field, not a Note.

## Errors

```go
var (
    ErrGoalDateInPast            = errors.New("planner: goal date is in the past")
    ErrGoalPartyTooLarge         = errors.New("planner: party exceeds venue capacity")
    ErrPlannerProviderUnavailable = errors.New("planner: provider unavailable")
    ErrPlannerNoDropWindow       = errors.New("planner: no drop window can be derived")
)
```

`ErrPlannerNoDropWindow` is the failure mode flagged in
[ADR-0001](../adr/0001-goal-driven-architecture.md) §Negative — it
fires when none of the three strategies can be applied (e.g. the
target date is before the venue's earliest releasable horizon AND no
cached observation exists AND `/4/find` returns neither slots nor
`book_on_date`).

Errors cross the package boundary as sentinels per
[laws.md](../../laws.md) §Errors. Planner never returns a bare
`fmt.Errorf` without `%w`-wrapping a sentinel.

## Dependency rules

| Imports                    | Why                                                         |
|----------------------------|-------------------------------------------------------------|
| `internal/domain`          | Goal, Venue, Intent, ReleaseStrategy, WallTime              |
| `internal/providers`       | Provider seam for Find + Calendar                           |
| `internal/clock`           | Now (rule 7 of laws.md — never `time.Now()`)                |
| stdlib                     | `crypto/sha256`, `encoding/json`, `sort`, `time`, `errors`  |

| Does **not** import       | Why                                                                                  |
|---------------------------|--------------------------------------------------------------------------------------|
| `internal/resy`           | Layering rule 1 — concrete provider lives below the seam.                            |
| `internal/store`          | Persistence is the Service layer's job. Cache access is via injected interface.      |
| `internal/engine`         | Engine consumes the Plan; never the other way around.                                |
| `internal/notify`         | Planner returns Notes; Service layer turns them into notifications.                  |
| `internal/resolver`       | Planner consumes a *resolved* `Venue`. The Service layer composes Resolver + Planner. |

The package's `doc.go` should restate these rules in 2 paragraphs per
[laws.md](../../laws.md) rule 23.

## Test plan

### Unit: strategy selection (table-driven)

```
matrix:
  release_window_state ∈ {open, closed_with_book_on_date, closed_no_data}
  observed_release_cache ∈ {miss, hit_strong, hit_weak}
  goal_date_position ∈ {past, today, in_window, beyond_window}

for each cell:
  given a fake Provider returning the matching /4/find shape
  given a fake Cache returning the matching observation
  given a fake Clock at a known instant
  when planner.Plan(goal, venue) runs
  expect strategy ∈ {Explicit, Discovered, Continuous, error}
  expect Notes contains the specific reason string
  expect DropMoment matches the worked formula
```

The matrix is `3 × 3 × 4 = 36` cells; not all are reachable
(`past` short-circuits to `ErrGoalDateInPast` regardless of the other
axes). The expected outcomes are codified once in a `strategyMatrix`
golden file.

### Property: hash stability

```
for any (Goal, Venue) sampled from a generator:
    p1 := planner.Plan(goal, venue)
    p2 := planner.Plan(goal, venue)            # second call, fresh planner
    require.Equal(p1.Hash, p2.Hash)

for any Plan p:
    bytes_a := canonicalize(p)
    bytes_b := canonicalize(shuffle_map_keys(p))
    require.Equal(bytes_a, bytes_b)
    require.Equal(p.Hash, sha256_hex(bytes_a))
```

The shuffle is achieved by re-marshaling through a `map[string]any`
with intentionally randomized iteration order. A regression here means
canonicalization is buggy.

### Integration: Plan → Intent → engine.runStrategy

```
given a fixture provider with a known /4/find shape
given an in-memory store (real, not mock) with a fake clock
when service.PlanQuest(goal) runs and returns Plan P
and  service.CreateQuest(goal, planHash=P.Hash) runs
then a SnipeState is constructed from P.Goal + P.Strategy
and  engine.runStrategy(ctx, state) executes the right code path
and  the resulting transitions match the expected golden trace
```

This test exercises the full layer composition: Resolver (with a
fixture venue), Planner (real, with the in-memory cache), Engine (the
real one). It is the closest-to-end-to-end test that does not require
the daemon HTTP layer; the daemon-level tests live in
[design/service-layer.md](service-layer.md).

### Replan: hash change triggers re-application

```
t=0  plan A; service stores A.Hash with the Quest
t=Δ  /4/find now returns a different book_on_date
     replan tick fires; planner produces B; B.Hash != A.Hash
     service emits EventReplanned, updates the Quest's Plan
     engine context is canceled and re-spawned from B
expect:
  - exactly one EventReplanned per tick where the hash changes
  - no EventReplanned when only Notes differ (Notes are not in hash)
  - no EventReplanned when |ΔdropMoment| < replanThreshold
```

## Anti-patterns

The planner does **not**:

- Call `/3/book`, `/3/details`, or anything that mutates Resy state.
  Booking is the engine's job. The planner only reads (`/4/find`,
  `/3/calendar`).
- Persist anything. The Service layer owns persistence. The planner
  takes a `Cache` interface; the writes back to that cache happen in
  the engine when it observes a release (per
  `engine.recordObserved`).
- Log to user surfaces. Notes are the user-facing channel; structured
  logs go to the dev logger via `slog`.
- Consult the audit log. Replan decisions are based on (Goal, Venue,
  cache, clock) only — the audit log is a downstream consumer.
- Mutate the input Goal. The Goal echoed in `Plan.Goal` is a
  normalized copy; the input is not modified.
- Embed Resy-specific knowledge. `release_days_ahead` and
  `release_time_local` are surfaced through the resolved `Venue` and
  through `/4/find`'s `book_on_date`; the planner treats them as
  generic provider signals. An OpenTable planner would consume the
  same shape.
- Sleep, wait, or block on the clock. `Plan` returns immediately after
  the synchronous `/4/find` call. Any waiting belongs to the engine.
- Choose a strategy by string-matching error bodies. Strategy
  selection is keyed on typed fields (`book_on_date`, slot list
  emptiness, cache presence) and sentinel errors per
  [laws.md](../../laws.md) §Errors.

## Cross-references

- [ADR-0001 — Goal vs Intent](../adr/0001-goal-driven-architecture.md)
- [ADR-0002 — Resolver/Planner/Engine layering](../adr/0002-resolver-planner-engine-layering.md)
- [ADR-0012 — Plan as approvable artifact](../adr/0012-plan-first-ux.md)
- [design/resolver.md](resolver.md) — where the `Venue` comes from
- [design/service-layer.md](service-layer.md) — composition of Resolver + Planner + Engine
- [design/multi-user.md](multi-user.md) — `Goal.Account`, per-account rate limits
- [docs/release-strategies.md](../../release-strategies.md) — engine-level details on each strategy
- [internal/domain/release.go](../../../internal/domain/release.go) — the union the planner returns
- [internal/domain/intent.go](../../../internal/domain/intent.go) — `Intent.Hash` for idempotency
- [internal/engine/release.go](../../../internal/engine/release.go) — what consumes the chosen strategy
