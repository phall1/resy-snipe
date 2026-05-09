# Architecture

resy-snipe is a single Go binary. The cut between packages is an
explicit dependency rule, not a stylistic preference: each package can
only depend on the ones below it. A change that violates the rule is a
change that's about to grow a circular dep or accidentally couple two
unrelated concerns.

## Dependency graph

```
                  cmd/resy-snipe
                  ┌──────┴───────┐
                  ▼              ▼
              engine          notify
              ┌───┴────┐         │
              ▼        ▼         ▼
          providers   store    domain
              │        │         ▲
              └────────┴────┐    │
                            ▼    │
                          domain ─┘
                            │
                            ▼
                          clock
```

Rules:

- **`internal/domain`** depends on nothing inside the project (only
  `time`, `crypto`, `slices`, etc.).
- **`internal/clock`** depends on nothing.
- **`internal/providers`** depends on `domain` only. The interface
  here is the cross-provider seam.
- **`internal/store`** depends on `domain` only. SQL/SQLite is hidden
  behind the `Store` interface; everything else gets the interface.
- **`internal/resy`** is one concrete `providers.Provider`
  implementation. Depends on `providers`, `domain`, `clock`,
  `resy/sign`. Does **not** depend on `store` — session persistence
  goes through a slim local `SessionStore` interface that `cmd/`
  adapts a real store to.
- **`internal/resy/sign`** is the anti-bot signing seam consulted by
  `internal/resy` before each `/3/details` and `/3/book` call.
  Depends only on `internal/clock`. Ships a `Noop` default (so every
  caller that hasn't opted in keeps working unchanged) and a
  `Subprocess` impl that shells out to an operator-supplied signing
  binary configured via `RESY_SNIPE_SIGNER_BIN`. See
  [anti-bot.md](anti-bot.md).
- **`internal/notify`** depends on `domain` only. The Notifier
  interface lives here.
- **`internal/engine`** depends on `providers`, `store`, `domain`,
  `clock`. Never on a concrete provider. Never on `notify` (the engine
  emits events; `cmd/` bridges them to a Notifier).
- **`cmd/resy-snipe`** is the wiring layer. It owns construction,
  signal handling, the `providerAdapter` that bridges
  `*resy.Client` to `providers.Provider`, and the `notifierBridge`
  that subscribes to engine events.

A new provider (OpenTable, Tock, …) lands as a new sibling of
`internal/resy` plus a new adapter in `cmd/`. The engine does not
change.

## Package responsibilities

### `internal/domain`

Pure data + transition rules. Three sealed unions:

- **Status** — lifecycle states. Transitions enforced by
  `domain.CanTransition`. The transition table in `status.go` is total
  over every (from, to) pair; `init()` panics if it isn't.
- **ReleaseStrategy** — `ExplicitRelease | DiscoveredRelease |
  ContinuousRelease`. The engine dispatches on type-switch.
- **SlotPayload** — currently `ResySlotPayload`, but the union shape
  is what lets the engine never reach `json.RawMessage` for an
  in-memory slot. Decoding happens at the store boundary.

`Intent.Hash()` is the content-addressable id that idempotency keys,
snipe ids, and observability all key off.

### `internal/clock`

`Clock` interface with `Now()`, `After(d)`, `AfterFunc(d, f)`. Real
impl uses the real wall clock; fake impl drives time deterministically
in tests. **Project rule**: nothing outside this package may call
`time.Now()`. The `gates` recipe greps for violations.

### `internal/providers`

Cross-provider interface + sentinel error taxonomy. The engine
imports this; concrete providers implement it.

The interface is intentionally narrower than what Resy exposes — it
takes `Calendar/Find/Book` but not `/3/details`, because details is
an implementation detail of the booking race that the engine drives
through the optional `SlotPreparer` extension (engine.race.go).

### `internal/store`

`Store` interface with operations on `Snipe`, `Event`,
`SessionRow`, `ObservedRelease`. SQLite (modernc, WAL) impl in
`sqlite.go`; schema migrations in `migrate.go`. Payload codec keeps
domain types JSON-encoded at rest and typed in memory.

### `internal/resy`

The Resy adapter. One file per concern (`auth.go`, `find.go`,
`book.go`, `prepare.go`, `calendar.go`, `errors.go`, `session.go`,
…). The HTTP `do()` in `client.go` is the single transit point —
every endpoint goes through it for header assembly, x-request-id
capture, and structured logging.

`errors.go` is the classifier: HTTP status + body markers map to the
sentinel errors in `providers/`. Adapter code never returns a bare
HTTP error to the engine.

### `internal/engine`

The state machine and orchestrator.

- `engine.go` — construction, options, `Submit`, `Load`.
- `state.go` — `SnipeState.Transition` is the single atomic gate for
  status changes (validate → persist → swap in-memory).
- `run.go` — `Run` drives Submitted → Scheduled → release fire.
  `Clock.AfterFunc` is the wake-up mechanism.
- `release.go` — strategy-specific loops (Discovered, Continuous).
- `race.go` — `RunBookingRace`: serialize PrepareSlot, race
  ConfirmSlot, cancel siblings on first win, detach in-flight Book
  from parent ctx.
- `subscribe.go` — `Subscribe(fn)` lets the CLI/notifier observe
  events synchronously, in registration order, with panic isolation.

### `internal/notify`

`Notifier` interface + the Phase 1 stdout impl. The interface is
where SMS / iMessage / chat-bot frontends will plug in. The engine
does not import this — `cmd/`'s `notifierBridge` subscribes engine
events and calls `Notifier.Transition` / `Notifier.Result`.

### `cmd/resy-snipe`

CLI wiring. See `cmd/resy-snipe/main.go` for the run flow,
`snipe.go` for the engine lifecycle, `provider_adapter.go` for the
adapter that turns `*resy.Client` into `providers.Provider`, and
`login.go` for the credential-capture path.

## Anti-patterns this layout rules out

- The engine string-matching an HTTP body ⟹ would require importing
  `resy` (forbidden).
- A test setting `time.Now()` ⟹ caught by the `gates` grep.
- A new `*resy.Client` method that drifts away from `providers.Provider`
  ⟹ `providerAdapter`'s compile-time check breaks the build.
- Domain logic accidentally pulling in SQL ⟹ `store` cannot import
  `domain`'s parents (it has none).
