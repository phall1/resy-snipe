# Design: "Anywhere, Anytime" Reservation Orchestration

**Date:** 2026-06-06  
**Approach:** C — Agent-first, breadth follows  
**Builds on:** v2 architecture (M1–M2 complete, M3 MCP in progress)

---

## 1. Goal

Transform resy-snipe from a Resy-only, one-shot CLI into a persistent,
multi-provider reservation daemon that accepts natural-language goals
from humans (CLI/TUI) and agents (MCP), hunts continuously for openings
including cancellations, and books the best available slot across any
supported platform.

---

## 2. Architecture Overview

Four new concepts sit on top of the existing v2 layer cake. Nothing below
`internal/service` changes.

```
   Humans (CLI/TUI)     Agents (MCP)
        │                    │
        ▼ HTTP               ▼ stdio/HTTP
   ┌─────────────────────────────────────┐
   │  daemon (resy-snipe serve)          │
   │   ┌─────────────────────────────┐   │
   │   │  Service                    │   │
   │   │   PlanQuest / CreateQuest   │   │
   │   │   CreateSubscription        │   │
   │   │   CancelSiblings            │   │
   │   │   SuggestVenues             │   │
   │   └─────────────┬───────────────┘   │
   │                 │ composes          │
   │   ┌─────────────▼───────────┐       │
   │   │  Scheduler              │       │
   │   │  (hot/cold queues)      │       │
   │   └─────────────┬───────────┘       │
   │                 │                   │
   │   ┌─────────────▼───────────┐       │
   │   │  Resolver → Planner     │       │
   │   │  (multi-provider)       │       │
   │   └─────────────┬───────────┘       │
   │                 │                   │
   │   ┌─────────────▼───────────┐       │
   │   │  Engine (v1, unchanged) │       │
   │   │  RunBookingRace         │       │
   │   └─────────────────────────┘       │
   └─────────────────────────────────────┘
```

**New components:**

| Component | Package | Purpose |
|-----------|---------|---------|
| `Subscription` | `internal/domain` | Persistent Goal with lifecycle |
| `Scheduler` | `internal/daemon` | Hot/cold polling queues |
| `ProviderSet` | `internal/service` | Multi-provider fan-out + cancellation |
| `PreferenceProfile` | `internal/domain` | Per-user observed defaults |
| `AgentCatalog` | `internal/mcp` | Three new MCP tools |

---

## 3. Persistent Subscriptions

### 3.1 Domain model

```go
type Subscription struct {
    ID            SubscriptionID
    UserID        UserID
    Goal          Goal
    Status        SubscriptionStatus  // Active | Paused | Fulfilled | Expired | Cancelled
    CreatedAt     time.Time
    ExpiresAt     *time.Time          // nil = until cancelled
    FulfilledBy   *QuestID
    Compromise    *CompromisePolicy   // optional widening rules
    PollInterval  time.Duration       // overridden by scheduler based on date proximity
}

type CompromisePolicy struct {
    TimeWindowMin  time.Duration  // e.g., 30m — accept ±30min from target
    TimeWindowMax  time.Duration  // e.g., 90m — on retry N, expand to ±90min
    PartySizeFlex  int            // e.g., 1 — accept party±1
    TableTypeAny   bool           // ignore table-type preference if true
}
```

### 3.2 Store schema (additive)

New tables alongside existing `quests`, `users`, etc.:

```sql
CREATE TABLE subscriptions (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id),
    goal_json     TEXT NOT NULL,          -- domain.Goal JSON
    status        TEXT NOT NULL,
    created_at    INTEGER NOT NULL,       -- Unix seconds
    expires_at    INTEGER,                -- NULL = no expiry
    fulfilled_by  TEXT REFERENCES quests(id),
    compromise_json TEXT,
    poll_interval INTEGER,                -- seconds, NULL = default
    next_poll_at  INTEGER NOT NULL        -- Unix seconds; scheduler wakes here
);

CREATE INDEX idx_subscriptions_user_status
    ON subscriptions(user_id, status);

CREATE INDEX idx_subscriptions_status_next_poll
    ON subscriptions(status, next_poll_at) WHERE status = 'Active';
```

### 3.3 Scheduler

A new goroutine in `internal/daemon/scheduler.go` maintains two queues:

- **Hot queue** — Subscriptions where `Goal.Date` is within 7 days.
  Default poll interval: 90 seconds.
- **Cold queue** — Subscriptions further out.
  Default poll interval: 5 minutes.

**Scheduling loop:**

1. Wake on `Clock.AfterFunc(nextPollAt)` or explicit notify channel.
2. SELECT subscriptions where `status = 'Active' AND next_poll_at <= now`,
   ordered by `next_poll_at`.
3. For each subscription:
   a. Call `service.PlanQuest(ctx, userID, goal)`.
   b. If Plan has viable Intents, submit them:
      - AA-M1 (Resy-only): call `service.CreateQuest` for the single Intent.
      - AA-M2+: call `service.RunMultiRace` with the full Plan.
   c. If any Quest reaches `Booked`, transition Subscription to `Fulfilled`,
      set `fulfilled_by`, emit notification, stop polling.
   d. If all Quests fail, compute next poll time (apply backoff up to a
      ceiling of 10 minutes), update `next_poll_at`, continue.
