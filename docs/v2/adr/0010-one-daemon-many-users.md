# ADR 0010: One daemon process serves all friends-and-family on a box

**Status**: Accepted
**Date**: 2026-05-10
**Decision-makers**: phall
**Related**: [ADR-0005](0005-multi-user-data-model-from-day-one.md),
[ADR-0006](0006-sqlite-only-no-external-deps.md),
[ADR-0008](0008-secrets-sealed-at-rest-operator-key.md),
[design/multi-user.md](../design/multi-user.md)

## Context

Given multi-user from day one ([ADR-0005](0005-multi-user-data-model-from-day-one.md)),
two shapes are possible on a single box:

a. **One daemon, many users.** All tenants share one process, one
   `data.db`, one signer, one secrets passphrase. Logical
   multi-tenancy in code.
b. **One daemon per user.** Each tenant gets their own process,
   their own DB, their own port. Reverse proxy multiplexes by
   subdomain / path / token.

(b) is simpler to *reason* about ("can phall see james's data?
literally different processes, different files") but worse on every
operational dimension: N times the memory, N processes to keep alive,
N secrets passphrases to manage, N independent migration windows, N
fragmented audit logs.

## Decision

One daemon process serves all users on a given box. Tenancy is
logical: every Service-layer call carries a `UserID`, every DB query
joins on it, the audit log has a `user_id` column.

The operator (the unix user running `resy-snipe serve`) has admin
authority over all tenants on that box. They can list users, revoke
tokens, read the audit log, restore from backup. Tenants only see
their own data via the API.

## Consequences

**Positive**
- One process to keep alive. One systemd unit, one Docker container,
  one set of logs.
- One signer subprocess shared across tenants — the cold-start cost
  of a signer process is real, sharing it is the right move.
- One secrets passphrase / keyfile. The operator only manages a single
  unlock at boot.
- Audit log is unified — the operator can see "everyone's quests
  this week" in one query.
- Cross-tenant correlation possible: if two tenants both target
  Carbone Friday, the planner can detect contention and warn at
  plan-time rather than have them shoot each other in the foot at
  the booking race.

**Negative**
- A bug in tenancy scoping is a security incident. Mitigated by
  schema-level enforcement (every user-data table has a `user_id`
  column with a NOT NULL FK to `users`) and a `tenant_check.go` test
  pass that audits every Store query for a `user_id` filter.
- One process going down takes everyone down. Acceptable at
  friends-and-family scale; SLAs are best-effort.

**Neutral**
- Adding tenants is `INSERT INTO users …` plus an invite token
  ([ADR-0011](0011-operator-issued-invites-no-self-registration.md)).
  No new processes, no new ports, no new files.

## Alternatives considered

1. **One daemon per user.** *Rejected:* see Context. Operationally
   worse for our scale.
2. **One daemon, but with separate SQLite files per user.**
   *Rejected:* makes audit and cross-tenant features harder for no
   meaningful isolation gain. Logical scoping in one DB is the
   industry standard at this scale.
3. **Shared daemon, but each tenant gets their own subprocess for
   bookings (so a crashed booking takes down only one tenant).**
   *Rejected:* the engine is not crash-prone, and the cost is
   substantial complexity. Address crashes via tests, not
   sub-process isolation.

## Notes

**Cross-user account sharing is not in v1.** The data model permits
it (`accounts.shared_with` could be a future column), and the
operator can manually duplicate-login a Resy account into two users'
records. Both options are deliberately not exposed via API in v1.
Reasoning: shared accounts make rate-limiting nondeterministic
(account hits limit because user A snuck a quest in, user B's
midnight snipe fails) and audit murky. Add it explicitly in a future
ADR if a real need shows up.

`tenant_check.go` is a Go test (built with `go test ./internal/store/...`
under a `tenancy` build tag) that walks the `Store` interface, builds
fake calls with mismatched user IDs, and asserts each operation
either takes a `user_id` parameter or returns nothing. New Store
methods that don't satisfy the check don't compile. See
[design/multi-user.md](../design/multi-user.md) for the test contract.
