# Service layer

**Layer**: `internal/service` (new in M1)
**Status**: Design — implementation lands in M1
**Related ADRs**: [0003](../adr/0003-daemon-first-cli-as-client.md),
[0004](../adr/0004-mcp-as-peer-front-end.md),
[0005](../adr/0005-multi-user-data-model-from-day-one.md),
[0010](../adr/0010-one-daemon-many-users.md),
[0011](../adr/0011-operator-issued-invites-no-self-registration.md),
[0012](../adr/0012-plan-first-ux.md)
**Related design**: [overview.md](overview.md),
[resolver.md](resolver.md), [planner.md](planner.md),
[daemon.md](daemon.md), [mcp.md](mcp.md),
[multi-user.md](multi-user.md)

## Purpose

The Service layer is the in-process Go interface that both transports
consume. The "API of the system" lives here as a Go type, not a
swagger doc. Adding a verb adds one method; HTTP and MCP both pick it
up.

```
   CLI ─► HTTP ─┐
                ├─► Service ─► Resolver / Planner / Engine / Store
   MCP ────────┘
```

Per [ADR-0004](../adr/0004-mcp-as-peer-front-end.md), MCP is a peer of
HTTP, not a wrapper. Both transports speak `service.Service` in
process. Neither speaks to the other. The Service is the single
source of truth for system verbs; transports differ only in how they
authenticate, frame, and surface results.

The Service does not know about HTTP, JSON-RPC, MCP, status codes, or
SSE. It knows about `domain.Quest`, `domain.Plan`, `UserID`. It
composes the layers below it (Resolver, Planner, Engine, Store,
Secrets, Notifier) and enforces tenancy. That is its entire job.

## The interface

```go
// Package service is the transport-agnostic API of the system. Both
// the HTTP daemon (M2) and the MCP server (M3) consume this Go
// interface; neither package depends on the other.
package service

type Service interface {
    // ResolveVenue takes a free-form query (URL, slug, name) and
    // returns the canonical Venue. Pure — no persistence.
    ResolveVenue(ctx context.Context, userID UserID, query string) (domain.Venue, error)

    // PlanQuest is a pure function: (Goal, Venue) → Plan. No side
    // effects. The returned Plan carries a stable content hash that
    // CreateQuest will recompute and verify.
    PlanQuest(ctx context.Context, userID UserID, goal domain.Goal) (domain.Plan, error)

    // CreateQuest persists a quest and schedules it. If planHash is
    // non-nil, the daemon recomputes the Plan and refuses if the hash
    // doesn't match (prevents TOCTOU between plan-and-commit). See
    // ADR-0012.
    CreateQuest(ctx context.Context, userID UserID, goal domain.Goal, planHash *string, opts CreateOpts) (domain.Quest, error)

    // GetQuest returns the current state + most recent N events.
    // Refuses if questID is owned by a different user.
    GetQuest(ctx context.Context, userID UserID, questID domain.QuestID) (QuestState, error)

    // ListQuests returns lightweight summaries for this user, filtered
    // by status / venue / date range.
    ListQuests(ctx context.Context, userID UserID, filter QuestFilter) ([]QuestSummary, error)

    // CancelQuest signals the engine to abort. Idempotent: cancelling
    // an already-terminal quest is not an error.
    CancelQuest(ctx context.Context, userID UserID, questID domain.QuestID, opts CancelOpts) error

    // SubscribeQuest blocks until ctx is cancelled, invoking cb once
    // per event. The transport adapts: HTTP wraps it as SSE; MCP
    // wraps it as notifications/. See §Streaming.
    SubscribeQuest(ctx context.Context, userID UserID, questID domain.QuestID, cb func(domain.Event)) error

    // Login captures Resy credentials and persists a session. This is
    // interactive (password) and operator-only — not exposed via MCP.
    // Returns the AccountID the credentials were stored under.
    Login(ctx context.Context, userID UserID, accountEmail, password string) (domain.AccountID, error)

    // ListAccounts returns this user's bound Resy accounts (no
    // secrets — id, email, last_used_at).
    ListAccounts(ctx context.Context, userID UserID) ([]domain.Account, error)

    // Auth admin — operator-only. Transport binds these to the
    // operator's UserID; tenants get ErrUnauthorized.
    InviteUser(ctx context.Context, userID UserID, email string) (Invite, error)
    AcceptInvite(ctx context.Context, token string, email, password string) (UserID, error)
    RotateToken(ctx context.Context, userID UserID, target UserID) (Token, error)
    RevokeToken(ctx context.Context, userID UserID, target UserID) error
    ListUsers(ctx context.Context, userID UserID) ([]UserSummary, error)
}
```

