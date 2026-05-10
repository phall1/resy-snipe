# v2 architecture overview

resy-snipe v2 is a goal-seeking reservation-orchestration daemon with
two equal front-ends: humans (CLI/TUI) and agents (MCP). v1 was a
single-shot CLI that compiled a `domain.Intent` from flags, ran the
engine in-process, and exited; the user was the planner. v2 keeps the
v1 engine intact and inserts three new layers in front of it —
**Resolver → Planner → Service** — wraps the whole thing in a
**daemon**, and exposes that daemon over two transports: an HTTP API
the CLI calls and an MCP server agents call. Every load-bearing
decision behind that shape is captured as an ADR; this doc is the map.

Reader's mental model: the engine, the providers seam, the store, and
the clock are unchanged from v1. Everything new is upstream of the
engine (Goal → Resolver → Planner → Plan → Engine) or around the
process (daemon, HTTP, MCP, multi-user, sealed secrets). If you came
from v1's [docs/architecture.md](../../architecture.md), nothing you
already learned about the engine is wrong.

## Big picture

```
   ┌────────────────┐    ┌───────────────────┐
   │ Humans         │    │ Agents            │
   │ (CLI, TUI)     │    │ (Claude, MCP)     │
   └───────┬────────┘    └─────────┬─────────┘
           │ HTTP                  │ MCP (stdio | HTTP)
           ▼                       ▼
        ┌──────────────────────────────┐
        │ daemon (resy-snipe serve)    │
        │  ┌────────────────────────┐  │
        │  │ Service                │  │   ← in-process Go iface
        │  │  PlanQuest             │  │     shared by both
        │  │  CreateQuest           │  │     transports
        │  │  WatchEvents           │  │
        │  │  ListQuests / Cancel   │  │
        │  └──────┬─────────────────┘  │
        │         │ composes           │
        │  ┌──────▼─────┐ ┌──────────┐ │
        │  │ Resolver   │ │ Planner  │ │
        │  │ URL/slug   │ │ Goal +   │ │
        │  │ → Venue    │ │ Venue    │ │
        │  │            │ │ → Intent │ │
        │  └──────┬─────┘ └────┬─────┘ │
        │         │            │       │
        │         └─────┬──────┘       │
        │               ▼              │
        │           ┌────────┐         │
        │           │ Engine │ (v1)    │
        │           └───┬────┘         │
        │               │              │
        │  cross-cutting (any layer):  │
        │   Store · Secrets · Notifier │
        │   Clock · Signer · Provider  │
        └──────────────────────────────┘
```

Two front-ends, one Service, three serial domain layers, the v1
engine at the bottom. The cross-cutting boxes are interfaces — every
layer uses them, no layer owns their implementation.

ADRs that shape this picture:
[ADR-0001](../adr/0001-goal-driven-architecture.md) (Goal vs Intent),
[ADR-0002](../adr/0002-resolver-planner-engine-layering.md)
(three-layer split),
[ADR-0003](../adr/0003-daemon-first-cli-as-client.md) (daemon-first),
[ADR-0004](../adr/0004-mcp-as-peer-front-end.md) (MCP as peer),
[ADR-0012](../adr/0012-plan-first-ux.md) (Plan-first UX).

## Package layout

After M1–M3 the tree is:

```
cmd/resy-snipe/                — wiring + transports + CLI verbs
internal/
  domain/                      — pure data + transitions      (v1, +Goal,+Plan)
  clock/                       — time injection seam          (v1, unchanged)
  providers/                   — cross-provider iface         (v1, +ResolveVenue)
  store/                       — SQLite, schema, migrations   (v1, +new tables)
  resy/                        — Resy adapter                 (v1, unchanged)
  resy/sign/                   — anti-bot signer seam         (v1, unchanged)
  notify/                      — Notifier iface               (v1, unchanged)
  engine/                      — state machine + booking race (v1, unchanged)
  resolver/                    — VenueQuery → Venue           (NEW, M1)
  planner/                     — (Goal,Venue) → Plan/Intent   (NEW, M1)
  service/                     — RPC surface used by both     (NEW, M1)
                                 transports; composes the
                                 three layers above
  daemon/                      — process lifecycle, config,   (NEW, M2)
                                 HTTP transport, health,
                                 boot order, shutdown
  mcp/                         — MCP transport                (NEW, M3)
  secrets/                     — sealed-at-rest secrets       (NEW, M2)
  observability/               — logs, metrics, doctor        (NEW, M2)
```

