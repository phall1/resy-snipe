# ADR 0005: Multi-user data model lands in M1, not later

**Status**: Accepted
**Date**: 2026-05-10
**Decision-makers**: phall
**Related**: [ADR-0007](0007-self-hosted-only-no-saas.md),
[ADR-0010](0010-one-daemon-many-users.md),
[ADR-0011](0011-operator-issued-invites-no-self-registration.md),
[design/multi-user.md](../design/multi-user.md)

## Context

The eventual deployment ([ADR-0007](0007-self-hosted-only-no-saas.md))
is friends-and-family on a homelab box: phall and ~5 others, plus
james potentially running his own instance. That means real
multi-tenancy: per-user passwords, per-user audit, per-user quests.

The naive path is "ship single-user, add multi-user when we need it."
That path is a tarball: every layer (auth, store, planner, MCP,
notifier) is built around the assumption "the only actor is the
operator," and retrofitting tenancy means schema migrations, query
rewrites, and re-auditing every code path for "did I scope this to
the user?" bugs.

## Decision

Multi-user lands in M1, before any front-end is built. Concretely, the
schema includes from day one:

```
users        — homelab tenants (phall, james, ...)
accounts     — Resy logins, owned by a user
sessions     — JWT bag, owned by an account (exists today)
quests       — owned by a user, references an account
audit_events — every Service call writes one row
```

The CLI in M1 implicitly scopes to "the operator's user" (single seeded
row), but every Service call carries an explicit `UserID` and every
DB query joins on it. There is no global mutation in v2.

## Consequences

**Positive**
- M2 (daemonize + auth) is a transport concern only. Adding a bearer
  token doesn't require touching domain or store code.
- Per-user audit and per-user notification are free — the columns are
  already there.
- Adding james as a second user in M2 is `INSERT INTO users …` plus
  a token, not a schema migration.

**Negative**
- M1's `cmd/` glue is slightly more verbose: every call passes a
  `UserID` even when there's only one user. A few extra columns in
  M1 schema we won't read until M2.
- "Single-user mode" is a deployment shape, not a code path —
  operators who run one user pay a tiny tax (~1 extra row in 4 tables)
  for the option to add more.

**Neutral**
- v1 schema (`users`, `sessions` tables) already exists. M1 extends
  it; doesn't replace it. The migration is additive.

## Alternatives considered

1. **Single-user in M1, retrofit multi-user in M2.** *Rejected:* every
   query rewrite is a place to introduce a tenancy bug. "phall sees
   james's quest" once would be a credibility-destroying incident with
   friends-and-family.
2. **A "tenant" abstraction with optional cross-tenant grants.**
   *Rejected:* over-engineered for v1. Add per-user grants in v2 if
   anyone asks for "share this Resy account with my partner." Most
   users won't.
3. **Schema-per-user (separate SQLite file per user).** *Rejected:*
   makes audit-across-users impossible and cross-user concerns (rate
   limits per Resy account that two users share, cross-quest
   notifications) much harder. Logical multi-tenancy in one file is
   the standard answer at this scale.

## Notes

The line between "user" (homelab tenant, identified by email) and
"account" (Resy login, identified by Resy email) is load-bearing:

- A user has ≥1 accounts (phall might have a personal and a work Resy
  login).
- An account is owned by exactly one user in v1 (no sharing —
  [ADR-0010](0010-one-daemon-many-users.md) Notes).
- Quests reference both: `quest.user_id` (who created it, who gets
  notified) and `quest.account_id` (whose Resy creds book it).

Authentication is per-user; rate-limiting and idempotency are
per-account. The Planner uses `quest.account_id` to scope its
`PerAccountRateLimiter` lookup. See
[design/multi-user.md](../design/multi-user.md) for schema and query
contracts.
