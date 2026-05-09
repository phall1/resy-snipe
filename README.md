# resy-snipe

A typed, observable, race-and-cancel reservation sniper for Resy.
Single-binary Go CLI. Every line of orchestration is on the same
state machine; every state change is persisted; every external call
goes through one error taxonomy.

> **Ethics & ToS**: this tool automates Resy endpoints. It can violate
> rate limits and acceptable-use. Use it for accounts and venues you
> are authorized to book. Do not point it at venues you do not have a
> personal stake in.

```
              ┌────────────┐
              │  CLI flags │
              │ + interactive
              └─────┬──────┘
                    ▼
              ┌────────────┐
              │  Intent    │ ── content-addressed by Hash() ──┐
              └─────┬──────┘                                  │
                    ▼                                         ▼
        ┌────────────────────┐                       ┌────────────────┐
        │ engine.Submit/Run  │ ◀──── providers ─────▶│  resy.Client   │
        │  state machine +   │      Provider seam    │  + SlotPreparer
        │  release strategy  │                       └────────────────┘
        └─────┬──────────────┘                                ▲
              │ events                                        │ HTTP
              ▼                                               ▼
        ┌────────────────────┐                       ┌────────────────┐
        │ store.Store (SQLite, WAL)                 │  api.resy.com  │
        │  snipes, events, sessions, observed releases             │
        └────────────────────┘                       └────────────────┘
              │
              ▼
        ┌────────────────────┐
        │ engine.Subscribe   │ ─────▶ notify.Notifier (stdout, future SMS/iMessage)
        └────────────────────┘
```

---

## What it does

- **Login + persisted session**. JWT decoded for exp; expired session
  produces `run 'resy-snipe login' first` instead of mid-snipe failure.
  Session lives in `~/.local/share/resy-snipe/db.sqlite` (or
  `XDG_DATA_HOME`).
- **Three release strategies**:
  - **Explicit** — wake up at a known wall-clock time (e.g. midnight).
  - **Discovered** — poll Resy's calendar between `ProbeFrom` and
    `ProbeUntil`; the first time the target date appears available is
    the release moment, and we record the wall-clock-of-day so the next
    snipe for the same venue defaults to Explicit.
  - **Continuous** — poll Find at the engine's PollFloor until a slot
    appears or `Until` elapses.
- **Race-and-cancel booking**. PrepareSlot (`/3/details`) is serialized
  across candidates so we don't burn book_tokens we won't use; Confirm
  (`/3/book`) races in parallel goroutines under a shared cancellable
  context. The first successful Confirm cancels its siblings.
- **Graceful shutdown**. SIGINT/SIGTERM cancel the parent context.
  In-flight `/3/book` calls are detached from cancellation so we never
  end up with a confirmed-but-orphaned reservation; everything else
  unwinds cleanly within 5 s.
- **One error taxonomy**. Every Resy HTTP failure is classified into
  `providers.Err{AuthExpired,MFARequired,BookTokenExpired,SlotTaken,
  RateLimited,AntiBotChallenge,InventoryEmpty}` so engine code never
  string-matches a body.
- **Idempotency** on `/3/book`. Each attempt sends an
  `X-Resy-Idempotency-Key` derived from `sha256(intent_hash, slot
  payload, attempt)`. A retry with the same `attempt` cannot double-book.

---

## Quick start

```bash
# 1. Build
just build               # → bin/resy-snipe

# 2. Log in once. Persisted in the local SQLite store.
bin/resy-snipe login

# 3. Snipe at midnight tonight, Dead Rabbit, party of 2,
#    7 days out, prefer 18:30 then 19:00.
bin/resy-snipe \
  -user phall@example.com \
  -venue-id 38660 \
  -res-times 18:30,19:00 \
  -snipe-time 00:00
```

Without `-user`, the binary stays in **dry-run preview** mode — it
prints the assembled Intent and exits. This is intentional: silently
sniping with whichever session happens to be in the store is exactly
the kind of footgun the spec calls out.

### Anti-bot signing (optional)

The Resy adapter has a pluggable seam for PerimeterX-aware signing.
Without configuration, it's a no-op and the binary behaves exactly as
the docs above describe. If you have a signing binary that produces
the headers Resy's anti-bot pipeline expects, point at it via:

```bash
export RESY_SNIPE_SIGNER_BIN=/path/to/your-signer
```

The binary is invoked once per `/3/details` and `/3/book` call as
`<bin> sign --provider resy <path>`; it must write JSON to stdout in
the form `{"headers": {"x-px-foo": "..."}}`. On a `403` anti-bot
response the adapter calls `<bin> reset --provider resy` and retries
the request once. See [docs/anti-bot.md](docs/anti-bot.md) for the
full envelope.