Eight existing packages, six new. New code lives in new packages —
v1 directories are not reshuffled. See [ADR-0002](../adr/0002-resolver-planner-engine-layering.md)
for why the three new domain layers are siblings rather than one
fat package, and [ADR-0006](../adr/0006-sqlite-only-no-external-deps.md)
for why nothing in the cross-cutting set adds an external dependency.

## Dependency rules

The v1 layering ([docs/laws.md](../../laws.md) §Layering) carries
forward unchanged. New rules for the new packages:

```
                           cmd/resy-snipe
                                │
                ┌───────────────┼────────────────┐
                ▼               ▼                ▼
              daemon           mcp           (CLI verbs)
                └────────┬──────┘
                         ▼
                       service
              ┌──────────┼──────────┐
              ▼          ▼          ▼
          resolver    planner     engine
              │          │          │
              └────┬─────┴────┬─────┘
                   ▼          ▼
               providers    store
                   │          │
                   └─────┬────┘
                         ▼
                       domain
                         │
                         ▼
                       clock

  cross-cutting (consumed by service & up):
    secrets        ── needs domain, store, clock
    notify         ── needs domain (v1, unchanged)
    observability  ── needs domain, clock
    resy/sign      ── needs clock (v1, unchanged)
```

Rules:

- **`internal/resolver`** depends on `providers`, `domain`, `clock`.
  Nothing else. It does not import `internal/resy`, does not import
  `internal/store`, does not call the planner. Its job is one
  function: `Resolve(ctx, VenueQuery) → domain.Venue`. See
  [design/resolver.md](resolver.md).
- **`internal/planner`** depends on `providers`, `domain`, `clock`.
  Same constraints as resolver. Pure function over `(Goal, Venue) →
  Plan` with `/4/find` as its only network call, mediated by the
  provider seam. Caching of `(venue, date) → drop_moment` lives here.
  See [design/planner.md](planner.md) and
  [ADR-0012](../adr/0012-plan-first-ux.md).
- **`internal/service`** depends on `resolver`, `planner`, `engine`,
  `store`, `secrets`, `notify`, `domain`, `clock`. The first package
  in the graph allowed to touch all four. Method signatures use
  serializable types only — no channels, no private fields, no
  Go-only unions ([ADR-0004](../adr/0004-mcp-as-peer-front-end.md)
  Notes). See [design/service-layer.md](service-layer.md).
- **`internal/daemon`** depends on `service`, `secrets`,
  `observability`, plus the concrete adapters (`resy`, `resy/sign`,
  `store/sqlite`). It is the only package that wires concrete
  implementations to interfaces — the v2 equivalent of v1's
  `cmd/resy-snipe/main.go` wiring. See [design/daemon.md](daemon.md).
- **`internal/mcp`** depends on `service` and `domain` only. It does
  not depend on `daemon`; the daemon constructs an MCP server given
  a Service and mounts its transport. See [design/mcp.md](mcp.md).
- **`internal/secrets`** depends on `store`, `domain`, `clock`.
  Owns AES-GCM seal/unseal, KDF, key rotation. The Service layer
  asks for plaintext only at the point of use; everything else
  sees ciphertext. See [design/secrets.md](secrets.md) and
  [ADR-0008](../adr/0008-secrets-sealed-at-rest-operator-key.md).
- **`internal/observability`** depends on `domain` and `clock`.
  Structured logging contract, metrics registry, `healthz` and
  `doctor` reporters. No package imports it for log production
  (every package logs via `log/slog` with structured keys) — it
  owns sinks, redactors, and the audit-log tap.
