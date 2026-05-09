# Work items

The authoritative tracker is **[beads](https://github.com/steveyegge/beads)**
(local SQLite + Dolt). This doc is the human-readable index.

## Finding work

```bash
bd ready              # issues with no blockers
bd list --status=open # everything open
bd show <id>          # detail
bd update <id> --claim
```

Conventions for this project:

- **Priorities**: P0/P1 are load-bearing for shipping; P2 is "would be
  good"; P3+ is backlog.
- **Issue types**: `feature`, `task`, `bug`. Epics are features that
  group child tasks.
- **Acceptance criteria** live in the `acceptance` field — set on
  every non-trivial issue.
- **Don't open issues for refactors** that fit in the same PR as the
  feature that motivated them. Issue tracker hygiene is for work that
  spans sessions.

## Phase 1 — closed

The 38 closed P1/P2 issues that built the foundation:

- **EPIC: Foundation** (resy-snipe-iy3) — package layout, clock,
  logging, justfile, lefthook, golangci-lint, Go 1.22+.
- **EPIC: Domain & Provider seam** (resy-snipe-96r) — Status table,
  SlotPayload union, ReleaseStrategy union, Event + idempotency,
  Provider interface + sentinel errors, OpenTable mapping exercise.
- **EPIC: Store** (resy-snipe-hji) — SQLite (modernc, WAL), schema
  migrations, Store interface + impl, unit tests at >80%.
- **EPIC: Resy adapter** (resy-snipe-y3l) — typed HTTP client, auth
  with MFA + JWT-exp persistence, calendar, find/details/book
  pipeline, error classifier, end-to-end httptest + anti-bot floor.
- **EPIC: Engine** (resy-snipe-0jn) — state machine, scheduler loop,
  three release strategies, race-and-cancel booking, graceful
  shutdown, full state-machine + property + integration coverage.
- **EPIC: CLI** (resy-snipe-pz3) — flags + interactive → Intent,
  stdout notifier, login subcommand.

Plus **E8 (subscriber surface)** and **E7/C4 (engine ↔ CLI wiring)**
that made the binary actually book.

## Phase 1.5 — closed

- **R7: printing-press integration** (resy-snipe-ou3, P2) — *closed*.
  Shipped as the `internal/resy/sign` seam (`Signer` interface +
  `Noop` default + `Subprocess` wrapper) plus a sign-and-retry
  envelope on `/3/details` and `/3/book`. Decision: subprocess
  wrapper, not Go port — the named upstream
  (`mvanhorn/cli-printing-press`) turned out to be a CLI generator,
  not a Resy signing toolkit, so the seam-with-pluggable-binary is
  the durable deliverable. See [anti-bot.md](anti-bot.md).

## Phase 1.5 — open

The visible gaps before this is "really done."

### Open

- **Doc refresh** (resy-snipe-kol, P2)
  This doc tree. ←[meta]

### Plausible next epics (not yet filed)

- **EPIC: Daemon (Phase 2)**: long-lived process driving N concurrent
  snipes from the SQLite store. Engine.Run is shaped for it; needs a
  scheduler that picks the next ready snipe + a small RPC surface
  (Unix socket?) for `resy-snipe submit` / `resy-snipe list`.
- **EPIC: Notifier transports**: SMS via Twilio, iMessage via the
  AppleScript `/usr/bin/osascript` trick, Telegram bot. The interface
  is already there — these are pure plug-ins.
- **EPIC: Observed-release feedback UI**: surface the
  `observed_release_times` table to the user so the next snipe for
  the same venue gets a defaulted Explicit time without having to
  remember.
- **EPIC: Multi-provider**: a second `internal/<provider>/` package
  + adapter. OpenTable is the obvious target; the
  [opentable-mapping.md](opentable-mapping.md) exercise has the
  design notes.
- **EPIC: Cancel + status subcommands**: `resy-snipe cancel <id>`
  driving Submitted/Scheduled/Awaiting/Finding/Booking → Canceled.
  `resy-snipe status` listing live snipes from the store.

## How to file a new bead

```bash
bd create \
  --title="Short imperative summary" \
  --type=feature|task|bug \
  --priority=2 \
  --description="Why this issue exists and what needs to be done" \
  --acceptance="The observable outcome that means this is done"
```

For multi-issue work, file the epic first, then the children with
`bd dep add <child> <epic>` to record the blocking relationship.
