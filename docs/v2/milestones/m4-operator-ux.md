# Milestone M4 — Operator UX & TUI

**Status**: Scoped (sub-issues materialize when M3 closes)
**Owner**: phall
**Beads epic**: `resy-snipe-0li`
**Depends on**: M2 (HTTP API), M3 (MCP — TUI talks via HTTP, not MCP)
**Blocks**: nothing

## Goal in one sentence

Make the system pleasant for the operator (phall) and his friends to
actually use day-to-day: a TUI dashboard for humans, push notifications
when quests fire, and a `doctor` subcommand that diagnoses everything
at once.

## What ships

- `resy-snipe ui` — Bubble Tea TUI. Lists quests, shows live event
  stream, lets the user create/cancel without leaving the terminal.
  Talks to the daemon over HTTP.
- `resy-snipe doctor` — diagnostic subcommand per
  `design/observability.md` §doctor. Standalone (doesn't require
  daemon running); reports DB integrity, schema, secrets, signer,
  Resy connectivity, clock skew.
- `resy-snipe user disable/reset/list-sessions` admin commands
  (operator UX gaps from M1's invite flow).
- Notifier impls: stdout (existing), Pushover, ntfy.sh, Slack webhook,
  email (SMTP). Selectable per-user via Service.
- Email templates and message formatting for booked / failed / expired
  quests.

## What does NOT ship in M4

- A web UI (out of scope per `docs/v2/README.md`).
- SMS notifications (Twilio/etc — adds a paid dep, declined).
- Mobile app.
- Pretty interactive plan-confirmation UX (CLI prompt is sufficient).

## Acceptance criteria

- A user can run `resy-snipe ui`, see all their quests, drill into one,
  watch live events, and cancel — without typing another command.
- Quest completion fires a notification via at least Pushover and ntfy.
- `resy-snipe doctor` exits 0 on a healthy install and surfaces the
  specific failing subsystem on a broken one.
- `user disable` revokes all of that user's tokens and prevents future
  Service calls; existing in-flight quests complete (do not get
  killed).

## Cross-references

- ADRs: 0003, 0011
- Design: daemon.md, observability.md, multi-user.md