- **`cmd/resy-snipe`** stays the wiring + verbs layer. v1's
  in-process engine glue collapses into a `client` subpackage that
  speaks HTTP to the daemon. There is no embedded-engine fallback
  ([ADR-0003](../adr/0003-daemon-first-cli-as-client.md)).

What this rules out:

- Resolver calling Planner (or vice versa) — both are siblings
  composed by Service.
- Engine calling Resolver — Engine consumes a fully-formed Intent;
  it knows nothing about URLs or release-time inference.
- MCP calling HTTP — both are peers consuming the same in-process
  Service ([ADR-0004](../adr/0004-mcp-as-peer-front-end.md)).
- Daemon importing `internal/mcp` business logic — daemon mounts the
  MCP server's transport handler, no more.
- Anything below `service` reading the `secrets` package — secrets
  unwrap is a Service-layer concern; lower layers receive plaintext
  via injected dependencies (e.g. the `resy.Client` is constructed
  with a session blob the Service decrypted).

## Request flow: Goal to booked reservation

A user pastes a Resy URL. End-to-end:

```
1.  Human                 $ resy-snipe quest plan https://resy.com/cities/ny/astoria
                            --date 2026-06-12 --party 2 --time 19:00-21:00
2.  CLI                   parses flags + URL → builds domain.Goal
                          POST /v1/quests:plan  { goal }
3.  daemon HTTP           authenticates bearer → resolves UserID
                          calls service.PlanQuest(ctx, userID, goal)
4.  Service               resolver.Resolve(goal.VenueQuery) → Venue
                          planner.Plan(goal, venue, now) → Plan
                          returns Plan (incl. content hash)
5.  daemon HTTP           serializes Plan → 200 OK
6.  CLI                   prints Plan as a table; prompts "create? [y/N]"
7.  Human                 y
8.  CLI                   POST /v1/quests  { goal, plan_hash }
9.  daemon HTTP           service.CreateQuest(ctx, userID, goal, planHash)
10. Service               recomputes Plan; verifies hash matches
                          store.UpsertQuest(quest) — persists Goal + Plan
                          engine.Submit(intent) — schedules wake-up
                          notifier.QuestCreated(quest) — fan-out
                          returns Quest{ID, status=Scheduled}
11. CLI                   prints quest id; exits
12. (later) Engine        Clock.AfterFunc fires at DropMoment
                          state machine transitions Scheduled → Firing
                          providers.Find → providers.PrepareSlot →
                          providers.Book (race, detached ctx)
                          emits domain.Event for each transition
13. daemon                bridges engine events → in-proc pubsub
14. CLI/TUI/MCP           subscribers see events live
                          (CLI: `resy-snipe quest watch <id>`
                           MCP: `notifications/quest_event`)
15. Engine                terminal status (Booked | Failed | Aborted)
                          notifier.Result(quest, outcome) — fan-out
                          audit-log row written
```

The agent path through MCP is structurally identical: `plan_quest`
tool → present Plan → user confirms → `create_quest` with
`plan_hash` → subscribe to `notifications/quest_event`. Same Service
methods, different transport. See [design/mcp.md](mcp.md) for the
tool catalog, and [ADR-0012](../adr/0012-plan-first-ux.md) for why
plan-and-confirm is split from commit.

The Engine half of step 12 (Find → PrepareSlot → Book race) is
exactly the v1 flow. See [docs/state-machine.md](../../state-machine.md)
and [docs/anti-bot.md](../../anti-bot.md) for the unchanged details.

## What did not change from v1

Seven things you already know still apply. If you came in expecting
a rewrite: you didn't get one.

1. **Engine state machine.** `domain.Status`, `CanTransition`,
   `SnipeState.Transition` are byte-for-byte the same.
   [docs/state-machine.md](../../state-machine.md) is still
   authoritative.