---

## CLI surface

| Flag | Purpose |
|---|---|
| `-user <email>` | Required for the live snipe path. Selects the persisted session. |
| `-venue-id <id>` | Resy venue id. Built-in shortcuts in interactive mode. |
| `-date YYYY-MM-DD` | Reservation date. Defaults to `now+7d` in `America/New_York`. |
| `-party-size <n>` | Defaults to 2. |
| `-res-times "18:30,19:00"` | Preferred times, comma-separated. First match wins the race. |
| `-table-types "Bar,Parlor"` | Optional. `none` accepts any. |
| `-snipe-time 00:00` | Wall-clock release moment for `ExplicitRelease`. |
| `-snipe-date YYYY-MM-DD` | Defaults to reservation date. |
| `-release-strategy explicit\|discovered\|continuous` | Defaults to explicit when `-snipe-time` is set, otherwise discovered. |
| `-retry-window 30m` | Polling envelope for Discovered/Continuous. |
| `-log-level debug\|info\|warn\|error` | Defaults to info. |
| `-interactive` | Walk the prompt flow. |

Subcommands: `resy-snipe login` walks the credential capture +
persistence flow and exits.

---

## Project layout

```
cmd/resy-snipe/      CLI entry, flag parsing, interactive prompt,
                     login subcommand, snipe lifecycle wiring,
                     providerAdapter (resy.Client → providers.Provider).

internal/
  domain/            Pure types: Status, Event, Intent, ReleaseStrategy
                     (sealed union), SlotPayload (sealed union),
                     transition table, idempotency key.
  providers/         Cross-provider seam: Provider interface + sentinel
                     error taxonomy. Engine depends only on this.
  resy/              Provider implementation: typed HTTP client, login
                     with MFA, calendar/find/details/book endpoints,
                     anti-bot classifier, JWT-exp session.
  resy/sign/         Anti-bot signing seam (Signer interface + Noop
                     default + Subprocess wrapper). See docs/anti-bot.md.
  store/             SQLite (modernc, WAL) + schema migrations + typed
                     payload codec.
  clock/             Clock interface + real + fake (drives tests).
  engine/            State machine + scheduler (Clock.AfterFunc) +
                     release-strategy loops + race-and-cancel booking +
                     subscriber surface.
  notify/            Notifier interface + stdout impl. Engine event
                     stream lands here via cmd/'s notifierBridge.
docs/                See docs/README.md.
```

---

## Documentation

The `docs/` tree is the durable record of how this thing is supposed
to work. If something here disagrees with the code, fix one or the
other — don't leave the contradiction.

- [docs/README.md](docs/README.md) — index & reading order
- [docs/architecture.md](docs/architecture.md) — package responsibilities, deps graph
- [docs/state-machine.md](docs/state-machine.md) — status diagram + transition rules
- [docs/release-strategies.md](docs/release-strategies.md) — explicit / discovered / continuous
- [docs/invariants.md](docs/invariants.md) — load-bearing properties
- [docs/laws.md](docs/laws.md) — project conventions (lint, layering, idioms)
- [docs/anti-bot.md](docs/anti-bot.md) — what we detect, what we don't yet handle
- [docs/state.md](docs/state.md) — current snapshot of what works, what's wired
- [docs/work-items.md](docs/work-items.md) — pointer to beads + open epics
- [docs/opentable-mapping.md](docs/opentable-mapping.md) — provider-interface stress test

---

## Build & test

```bash
just build          # → bin/resy-snipe
just test           # go test -race ./...
just lint           # golangci-lint with the strict ruleset
just gates          # build + test + custom invariants (no time.Now()
                    # outside internal/clock; no map[string]any; etc.)
```

The `gates` recipe is the ground truth for "is this branch shippable."
CI runs the same thing. Pre-commit (lefthook) runs gofmt + govet + bd
validation + gates + golangci-lint on every commit.

---

## Status

Phase 1 is complete: login, three release strategies, race-and-cancel
booking, graceful shutdown, full state-machine persistence, end-to-end
test coverage. The CLI snipe path is wired end-to-end — `resy-snipe -user
… -snipe-time …` actually books.

Anti-bot signing is wired as a pluggable seam (see "Anti-bot
signing" above and [docs/anti-bot.md](docs/anti-bot.md)). A Phase 2
daemon mode is tracked in [docs/work-items.md](docs/work-items.md).

For project-state details and what's next, see [docs/state.md](docs/state.md).

---

## License & lineage

Forked from a small reservation-sniping CLI; the entire architecture
(state machine, provider seam, race-and-cancel, persisted store) is
new. The legacy entry point lives in `_legacy/` for archaeology only.