4. Reschedule self for the earliest `next_poll_at` in the active set.

**Compromise expansion:** On each retry, the Scheduler passes a
`retryCount` to the Planner. The Planner widens the `CompromisePolicy`
monotonically: first attempt uses `TimeWindowMin`, then expands toward
`TimeWindowMax` over the first 10 retries, then holds at max.

### 3.4 Graceful degradation

- If the daemon restarts, the Scheduler recovers by reading all `Active`
  subscriptions and recomputing `next_poll_at` from `now + poll_interval`.
- If `PlanQuest` returns `ErrNoSlots`, that's a normal outcome — log at
  `debug`, reschedule.
- If `PlanQuest` returns `ErrAuthExpired`, the Subscription pauses itself
  and emits a user-facing notification. It resumes when the user re-logs
  in via `resy-snipe login`.

---

## 4. Multi-Provider Search and Race

### 4.1 Resolver change

Add `ResolveAll` to `internal/resolver`:

```go
// internal/resolver/resolver.go
func (r *Resolver) ResolveAll(ctx context.Context, query VenueQuery) ([]domain.Venue, error)
```

`ResolveAll` queries every registered provider and returns one `Venue`
per provider that recognizes the query. A query like `"Carbone NYC"`
returns a Resy venue and an OpenTable venue; a query like
`"noma Copenhagen"` might return only a Resy venue today.

The `Venue` type already carries `ProviderID`; no change needed.
The `Resolver` is constructed with a `[]providers.Provider` (the
`ProviderSet`) and iterates over it, collecting non-error results.

### 4.2 Planner change

For a multi-provider Goal, `planner.Plan` emits a `Plan` containing
`[]Intent` instead of a single `Intent`:

```go
type Plan struct {
    Intents   []Intent
    Hash      string  // sha256(sorted(intent_hashes))
}
```

Each `Intent` targets one `(Venue, Provider)` pair. The Planner calls
the provider's `Calendar` to validate date availability before including
an Intent in the Plan. If a provider has no inventory for the date, that
provider is omitted from the Plan (not an error — just fewer options).

### 4.3 Service-layer race coordinator

`internal/service/multi_race.go` composes N independent engine races:

```go
func (s *Service) RunMultiRace(ctx context.Context, plan Plan) (*Quest, error)
```

**Algorithm:**

1. Submit each `Intent` to `engine.Submit` in parallel.
   Collect `[]QuestID`.
2. Subscribe to engine events for all QuestIDs via `engine.Subscribe`.
3. Wait for first `StatusBooked` event:
   a. Call `service.CancelQuest(ctx, siblingQuestID)` on all siblings.
      *(Note: quest cancellation is a prerequisite not yet wired in v2;
      it is implemented as part of AA-M1.)*
   b. Return the winning Quest.
4. If all quests reach terminal failure states (`Failed`, `Aborted`,
   `InventoryEmpty`), return `ErrNoSlotsAvailable`.
5. Timeout: if no winner within `RaceTimeout` (default 30s), cancel all
   and return `ErrRaceTimeout`.

**Critical invariant:** We race *entire* `RunBookingRace` pipelines per
provider, not individual ConfirmSlot calls. This preserves the existing
PrepareSlot serialization and avoids burning book_tokens on platforms
where we don't intend to complete the booking.

**Cancellation semantics:** When a sibling is cancelled, its in-flight
`Book` call is detached from the parent context (same as v1 graceful
shutdown). The winning Quest proceeds to completion; siblings may
receive `SlotTaken` or `BookTokenExpired` from their in-flight calls,
but the user only sees the winner.

### 4.4 Provider adapter: OpenTable

The second concrete provider lands as `internal/opentable/`, mirroring
`internal/resy/`:

- `client.go` — HTTP transport, header assembly, structured logging
- `auth.go` — session capture (OpenTable uses OAuth + session cookies)
- `resolve.go` — venue search by name/slug/URL
- `find.go` — availability search
- `book.go` — reservation hold + confirm
- `errors.go` — classifier mapping HTTP responses to `providers.Err{...}`

The `Provider` interface is the compile-time contract:
`var _ providers.Provider = (*opentable.Client)(nil)`.

The adapter is constructed in `internal/daemon/boot.go` alongside the
Resy adapter and injected into the `ProviderSet`.

---

## 5. Preference Profile

### 5.1 Model

Lightweight, no ML. Observed patterns stored as JSON in SQLite:

```go
type PreferenceProfile struct {
    UserID          UserID
    TimeRanges      map[time.Weekday][]TimeRange  // e.g., Friday: [{19:00,21:00}]
    DefaultPartySize int
    PreferredNeighborhoods []string
    PreferredCuisines      []string
    CompromiseDefaults     CompromisePolicy
    BookingHistory         []HistoricalBooking
}

type HistoricalBooking struct {
    VenueID   VenueID
    Date      time.Time
    Time      time.Time
    PartySize int
    Provider  ProviderID
}
```