2. **Booking race semantics.** `RunBookingRace` keeps PrepareSlot
   serialized, ConfirmSlot raced, sibling cancellation on first win,
   in-flight Book detached from parent ctx. See
   [`internal/engine/race.go`](../../../internal/engine/race.go) and
   [I-4](../../invariants.md#in-flight-book-is-detached-from-cancel).
3. **Signer interface.** `internal/resy/sign` is the anti-bot seam,
   `Noop` default, `Subprocess` for operator-supplied binaries. v2
   adds a daemon-shared signer instance ([ADR-0010](../adr/0010-one-daemon-many-users.md))
   but the contract is the same. See
   [docs/anti-bot.md](../../anti-bot.md).
4. **Store interface.** Operations on `Snipe`, `Event`, `SessionRow`,
   `ObservedRelease` are unchanged. M1 adds `Quest`, `User`,
   `Account`, `AuditEvent`, `Invite`, `Secret` — additive, not a
   rewrite. SQLite + WAL still the only datastore
   ([ADR-0006](../adr/0006-sqlite-only-no-external-deps.md)).
5. **Clock injection.** `clock.Clock` is still the only way to read
   time. `gates` still greps for `time.Now()`. Tests still use
   `clock.NewFake`. Nothing about the v2 layers above the engine
   bypasses this.
6. **Providers seam.** `providers.Provider` stays the cross-provider
   interface. v2 adds `ResolveVenue(ctx, VenueQuery) → Venue` to
   support the Resolver; existing methods (`Find`, `PrepareSlot`,
   `Book`, `Calendar`) are unchanged.
7. **Adapter pattern.** `*resy.Client` is still a concrete
   implementation behind `providers.Provider`, with a compile-time
   `var _ providers.Provider = (*providerAdapter)(nil)` check. The
   adapter moves from `cmd/` into `internal/daemon` (it's wiring),
   but the pattern is identical.

The "engine is unchanged" claim is the strongest invariant of v2.
A change to engine code outside a fix for an existing v1 bug is a
red flag — ask before you merge.

## What did change from v1

Seven things are new. None of them rewrite v1 code; all are added
above or around it.

1. **`domain.Goal`.** A new sealed type for "what the user wants,"
   distinct from "what the system will do." v1 conflated the two in
   `Intent`. See [ADR-0001](../adr/0001-goal-driven-architecture.md).
2. **`domain.Plan`.** A serializable, hashable artifact derived from
   `(Goal, Venue)`. Users (and agents) approve the Plan before
   commit. See [ADR-0012](../adr/0012-plan-first-ux.md).
3. **`internal/resolver`.** New package owning `VenueQuery → Venue`.
   Hides `/3/venue` and slug parsing. See
   [design/resolver.md](resolver.md).
4. **`internal/planner`.** New package owning `(Goal, Venue) →
   Plan`. Picks `ReleaseStrategy`, computes `DropMoment`,
   per-account rate-limiting lives here. See
   [design/planner.md](planner.md).
5. **`internal/service`.** The RPC surface both transports consume.
   The single source of truth for "what the system can do." See
   [design/service-layer.md](service-layer.md).
6. **`internal/daemon` + `internal/mcp`.** Long-lived process plus
   second front-end. The CLI is now a thin HTTP client; MCP is a
   peer ([ADR-0003](../adr/0003-daemon-first-cli-as-client.md),
   [ADR-0004](../adr/0004-mcp-as-peer-front-end.md)). See
   [design/daemon.md](daemon.md), [design/mcp.md](mcp.md).
7. **Multi-user data model from M1.** `users`, `accounts`, `quests`,
   `audit_events`, `invites`, `secrets` (sealed) tables. Every
   Service call carries a `UserID`; every Store query joins on it.
   See [ADR-0005](../adr/0005-multi-user-data-model-from-day-one.md),
   [ADR-0010](../adr/0010-one-daemon-many-users.md),
   [ADR-0011](../adr/0011-operator-issued-invites-no-self-registration.md),
   [design/multi-user.md](multi-user.md).

A practical implication of (7): even M1's single-operator deployment
goes through the multi-tenant code paths. There is no
"single-user mode" code path to retrofit later. The single user is
just one row in `users`.

## Out of scope

Things v2 deliberately does not do, with the ADR that locks the
decision in:

- **No SaaS, no hosted instance.** Self-hosted only. See
  [ADR-0007](../adr/0007-self-hosted-only-no-saas.md). The README
  never directs users to a URL or signup form.
- **No cross-user Resy account sharing in v1.** One user owns one
  Resy account. The data model permits sharing later but the API
  doesn't expose it. See
  [ADR-0010 §Notes](../adr/0010-one-daemon-many-users.md#notes) and
  [design/multi-user.md](multi-user.md).
- **No self-registration.** New users join via operator-issued
  invite tokens. There is no `/register` endpoint. See
  [ADR-0011](../adr/0011-operator-issued-invites-no-self-registration.md).
- **No internet-facing TLS in the daemon.** Operator terminates TLS
  at a reverse proxy (Caddy / nginx / Tailscale Funnel). The daemon
  binds to `127.0.0.1` by default. See
  [ADR-0009](../adr/0009-reverse-proxy-native-http.md).
- **No external datastore.** No Redis, no Postgres, no message
  broker. SQLite + WAL is the whole persistence layer. See
  [ADR-0006](../adr/0006-sqlite-only-no-external-deps.md).
- **No web UI.** TUI is the human surface; agents use MCP. A web UI
  is a v3 problem.
- **No federated multi-instance.** Each homelab box is a standalone
  daemon. No cross-instance coordination. See
  [ADR-0007](../adr/0007-self-hosted-only-no-saas.md).
- **No standalone-CLI fallback.** If the daemon isn't running, the
  CLI prints an error pointing at `resy-snipe serve`. There is no
  embedded engine in the CLI binary. See
  [ADR-0003 §Notes](../adr/0003-daemon-first-cli-as-client.md#notes).
- **No plaintext secrets at rest.** Resy passwords, session JWTs,
  and signer state are AES-GCM sealed with an operator-controlled
  key. See [ADR-0008](../adr/0008-secrets-sealed-at-rest-operator-key.md)
  and [design/secrets.md](secrets.md).

## Where to read next

Pick the closest layer to the work you're about to do.

- Touching URL/slug/name parsing or the venue cache →
  [design/resolver.md](resolver.md)
  ([ADR-0002](../adr/0002-resolver-planner-engine-layering.md)).
- Picking a release strategy or computing a drop moment →
  [design/planner.md](planner.md)
  ([ADR-0001](../adr/0001-goal-driven-architecture.md),
  [ADR-0012](../adr/0012-plan-first-ux.md)).
- Adding a Service method or wiring a transport →
  [design/service-layer.md](service-layer.md)
  ([ADR-0003](../adr/0003-daemon-first-cli-as-client.md),
  [ADR-0004](../adr/0004-mcp-as-peer-front-end.md)).
- Boot order, config, HTTP, healthz, deployment →
  [design/daemon.md](daemon.md)
  ([ADR-0006](../adr/0006-sqlite-only-no-external-deps.md),
  [ADR-0009](../adr/0009-reverse-proxy-native-http.md)).
- MCP tool catalog, streaming, agent UX →
  [design/mcp.md](mcp.md)
  ([ADR-0004](../adr/0004-mcp-as-peer-front-end.md)).
- Users, accounts, audit, invites, tenancy enforcement →
  [design/multi-user.md](multi-user.md)
  ([ADR-0005](../adr/0005-multi-user-data-model-from-day-one.md),
  [ADR-0010](../adr/0010-one-daemon-many-users.md),
  [ADR-0011](../adr/0011-operator-issued-invites-no-self-registration.md)).
- Sealing, KDF, key rotation, dev-mode →
  [design/secrets.md](secrets.md)
  ([ADR-0008](../adr/0008-secrets-sealed-at-rest-operator-key.md)).
- Logs, metrics, doctor, redaction →
  [design/observability.md](observability.md).
- Engine internals (unchanged from v1) →
  [docs/state-machine.md](../../state-machine.md),
  [docs/anti-bot.md](../../anti-bot.md),
  [docs/architecture.md](../../architecture.md).