Eleven domain verbs + five auth-admin verbs = sixteen methods. Both
transports cover all sixteen; MCP curates which it surfaces as tools
([design/mcp.md](mcp.md) §exposed-tools). The full set is on the
HTTP API ([design/daemon.md](daemon.md) §routes).

## Type contract

Per [ADR-0004 Notes](../adr/0004-mcp-as-peer-front-end.md#notes), every
parameter and return type in `Service` is a **plain serializable
type**. Concretely:

- No `chan T`, no `<-chan T`. Streaming is the callback in
  `SubscribeQuest`.
- No `func() T` except the streaming callback (which is data flowing
  out, not a continuation).
- No private fields on returned structs. Everything is exported and
  JSON-tagged.
- No `interface{}`-typed payloads. `domain.SlotPayload` is a sealed
  union; transports pattern-match on the union, never `json.RawMessage`
  (per [I-9](../../invariants.md)).
- No `time.Time` aliased into a domain-specific type at the Service
  boundary — `time.Time` is RFC3339 in JSON and `chrono.DateTime` in
  Python clients. Same on the wire either way.

These rules mean the Service surface can be auto-described for MCP
tool definitions (via reflection) and serialized for HTTP without
custom marshallers.

## Identity scoping

Every method takes `userID UserID` as the second parameter, after
`ctx`. This is non-negotiable per
[ADR-0005](../adr/0005-multi-user-data-model-from-day-one.md) and
[ADR-0010](../adr/0010-one-daemon-many-users.md).

```go
type UserID string  // homelab-tenant id, not Resy account id
```

The transport authenticates the request (HTTP: bearer token; MCP:
session token) and binds `userID` from the token. The Service trusts
that `userID` and enforces ownership at every step:

```go
func (s *service) GetQuest(ctx context.Context, userID UserID, questID domain.QuestID) (QuestState, error) {
    s.audit(ctx, userID, "get_quest", questID, nil)
    q, err := s.store.LoadQuest(ctx, questID)
    if errors.Is(err, store.ErrNotFound) {
        return QuestState{}, ErrNotFound
    }
    if err != nil {
        return QuestState{}, err
    }
    if q.UserID != userID {
        // Tenancy violation: quest exists, but not yours. Surface as
        // NotFound to avoid leaking existence to other tenants.
        return QuestState{}, ErrNotFound
    }
    ...
}
```

The "exists-but-not-yours → NotFound" pattern is deliberate: returning
`ErrUnauthorized` for a wrong-owner read would let one tenant probe
the existence of another tenant's quest IDs. Fold both cases into
NotFound. The audit log distinguishes them (see §Audit).

The `tenant_check.go` test
([ADR-0010 Notes](../adr/0010-one-daemon-many-users.md#notes)) walks
every Service method, calls it with one user's token but another
user's `questID` / `accountID` argument, and asserts the result is
`ErrNotFound`. New methods that don't satisfy the check don't pass
review.

## Idempotency

`CreateQuest` and `CancelQuest` accept an optional idempotency key:

```go
type CreateOpts struct {
    IdempotencyKey string  // empty = no idempotency
}
type CancelOpts struct {
    IdempotencyKey string
}
```

When non-empty, the Service does an upsert against
`idempotency_keys (user_id, key, action, target_id, response_blob,
expires_at)`. A repeated call with the same `(user_id, key, action)`
returns the cached response. Different action under the same key is
`ErrConflict`.

Retention: 24h. The transport documents this in its public surface
([design/daemon.md](daemon.md) §idempotency,
[design/mcp.md](mcp.md) §idempotency). The Service just enforces it.

`PlanQuest` and read methods are idempotent by construction — no key
needed.

## Streaming

`SubscribeQuest` is the one method that does not return a flat value.
Its contract:

```go
// SubscribeQuest blocks until ctx is cancelled or the quest reaches
// a terminal status. cb is invoked synchronously, in event order, on
// the calling goroutine. cb MUST NOT block — the transport is
// responsible for buffering / framing.
SubscribeQuest(ctx context.Context, userID UserID, questID domain.QuestID, cb func(domain.Event)) error
```

The implementation wraps `engine.Subscribe`
([internal/engine/subscribe.go](../../../internal/engine/subscribe.go))
and filters by `questID + userID`:

```go
func (s *service) SubscribeQuest(ctx context.Context, userID UserID, questID domain.QuestID, cb func(domain.Event)) error {
    if err := s.assertOwner(ctx, userID, questID); err != nil {
        return err
    }

    // Replay: deliver events that already happened so a late subscriber
    // sees the full history. The store query is bounded by questID; we
    // know it's owned because of assertOwner above.
    history, err := s.store.LoadEvents(ctx, questID)
    if err != nil {
        return err
    }
    for _, ev := range history {
        cb(ev)
    }

    // Live: register with engine, filter, deliver.
    cancel := s.engine.Subscribe(func(n engine.Notification) {
        if domain.QuestID(n.SnipeID) != questID {
            return
        }
        cb(n.Event)
    })
    defer cancel()

    <-ctx.Done()
    return ctx.Err()
}
```

Both transports adapt this pattern:

- **HTTP-SSE** ([design/daemon.md](daemon.md) §events): the handler
  passes a callback that writes `event: …\ndata: …\n\n` frames to the
  response body. When the client disconnects, the request context
  cancels, `SubscribeQuest` returns, the goroutine exits.
- **MCP-streaming** ([design/mcp.md](mcp.md) §notifications): the tool
  handler passes a callback that calls `mcp.SendNotification(...)`.
  When the MCP session closes, ctx cancels, same path.

Per [ADR-0004](../adr/0004-mcp-as-peer-front-end.md), the same
internal pubsub feeds both. There is no second event path for MCP.

## Error model

Service errors are sentinels. Every error returned from a Service
method either is one of the table below or wraps one (use
`errors.Is`).

| Sentinel             | Meaning                                            | HTTP code | MCP error code              |
|----------------------|----------------------------------------------------|-----------|-----------------------------|
| `ErrNotFound`        | Resource does not exist (or not owned by caller)   | 404       | `quest_not_found`           |
| `ErrUnauthorized`    | Token invalid / missing / not operator             | 401       | `unauthorized`              |
| `ErrForbidden`       | Authenticated but action not allowed for this user | 403       | `forbidden`                 |
| `ErrConflict`        | Idempotency-key reuse with different action        | 409       | `conflict`                  |
| `ErrInvalidPlanHash` | `CreateQuest` plan hash didn't match               | 412       | `plan_hash_mismatch`        |
| `ErrVenueNotFound`   | Resolver couldn't identify venue                   | 422       | `venue_not_found`           |
| `ErrInvalidGoal`     | Goal failed validation (date in past, etc.)        | 422       | `invalid_goal`              |
| `ErrAccountLocked`   | Resy account hit rate limit / 2FA                  | 423       | `account_locked`            |
| `ErrInternal`        | Unclassified — bug                                 | 500       | `internal`                  |

The transport packages own the translation table, but the Service
defines the sentinel set. New sentinels go in
`internal/service/errors.go` and ship with both transports updating
their tables in the same PR.

```go
// errors.go
var (
    ErrNotFound        = errors.New("service: not found")
    ErrUnauthorized    = errors.New("service: unauthorized")
    ErrForbidden       = errors.New("service: forbidden")
    ErrConflict        = errors.New("service: conflict")
    ErrInvalidPlanHash = errors.New("service: plan hash mismatch")
    ErrVenueNotFound   = errors.New("service: venue not found")
    ErrInvalidGoal     = errors.New("service: invalid goal")
    ErrAccountLocked   = errors.New("service: account locked")
    ErrInternal        = errors.New("service: internal")
)
```

Transports never return a bare `error` to the wire. They map sentinels
to their wire shape; an unmapped error becomes `ErrInternal` and
logs a stack. This is the same pattern
[`internal/providers/errors.go`](../../../internal/providers/errors.go)
uses one layer down.

## Implementation sketch

```go
type service struct {
    resolver  Resolver       // venue lookup
    planner   Planner        // (Goal, Venue) → Plan
    engine    *engine.Engine // submit, load, subscribe
    store     Store          // tenancy-scoped persistence
    secrets   Secrets        // session capture / unlock
    notifier  Notifier       // out-of-band channel (sms/imessage/...)
    clock     clock.Clock    // injected — never time.Now() (Law 7)
    audit     AuditLogger    // every call writes one row
    log       *slog.Logger   // injected, never the global default
}

func New(deps Deps) Service {
    // Compile-time interface check (Law 6).
    var _ Service = (*service)(nil)
    return &service{...}
}
```

Each method is composition + auth + audit. No business logic. The
Resolver decides which venue; the Planner decides what fires when;
the Engine decides how to retry; the Store decides what to persist.
Service decides only "is this caller allowed to do this, and which
component handles it?"

Sketch of `CreateQuest`:

```go
func (s *service) CreateQuest(
    ctx context.Context,
    userID UserID,
    goal domain.Goal,
    planHash *string,
    opts CreateOpts,
) (domain.Quest, error) {
    if cached, ok, err := s.idem.Lookup(ctx, userID, opts.IdempotencyKey, "create_quest"); err != nil {
        return domain.Quest{}, err
    } else if ok {
        return cached.(domain.Quest), nil
    }

    venue, err := s.resolver.Resolve(ctx, goal.VenueQuery)
    if err != nil {
        s.audit.Fail(ctx, userID, "create_quest", "", err)
        return domain.Quest{}, mapResolverErr(err)
    }

    plan, err := s.planner.Plan(ctx, goal, venue)
    if err != nil {
        s.audit.Fail(ctx, userID, "create_quest", "", err)
        return domain.Quest{}, mapPlannerErr(err)
    }

    if planHash != nil && plan.Hash != *planHash {
        s.audit.Fail(ctx, userID, "create_quest", "", ErrInvalidPlanHash)
        return domain.Quest{}, ErrInvalidPlanHash
    }

    quest, err := s.engine.Submit(ctx, userID, plan.AsIntent())
    if err != nil {
        s.audit.Fail(ctx, userID, "create_quest", "", err)
        return domain.Quest{}, mapEngineErr(err)
    }

    s.idem.Record(ctx, userID, opts.IdempotencyKey, "create_quest", quest.ID, quest)
    s.audit.OK(ctx, userID, "create_quest", string(quest.ID))
    return quest, nil
}
```

The pattern repeats: `audit-fail-on-error, audit-ok-on-return,
idempotency-bracket-around-the-real-work`. New methods follow the
same shape; the test suite (§Test plan) enforces it by inspecting
audit-log contents after every call.

## Audit log contract

Every Service call writes one `audit_events` row before returning,
even if the call fails authentication. Schema (per
[design/multi-user.md](multi-user.md)):

```sql
CREATE TABLE audit_events (
    id          INTEGER PRIMARY KEY,
    user_id     TEXT NOT NULL,            -- from token, even for failed auth
    action      TEXT NOT NULL,            -- "create_quest", "cancel_quest", ...
    target_id   TEXT,                     -- questID, accountID, etc., nullable
    ok          INTEGER NOT NULL,         -- 0/1
    err_code    TEXT,                     -- sentinel name when ok=0
    created_at  INTEGER NOT NULL          -- unix nanos
);
CREATE INDEX audit_events_user_time ON audit_events(user_id, created_at DESC);
```

The audit row is written **before** the response is built. If the
write itself fails the call returns `ErrInternal` and the operator
investigates — an unaudited mutation is a bug (per
[ADR-0010](../adr/0010-one-daemon-many-users.md): the daemon's audit
log is a tenancy contract, not a nice-to-have).

The audit row records `user_id` from the token even when the action
is unauthorized — this is how the operator detects token misuse.
Tenant-scoped reads use the same row format, so `bd audit list
--user phall` shows phall's full activity in one query.

Read-only methods (`GetQuest`, `ListQuests`, `ListAccounts`) audit at
the same level. Volume is acceptable: friends-and-family scale, ~10
calls per user per day. Trim policy: 90 days retention, dropped via
nightly daemon job.

## Test plan

`internal/service` ships with three test layers:

1. **Per-method unit tests, all-fakes.** Every dep (`Resolver`,
   `Planner`, `*engine.Engine`, `Store`, `Secrets`, `Notifier`,
   `clock.Clock`, `AuditLogger`) has a fake. Tests cover:
   - happy path returns expected value
   - tenancy enforcement: caller B's userID + caller A's questID →
     `ErrNotFound`
   - sentinel propagation: each downstream error maps to the right
     Service sentinel
   - audit row is written, with correct action/target/ok/err_code
   - idempotency: repeated `CreateQuest` with same key returns cached
     value; same key + different action → `ErrConflict`
   100% method coverage. Each Service method has at least one test
   per row of the error-model table that's reachable from it.

2. **Integration test, in-memory SQLite + fake provider.** One test
   constructs the real Service over the real `*store.SQLiteStore`
   (file://:memory:), the real Resolver, the real Planner, the real
   Engine, the fake Provider. It exercises every verb in a realistic
   sequence:
   ```
   InviteUser → AcceptInvite → Login → ResolveVenue → PlanQuest →
   CreateQuest (with planHash) → GetQuest → SubscribeQuest (in goroutine,
   collect events) → CancelQuest → ListQuests → RevokeToken
   ```
   Asserts the audit log contains 11 rows, in order, with correct
   `user_id`s.

3. **`tenant_check.go`** ([ADR-0010](../adr/0010-one-daemon-many-users.md))
   builds calls with mismatched user IDs against every Service
   method and asserts the result is `ErrNotFound` (not `ErrUnauthorized`,
   not nil). New methods that don't satisfy the check fail the build.

All three layers run under `-race` per Law 17. The integration test
runs under `clock.NewFake` per Law 8 — `SubscribeQuest`'s ctx is
cancelled by the test driver, not by elapsed time.

## Versioning

The Service interface is `internal/`. Go's package-visibility rules
prevent external code from importing it; the only consumers are
in-tree (`internal/transport/http`, `internal/transport/mcp`,
`cmd/resy-snipe`).

Therefore: **breaking changes to the Service interface are fine.**
Both transports update in the same PR. There is no compatibility
window inside the binary.

The transports' wire formats are versioned independently:

- HTTP API versioning lives in [design/daemon.md](daemon.md) §versioning
  (URL prefix `/v1/...`, deprecation policy, breaking-change rules).
- MCP versioning lives in [design/mcp.md](mcp.md) §versioning (tool
  schema versioning, capability negotiation).

A breaking Service change might be wire-compatible at HTTP and break
at MCP, or vice versa. The transports own their own compatibility
stories. Service changes only need to keep the in-tree call sites
compiling.

This is the same model
[`internal/providers`](../../../internal/providers) uses: an internal
interface that turns over freely; concrete adapters absorb churn.

## Anti-patterns

These are explicit non-goals. Code that does any of them is wrong.

- **Service depends on transports.** The Service does not import
  `net/http`, `mcp`, JSON-RPC, status codes, or SSE framing. It does
  not know it has transports. (If it did, it would be the transport.)
- **Service depends on `cmd/`.** Per Law 4, `cmd/` is wiring only.
  Service is wired by `cmd/`, not the reverse.
- **Service is generic.** No `Do(action string, args interface{})`,
  no `Call(method string, params map[string]any)`. Each verb is a
  typed Go method. Generic dispatch is what we have transports for.
- **Service has business logic.** A Service method that contains a
  loop, a state transition, an HTTP call, or a SQL string is doing
  the wrong layer's job. Push it down into Resolver / Planner /
  Engine / Store.
- **Service skips audit.** Every method writes an `audit_events` row.
  A Service method without an audit call is a bug; the test suite
  catches it.
- **Service trusts client-provided UserIDs without auth.** UserID
  comes from the transport, which got it from the token. Service
  never accepts a UserID claim from the request body.
- **Service has its own goroutines.** Per Law 11, no background
  goroutines outlive their function. `SubscribeQuest` blocks on its
  caller's ctx; it does not spawn detached workers.
- **Service exposes `chan T`.** Streaming is a callback. Channels are
  Go-isms that don't translate to the MCP wire (per
  [ADR-0004 Notes](../adr/0004-mcp-as-peer-front-end.md#notes)).

## Cross-references

- [ADR-0003](../adr/0003-daemon-first-cli-as-client.md) — daemon
  hosts the Service; CLI is a thin HTTP client of one transport.
- [ADR-0004](../adr/0004-mcp-as-peer-front-end.md) — MCP is the
  second transport, peer to HTTP. Same Service.
- [ADR-0005](../adr/0005-multi-user-data-model-from-day-one.md) —
  every Service call carries `UserID`.
- [ADR-0010](../adr/0010-one-daemon-many-users.md) — one process,
  logical tenancy; Service enforces it.
- [ADR-0011](../adr/0011-operator-issued-invites-no-self-registration.md)
  — `InviteUser` / `AcceptInvite` flow.
- [ADR-0012](../adr/0012-plan-first-ux.md) — `PlanQuest` /
  `CreateQuest` separation; plan-hash pinning.
- [design/resolver.md](resolver.md) — `Resolver` dep.
- [design/planner.md](planner.md) — `Planner` dep, `Plan` shape, hash.
- [design/daemon.md](daemon.md) — HTTP transport over Service.
- [design/mcp.md](mcp.md) — MCP transport over Service.
- [design/multi-user.md](multi-user.md) — `users` / `accounts` /
  `audit_events` schema.
- [internal/engine/subscribe.go](../../../internal/engine/subscribe.go)
  — pubsub Service wraps for `SubscribeQuest`.