### 5.2 Population

- **Explicit:** User sets preferences via CLI/MCP: `resy-snipe prefs set --friday 19:00-21:00 --party 2`.
- **Implicit:** Every successful booking appends a `HistoricalBooking`.
  A background goroutine (in Scheduler) recomputes `TimeRanges` from
  the last 20 bookings weekly.

### 5.3 Usage

The Planner reads `PreferenceProfile` when a Goal has fuzzy fields:

- Goal with no `TimeRange` → use `TimeRanges[Goal.Date.Weekday()]`.
- Goal with no `PartySize` → use `DefaultPartySize`.
- Goal with venue query `"somewhere good in the East Village"` →
  Resolver uses `PreferredNeighborhoods` to rank search results.

If no profile exists, the Planner falls back to v2 defaults (7pm, party of 2).

---

## 6. MCP Agent Catalog

Three new tools beyond the v2 M3 baseline. All use the existing Service
methods; no new transport logic.

### 6.1 `create_subscription`

Accepts a structured Goal (same schema as `plan_quest`; the MCP agent
translates natural-language user text into this structure), optional
`CompromisePolicy`, and optional `ExpiresAt`. Calls
`service.CreateSubscription(ctx, userID, goal, compromise, expiresAt)`.

Returns: `Subscription` with ID, status `Active`, and estimated first
poll time.

### 6.2 `list_subscriptions`

Parameters: `status` filter (Active | Paused | Fulfilled | Expired),
`date_range`, `limit`.

Returns: `[]Subscription` summary with fulfillment status and next poll.

### 6.3 `suggest_venues`

Parameters: `neighborhood`, `cuisine`, `date`, `party_size`, `time_range`.

Behavior:
1. Read `PreferenceProfile` for the user.
2. Merge explicit params with profile defaults.
3. Call `resolver.SearchVenues(ctx, query)` across all providers.
4. Rank results by: profile match score, availability on the target date,
   and historical success rate for that venue.
5. Return top N with availability preview.

This is the proactive "find me something good" interface.

---

## 7. Error Handling

### 7.1 Per-provider failures

A multi-provider Plan tolerates partial failure. If Resy returns
`ErrRateLimited` but OpenTable returns slots, the race continues with
OpenTable only. The user is notified of the degraded provider state
via the event stream.

### 7.2 Auth failures

If a provider's session expires mid-race, that provider's Quest fails
with `ErrAuthExpired`. The Subscription does NOT abort — it continues
polling with the remaining providers. A user notification prompts
re-login for the affected provider.

### 7.3 Anti-bot escalation

Each provider has its own signer seam (`resy/sign` pattern is duplicated
as `opentable/sign`). The Service constructs each provider's client with
its own `Signer` instance. Anti-bot challenges on one provider do not
block the other.

### 7.4 Scheduler crash recovery

If the Scheduler panics, it is restarted by the daemon's supervisor loop
with the same `[]Active Subscription` snapshot from the store. No in-flight
Quests are lost because they live in the engine, not the Scheduler.

---

## 8. Testing Strategy

| Component | Test type | Notes |
|-----------|-----------|-------|
| `Subscription` transitions | Unit | Same sealed-union pattern as `domain.Status` |
| `Scheduler` | Integration with fake clock | Hot/cold queue behavior, backoff, recovery |
| `MultiRace` | Property test | First win cancels siblings; all-failure returns error |
| `Planner` (multi-intent) | Unit | Compromise expansion over retry count |
| `OpenTable adapter` | httptest fixtures | Mirror `internal/resy/` test coverage |
| `PreferenceProfile` | Unit | Merge logic, fallback behavior |
| `MCP tools` | E2E against Service fake | Same pattern as v2 M3 tests |
| Full daemon | Integration | Scheduler + Service + Engine + fake providers |

All tests run under `-race`. The `gates` recipe gains one new check:
no `time.Now()` outside `internal/clock`.

---

## 9. Milestones (proposed)

| Milestone | Scope | v2 Milestone |
|-----------|-------|--------------|
| AA-M1 | Persistent Subscriptions + Scheduler on Resy only | Post-M2 |
| AA-M2 | OpenTable provider + multi-provider race | Post-M3 |
| AA-M3 | PreferenceProfile + `suggest_venues` | Post-M3 |
| AA-M4 | MCP `create_subscription` + `list_subscriptions` | Post-M3 |
| AA-M5 | Tock provider (third provider, validates abstraction) | Post-M4 |

---

## 10. Out of Scope

- **SaaS / hosted.** Stays self-hosted per ADR-0007.
- **Real-time push / WebSocket from providers.** We poll. If a provider
  offers a push API, an adapter can be added later without changing the
  Scheduler.
- **ML / neural recommendations.** Preferences are explicit + simple
  frequency counting.
- **Payment handling.** We book tables, not prepaid experiences.
- **Cross-user account sharing.** One user, one account per provider.
  The data model permits sharing later; not in this design.
