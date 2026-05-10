# Milestone M1 — Goal-driven local CLI + multi-user schema

**Status**: Scoped, not started
**Owner**: phall
**Target**: pre-cutover; this is the milestone where the contract changes
**Beads epic**: `resy-snipe-0li`
**Depends on**: nothing (greenfield against current `master`)
**Blocks**: M2, M3, M4, M5

## Goal in one sentence

Replace "user is the planner" with "user states a Goal; system resolves,
plans, and executes," with the multi-user data model in place from day
one so M2's daemonization is a transport concern, not a schema migration.

## What ships

A binary that, given a Resy URL or venue name, a target date, party
size, and time prefs, **computes when reservations drop and schedules
itself** without the user computing snipe-time. The CLI talks directly
to an in-process Service layer; M1 has no daemon yet.

User-visible surface after M1:

```
# Resolve a venue (read-only, no quest)
$ resy-snipe venue resolve https://resy.com/cities/washington-dc/venues/astoria-dc
venue: Astoria DC (resy:49716) · slug=astoria-dc city=washington-dc tz=America/New_York
release: 30 days ahead at 00:00 local (observed)

# Plan a quest — pure preview, no commit
$ resy-snipe quest plan https://resy.com/...?date=2026-06-09 --party=2 --time=18:30..21:00
PLAN
  venue:        Astoria DC (resy:49716)
  date:         2026-06-09 (Tue)
  party:        2
  time prefs:   18:30, 19:00, 19:30, 20:00, 20:30
  drop moment:  2026-05-10T00:00 EDT (28 days from now, Explicit)
  strategy:     Explicit (release time observed for this venue)
  signing:      probably not required (medium-demand window)
  hash:         a8f2…
  notes:        — observed_release_times has 4 prior samples within ±10s

# Create from a plan (auto-confirm with --yes; prompts otherwise)
$ resy-snipe quest create <url> ... --yes
quest qst_8xK3aZ created · scheduled for 2026-05-10T00:00 EDT

# List, get, cancel
$ resy-snipe quest list
$ resy-snipe quest get qst_8xK3aZ
$ resy-snipe quest cancel qst_8xK3aZ --reason="changed mind"

# User mgmt (operator only — single-user mode is the default)
$ resy-snipe user list
$ resy-snipe user invite james@example.com
```

The old monolithic invocation (`bin/resy-snipe -venue-id … -snipe-time …`)
is **removed** at the end of M1. No back-compat shim — v2 is the new
contract.

## What does NOT ship in M1

- **No daemon.** The CLI is the daemon (in-process Service). Crashes with
  the terminal. M2 fixes that.
- **No HTTP API.** Service is consumed in-process by the CLI only.
- **No MCP.** M3.
- **No secrets sealing.** Resy passwords live in plaintext in SQLite for
  M1, with a CRITICAL boot warning. M2 lands sealing.
- **No notifications beyond stdout.** v1's stdout notifier carries forward.
- **No TUI.** M4.

## Acceptance criteria

A1. `resy-snipe venue resolve <url|slug+city|name>` returns a
    `domain.Venue` with populated `ReleaseConfig`. URL parsing handles
    Resy's full-tracking-param URLs (verified against the conversation's
    Astoria DC URL).

A2. `resy-snipe quest plan` returns a `Plan` with a stable hash. Same
    Goal + same `now` + same Venue → same hash. Documented in
    `design/planner.md` §canonicalization.

A3. `resy-snipe quest create` requires either a `plan_hash` (from a
    prior `plan` invocation) or `--yes` (which inlines a fresh plan
    server-side and immediately commits). Mismatched hash returns
    `ErrInvalidPlanHash`.

A4. The Engine consumes the Planner-produced Intent **unchanged from v1**.
    No engine code edits other than the boot wiring.

A5. The DB schema includes `users`, `tokens`, `accounts`, `sessions`,
    `secrets`, `quests`, `intents`, `runs`, `events`, `audit_events`,
    `invites`, `venues_cache`, `name_search_cache`. All user-data tables
    have NOT NULL `user_id` FK. The `tenant_check.go` test pass enforces
    every Store method takes `user_id`. New Store method without it =
    test fails.

A6. Single-user mode is the default install: first run seeds one
    operator user, audit log records that fact. Adding a second user is
    `resy-snipe user invite <email>`.

A7. `resy-snipe quest list` for user A never returns user B's quests.
    Tested with a tenancy fuzz test that creates random quests across
    fake users and asserts isolation.

