# ADR 0003: Daemon is the system; CLI is a thin client

**Status**: Accepted
**Date**: 2026-05-10
**Decision-makers**: phall
**Related**: [ADR-0004](0004-mcp-as-peer-front-end.md),
[ADR-0009](0009-reverse-proxy-native-http.md),
[design/daemon.md](../design/daemon.md),
[design/service-layer.md](../design/service-layer.md)

## Context

A snipe is a fundamentally repeated, long-lived activity. A user wants
to "watch for a Carbone 4-top in the next two months" and walk away.
The v1 binary is a foreground process the user must keep alive — that
loses every time the laptop sleeps, the battery dies, or the user
quits the terminal.

The fix is not "make the CLI more durable" (cron + pid files +
restart loops are a tarball of failure modes). The fix is to put the
durability somewhere designed for it: a daemon process, owned by an
operator, with persistent state.

## Decision

The daemon (`resy-snipe serve`) **is** the system. The CLI
(`resy-snipe quest add …`, `resy-snipe quest list`, …) is a thin
HTTP client that hits the daemon's Service layer. Likewise the MCP
server ([ADR-0004](0004-mcp-as-peer-front-end.md)).

There is no "standalone CLI" mode in v2. Every CLI invocation either
(a) hits a running daemon over HTTP, or (b) prints a clear error
asking the operator to start one. The CLI does not contain a copy of
the engine.

## Consequences

**Positive**
- Quests survive everything: laptop sleep, terminal close, daemon
  restarts (state is in SQLite, daemon resumes on boot).
- One process owns the SQLite file → no multi-writer contention.
- One process owns the signer subprocess → no startup-cost duplication.
- Notifications, scheduling, audit logging all happen in one place.
- The CLI is trivially scriptable from any machine that can reach the
  daemon over Tailscale / VPN / localhost.

**Negative**
- "I just want to do a one-shot" requires `serve` to be running.
  Mitigated by making `serve` cheap to start and `systemctl enable`
  one command.
- Two binaries' worth of code (transport + service vs. embedded). In
  practice ~one transport package and a thin client, not actually two
  binaries (same binary, different subcommand).
- Operator now has a process to babysit. Mitigated by single-binary,
  no-deps deployment ([ADR-0006](0006-sqlite-only-no-external-deps.md))
  and shipped systemd/Docker artifacts.

**Neutral**
- The Engine code is unchanged — it just runs inside the daemon
  instead of inside the CLI process.

## Alternatives considered

1. **Keep CLI standalone, add a `--daemon` mode.** *Rejected:* same
   code paths must work in both modes; the abstraction tax is
   permanent. "It works either way" means "it works neither way well."
2. **Make the CLI a wrapper that auto-starts a daemon if none is
   running.** *Rejected:* daemons started from random shell sessions
   inherit weird environments, get killed when the terminal closes,
   and confuse the operator about who owns what. If you want a
   daemon, run `systemctl start resy-snipe`.
3. **Pure CLI, no daemon, persist quests in SQLite, run via cron.**
   *Rejected:* cron's granularity is per-minute; we need
   sub-second precision for midnight drops. Cron also doesn't solve
   long-lived continuous-poll quests.

## Notes

The CLI does not have its own "embedded engine fallback." If the
daemon isn't reachable, the CLI prints an error pointing at
[design/daemon.md](../design/daemon.md) ("start the daemon with
`resy-snipe serve` or `systemctl start resy-snipe`"). This is a
deliberate sharp edge — the alternative is two ways to do everything
and double the test surface.

For local development, `resy-snipe serve --foreground --dev` runs the
daemon attached to the terminal, with verbose logs and an
unprivileged secrets passphrase prompt — the dev-loop equivalent of
v1's standalone binary.
