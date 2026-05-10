# ADR 0006: SQLite WAL is the only datastore

**Status**: Accepted
**Date**: 2026-05-10
**Decision-makers**: phall
**Related**: [ADR-0007](0007-self-hosted-only-no-saas.md),
[ADR-0010](0010-one-daemon-many-users.md),
[design/daemon.md](../design/daemon.md)

## Context

A homelab daemon must be cheap to operate. Every external dependency
is a new failure mode the operator has to babysit (Postgres unhealthy,
Redis OOM, message broker upgrade, …). At friends-and-family scale (≤
~50 users, ≤ ~thousands of quests/year), there's no read/write volume
that requires anything beyond an embedded store.

## Decision

SQLite (in WAL mode, with `busy_timeout=5000`) is the only datastore.
No Redis, no Postgres, no message broker, no separate cache. Quest
state, audit log, sessions, sealed secrets — all in one `data.db`.

The Store is single-writer (the daemon process — see
[ADR-0010](0010-one-daemon-many-users.md)). The CLI never opens the
DB file; it goes through the daemon over HTTP.

## Consequences

**Positive**
- Backup is `cp data.db data.db.bak` (or `litestream replicate` for
  continuous). One file, one truth.
- No service discovery. No connection pool. No "is the broker up?"
  on the boot path.
- Tests run against the same engine that ships in prod. No
  Postgres-only feature creep.
- Migration tooling is already established (`internal/store/migrations`).

**Negative**
- One process can write at a time. With WAL, readers don't block
  writers, but two daemon instances would conflict — enforced by a
  daemon-startup file lock.
- Cross-machine clustering is not a path. Friends-and-family and
  homelab don't need it; if anyone ever does, it's a new ADR and
  probably a new project.
- BLOB / large-text columns are constrained. Audit log entries stay
  small (no full request/response capture in the DB; that's a logs
  concern).

**Neutral**
- The Store interface ([internal/store](../../../internal/store/))
  hides SQLite specifics. Migrating to Postgres would be a single-PR
  rewrite of one package, *if* that day ever comes. We don't design
  for it.

## Alternatives considered

1. **SQLite + Redis for pubsub** (so events fan out to multiple HTTP
   subscribers cheaply). *Rejected:* in-process channels are simpler
   and sufficient. Daemon is single-process; subscribers are local.
   Redis adds an operator concern for zero gain.
2. **Postgres because "it scales."** *Rejected:* premature. SQLite at
   this scale handles thousands of writes/sec single-threaded — far
   beyond our load profile. Postgres adds a tarball of operational
   choices (backup tool, version, replication, connection pool, TLS).
3. **Bolt / Badger / embedded KV.** *Rejected:* SQLite already gives
   us SQL, transactions, joins, migrations, FTS, JSON columns, and a
   universally understood backup story. The KV stores trade familiarity
   for nothing.

## Notes

WAL config defaults the daemon enforces at boot:

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
```

Failure to set any of these is a daemon startup error, not a warning.
See [internal/store/migrations](../../../internal/store/migrations/)
for migration conventions and [design/daemon.md](../design/daemon.md)
for the boot sequence.

`data.db` location follows XDG: `$XDG_DATA_HOME/resy-snipe/data.db`,
overridable by `--data-dir` or `RESY_SNIPE_DATA_DIR`.