A8. End-to-end integration test: `Goal{url, date, party, prefs}`
    submitted via the Service layer produces a Quest that, when
    `time.Now()` (fake clock) reaches the drop moment, fires
    `/4/find` then PrepareSlot+Book against an httptest fixture and
    transitions to `Booked`.

A9. The v1 `cmd/resy-snipe/intent.go` venue catalog (Dead Rabbit,
    Rubirosa, …) is **removed**. The Resolver replaces it.

A10. `docs/getting-started.md` rewritten to match the new contract.

## Beads breakdown

Top-level feature: `M1 — goal-driven local CLI` (filed as a child of
`resy-snipe-0li`).

Sub-issues (filed in beads, dependencies wired):

| ID | Title | Depends on | Notes |
|---|---|---|---|
| M1-01 | Add `domain.Goal` type + `domain.Plan` shape | — | ADR-0001, ADR-0012 |
| M1-02 | `internal/resolver` — URL parser | M1-01 | URL form only |
| M1-03 | `internal/resolver` — `/3/venue` provider call | M1-02 | Extends `providers.Provider` interface |
| M1-04 | `internal/resolver` — `/3/venuesearch/search` fallback | M1-03 | Name-only path |
| M1-05 | `internal/resolver` — SQLite cache (`venues_cache`, `name_search_cache`) | M1-03 | Stale-on-failure semantics |
| M1-06 | `internal/planner` — strategy selection from venue + goal | M1-03 | Three rules per design/planner.md §3 |
| M1-07 | `internal/planner` — drop-moment math | M1-06 | Worked example test |
| M1-08 | `internal/planner` — slot expansion + plan-hash canonicalization | M1-07 | RFC 8785-style |
| M1-09 | `internal/service` — interface defined, methods stubbed | M1-01 | Per design/service-layer.md §interface |
| M1-10 | `internal/service` — wire Resolver, Planner, Engine, Store | M1-05, M1-08, M1-09 | Composition |
| M1-11 | `internal/service` — audit log writes per Service call | M1-10, M1-15 | Cross-link to design/observability.md |
| M1-12 | `internal/service` — idempotency-key support on Create/Cancel | M1-10 | 24h retention table |
| M1-13 | `internal/service` — streaming `SubscribeQuest` adapter | M1-10 | Wraps engine subscribe.go |
| M1-14 | Schema migration `0002_v2_multi_user.sql` | M1-01 | Adds users/tokens/accounts/quests/intents/runs/events/audit/invites |
| M1-15 | `tenant_check.go` test pass | M1-14 | Build tag `tenancy` |
| M1-16 | v1→v2 data migration script | M1-14 | Idempotent, preserves existing sessions |
| M1-17 | CLI `venue resolve` subcommand | M1-05 | Thin wrapper over Service.ResolveVenue |
| M1-18 | CLI `quest plan` subcommand | M1-08, M1-10 | Pretty-prints Plan |
| M1-19 | CLI `quest create` subcommand (with `--yes` and plan-hash pinning) | M1-18 | Interactive confirm |
| M1-20 | CLI `quest list/get/cancel` subcommands | M1-10 | |
| M1-21 | CLI `user list/invite/accept-invite` | M1-14 | First-user seeding logic |
| M1-22 | Remove v1 `cmd/resy-snipe/intent.go` venue catalog | M1-17 | Hard cutover |
| M1-23 | E2E integration test against httptest fixture | M1-19, M1-20 | Acceptance A8 |
| M1-24 | Rewrite `docs/getting-started.md` for v2 contract | M1-19, M1-21 | |

## Risks + open questions

- **Risk: `/3/venue` undocumented behavior.** Mitigated by fixture-based
  tests + record/replay against the live API for the venue catalog
  (Astoria DC, Carbone, Don Angie, Rubirosa) so we have ground truth.
- **Risk: Plan-hash canonicalization drift.** Mitigated by RFC 8785 +
  property tests + an explicit `plan_hash_version` envelope.
- **Open: Goal serialization format for `quests.goal_json`.** Lean:
  canonical JSON of the same shape used in the Plan hash, so replan
  naturally produces a comparable hash. Confirmed in design/planner.md
  §replan.
- **Open: how many record/replay venue fixtures.** Lean: 5
  (Astoria/Carbone/Don Angie/Rubirosa/Dead Rabbit) covering different
  release configs (30d/14d/lottery).

## Done definition

All sub-issues closed. Acceptance criteria A1–A10 demonstrably true. The
binary's `--help` output reflects the new subcommand surface.
`docs/getting-started.md` rewritten and the old top-level
`bin/resy-snipe -venue-id …` invocation no longer parses. v1 venue
catalog removed. `git push` clean. Beads epic `resy-snipe-0li` reflects
M1 closed.
