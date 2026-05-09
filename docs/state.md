# State

What's wired, what's not, and where the load-bearing seams are.
This doc is the answer to "can I run this?" — not the answer to
"is this feature done?" (that's [work-items.md](work-items.md) and
the beads tracker).

> **Last refresh discipline**: edit this doc whenever a major path
> changes wiring. Don't add dated entries.

## Wired end-to-end

The CLI binary executes a complete snipe:

```
resy-snipe login                           → captures + persists session (JWT exp parsed)
resy-snipe -user … -snipe-time 00:00 …    → submits, schedules, waits, books
```

Concretely, the following paths have production wiring AND test
coverage:

- **CLI flag parsing + interactive prompt → `domain.Intent`**
  ([`cmd/resy-snipe/intent.go`](../cmd/resy-snipe/intent.go))
- **Login + MFA + session persistence**
  ([`cmd/resy-snipe/login.go`](../cmd/resy-snipe/login.go),
  [`internal/resy/auth.go`](../internal/resy/auth.go))
- **Engine construction + `Submit` + `Run`**
  ([`internal/engine/engine.go`](../internal/engine/engine.go),
  [`run.go`](../internal/engine/run.go))
- **All three release strategies**
  ([`release.go`](../internal/engine/release.go))
- **Race-and-cancel booking** with serial PrepareSlot + parallel
  ConfirmSlot
  ([`race.go`](../internal/engine/race.go))
- **Engine event stream** via `Subscribe`
  ([`subscribe.go`](../internal/engine/subscribe.go))
- **Notifier bridge** rendering live transitions to stdout
  ([`cmd/resy-snipe/snipe.go:notifierBridge`](../cmd/resy-snipe/snipe.go))
- **Graceful shutdown** (SIGINT/SIGTERM cancels parent ctx;
  in-flight Book detached)
  ([`cmd/resy-snipe/snipe.go:installSignalCancel`](../cmd/resy-snipe/snipe.go))
- **Persistence**: snipes, events, sessions, observed releases
  ([`internal/store`](../internal/store))
- **Idempotency keys** on `/3/book`
  ([`internal/domain/idempotency.go`](../internal/domain/idempotency.go))

## Wired but limited

- **Notifier surfaces**: only stdout. SMS / iMessage / chat-bot impls
  are interface seams without backends.
- **`SearchVenues`**: stubbed. The CLI takes venue ids directly via
  `-venue-id`. The Provider interface still requires the method (so
  the adapter satisfies the type), but a call panics with a clear
  "not implemented" error. See
  [`provider_adapter.go`](../cmd/resy-snipe/provider_adapter.go).
- **Anti-bot recovery**: seam wired. Detection
  (`ErrAntiBotChallenge`) plus a sign-and-retry envelope on
  `/3/details` and `/3/book` that calls `Signer.Reset(ctx)` and
  retries once on a 403 anti-bot response. The default Signer
  (`sign.Noop`) is a no-op, so behavior is unchanged unless
  `RESY_SNIPE_SIGNER_BIN` points at a signing binary that produces
  PerimeterX-aware headers. See [anti-bot.md](anti-bot.md).
- **`-user` is required for a real snipe**. Without it, the binary
  runs in dry-run preview (logs the Intent and exits). This is
  defensive — see the rationale in the README.

## Not wired

- **Phase 2 daemon**. The engine's `Run` is shaped to be called in a
  long-lived loop (`engine.go` doc comment), but there's no daemon
  process driving multiple concurrent snipes. The CLI is one snipe
  per invocation.
- **Cancel from the user**. `StatusCanceled` is a legal terminal but
  there's no `resy-snipe cancel <id>` subcommand yet. SIGTERM mid-run
  produces a `ctx.Err()`-driven exit, not a `Canceled` transition.
- **Multi-account**. `domain.UserID` is threaded through, but the CLI
  assumes one user per invocation. No account switching.
- **Observability beyond slog**. No metrics export. The structured
  log lines carry the canonical fields (`snipe_id`, `venue_ref`,
  `attempt`, `provider`, `resy_request_id`); a metrics shim would
  read those.

## Test coverage snapshot

| Package | Tests | Notes |
|---|---|---|
| `internal/domain` | unit | sealed-union exhaustiveness, transition totality, idempotency, log keys |
| `internal/clock` | unit | fake clock semantics |
| `internal/store` | unit + coverage | >80% as of S4 |
| `internal/providers` | unit | sentinel error semantics |
| `internal/resy` | unit + httptest fixtures | end-to-end against simulated Resy responses; anti-bot floor property |
| `internal/engine` | unit + property + integration | state machine, all three strategies, race-and-cancel, graceful shutdown, subscriber surface |
| `internal/notify` | unit | stdout formatting, attr rendering |
| `cmd/resy-snipe` | unit + e2e wiring | flag parse, interactive, login + MFA, full snipe runSnipe with fake provider |

The full suite runs under `-race` and is the gate `just gates`
checks.

## Build & lint floor

- Go 1.25
- `golangci-lint` with the strict config in `.golangci.yml`
- `lefthook` pre-commit hook runs gofmt + govet + bd validation +
  gates + golangci-lint
- `just gates` is the ground truth for CI parity
