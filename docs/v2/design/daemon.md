# Daemon

The daemon (`resy-snipe serve`) **is** the system. Per
[ADR-0003](../adr/0003-daemon-first-cli-as-client.md), the CLI and the
MCP server ([ADR-0004](../adr/0004-mcp-as-peer-front-end.md)) are HTTP
clients of the daemon. The daemon owns:

- the Service layer (one verb per public operation — see
  [service-layer.md](service-layer.md));
- the Engine (state machine, scheduler, booking race);
- the Store (single-writer SQLite, [ADR-0006](../adr/0006-sqlite-only-no-external-deps.md));
- the sealed secrets ([ADR-0008](../adr/0008-secrets-sealed-at-rest-operator-key.md));
- the signer subprocess ([anti-bot.md](../../anti-bot.md));
- the audit log ([ADR-0010](../adr/0010-one-daemon-many-users.md)).

This document defines the boot sequence, the HTTP transport, the
deployment shapes, and the operator contract. Everything else hangs off
those.

---

## 1. Process model

One binary, multiple subcommands. The subcommand selects mode; nothing
else does.

| Subcommand            | Mode             | Talks to       |
|-----------------------|------------------|----------------|
| `resy-snipe serve`    | Daemon           | SQLite, signer |
| `resy-snipe quest …`  | CLI client       | Daemon (HTTP)  |
| `resy-snipe user …`   | Operator admin   | Daemon (HTTP)  |
| `resy-snipe mcp`      | MCP server (M3)  | Daemon (HTTP)  |
| `resy-snipe version`  | Stateless        | nothing        |

Rules:

1. Only `serve` opens `data.db`. Every other subcommand reaches the
   daemon over HTTP. Per [ADR-0006](../adr/0006-sqlite-only-no-external-deps.md),
   "the CLI never opens the DB file."
2. There is no embedded-engine fallback in the CLI. If the daemon is
   unreachable, the CLI prints an error pointing at this document.
3. Operator admin (`user add`, `user revoke-token`, `secrets-rotate`)
   is a privileged client — it requires an operator-tier token, but it
   is still a client. The daemon enforces authority; the subcommand is
   just a UI.
4. `mcp` is a peer front-end. It does not embed the engine; it shells
   to the same Service layer the CLI does. See
   [ADR-0004](../adr/0004-mcp-as-peer-front-end.md).

The Engine and Store packages are imported only by `serve` and by
tests. A `cmd/resy-snipe/serve_only.go` build-tag check enforces this
at build time — `quest`, `mcp`, and `user` subcommands cannot be
linked against `internal/engine` or `internal/store`.

---

## 2. Boot sequence

The daemon boots in a strict order. Each step is fail-fast: any error
exits non-zero with a single-line cause and a pointer to the relevant
section of this document. No step is "best effort." If a step fails,
the daemon does not start.

```
  ┌──────────────────────────────────────────────────────────────┐
  │ resy-snipe serve                                             │
  └────────────────────────────┬─────────────────────────────────┘
                               │
                               ▼
        ┌─────────────────────────────────────────────────┐
   (1)  │ Parse flags + env + config file                 │
        │ precedence: flag > env > config > default       │
        └────────────────────────┬────────────────────────┘
                                 ▼
        ┌─────────────────────────────────────────────────┐
   (2)  │ Acquire SQLite-file lock (flock on data.db)     │
        │ → if held: exit 2, "another daemon owns it"     │
        └────────────────────────┬────────────────────────┘
                                 ▼
        ┌─────────────────────────────────────────────────┐
   (3)  │ Open SQLite WAL, set PRAGMAs (ADR-0006)         │
        └────────────────────────┬────────────────────────┘
                                 ▼
        ┌─────────────────────────────────────────────────┐
   (4)  │ Run pending migrations                          │
        │ → if schema newer than binary: exit 3           │
        └────────────────────────┬────────────────────────┘
                                 ▼
        ┌─────────────────────────────────────────────────┐
   (5)  │ Unlock secrets (passphrase prompt | keyfile)    │
        │   ADR-0008                                      │
        └────────────────────────┬────────────────────────┘
                                 ▼
        ┌─────────────────────────────────────────────────┐
   (6)  │ Start signer subprocess (if RESY_SNIPE_SIGNER_BIN)│
        └────────────────────────┬────────────────────────┘
                                 ▼
        ┌─────────────────────────────────────────────────┐
   (7)  │ Start Engine; resume in-flight quests           │
        └────────────────────────┬────────────────────────┘
                                 ▼
        ┌─────────────────────────────────────────────────┐
   (8)  │ Bind HTTP listener (127.0.0.1:port default)     │
        │   ADR-0009                                      │
        └────────────────────────┬────────────────────────┘
                                 ▼
        ┌─────────────────────────────────────────────────┐
   (9)  │ Print boot banner; signal "ready" to systemd    │
        └─────────────────────────────────────────────────┘
```

### 2.1 Parse configuration

Sources, in precedence order:

1. Command-line flags (`--bind`, `--data-dir`, `--config`, …).
2. Environment variables (`RESY_SNIPE_*`).
3. Config file (`--config <path>`, else
   `$XDG_CONFIG_HOME/resy-snipe/config.toml`, else
   `/etc/resy-snipe/config.toml`).
4. Compiled-in defaults.

Parsing has no side effects. Validation errors print all violations,
not just the first. See [§3](#3-config-sources) for the schema.

### 2.2 Acquire SQLite-file lock

The daemon acquires an OS file lock (`flock` on Linux/macOS) on
`data.db.lock` adjacent to `data.db`. If the lock is held, the daemon
exits with code `2` and message `another daemon already owns
<data.db>`. This enforces single-writer semantics
([ADR-0006](../adr/0006-sqlite-only-no-external-deps.md)) — two
`serve` processes cannot race the SQLite WAL.

### 2.3 Open SQLite WAL

The daemon opens `data.db` with the modernc driver and immediately
applies the PRAGMA set required by
[ADR-0006](../adr/0006-sqlite-only-no-external-deps.md):

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous   = NORMAL;
PRAGMA busy_timeout  = 5000;
PRAGMA foreign_keys  = ON;
```

If any PRAGMA fails to set (e.g. `journal_mode` returns something
other than `wal`), boot aborts. Warnings are not acceptable; the
daemon refuses to run with a misconfigured DB.

### 2.4 Migrations

The daemon runs `store.Migrate(ctx, db)`
([internal/store/migrate.go](../../../internal/store/migrate.go)) over
the embedded `migrations/` directory. Each migration is a numbered SQL
file applied inside its own transaction.

If `data.db` reports a `schema_migrations.version` higher than any
embedded migration, the daemon exits with code `3` and message
`schema is newer than this binary; downgrade refused`. This is the
upgrade contract: an older binary will never silently run against a
newer schema. See [§12](#12-upgrade-story).

### 2.5 Unlock secrets

Per [ADR-0008](../adr/0008-secrets-sealed-at-rest-operator-key.md):

- **Default**: prompt the operator on stdin for the passphrase. The
  daemon reads via a no-echo terminal and runs Argon2id (m=64MB, t=3,
  p=1 by default; tunable in config) to derive the unwrap key.
- **`--keyfile <path>`**: read the unwrap key from the file. The file
  is `mmap`'d, the bytes are mlocked, and the file is never written
  to. Operators can point this at an `age` identity, a tmpfs path
  populated by `LoadCredential=`, or a `1password run --` shell.
- **`RESY_SNIPE_PASSPHRASE`**: read the passphrase from the env var
  (for systemd `LoadCredential=` and Docker secrets). The daemon zeros
  the env var after reading.
- **`--insecure-no-encryption`** (dev only): skip secrets unwrap. The
  daemon refuses this if `RESY_SNIPE_PROD=1` is set or if there is no
  TTY attached and `--foreground` was not passed
  ([ADR-0008 §Notes](../adr/0008-secrets-sealed-at-rest-operator-key.md#notes)).

The derived key never leaves process memory. It is mlocked, wiped on
`os.Exit`, and never logged. The boot banner reports `secrets:
unlocked` or `secrets: dev-mode (insecure)` so the operator can audit
what they got.

### 2.6 Start signer subprocess

If `RESY_SNIPE_SIGNER_BIN` is set, the daemon starts the signer per
[anti-bot.md](../../anti-bot.md). The signer's stdout/stderr is
plumbed into the daemon's structured log under
`component=signer`. If the signer fails to start, the daemon emits a
warning and falls back to the Noop signer — booking quality may
degrade but the daemon keeps serving.

If `RESY_SNIPE_SIGNER_BIN` is unset, the daemon installs the Noop
signer silently. This is the same behavior as v1.

### 2.7 Start engine; resume in-flight quests

The Engine starts with a snapshot of all quests in resumable status:

```
status IN ('Submitted', 'Scheduled', 'Awaiting', 'Discovering')
```

These quests are passed to `engine.Resume(ctx, quest)`, which
re-arms `Clock.AfterFunc` for `Scheduled` quests, restarts polling
for `Discovering`/`Awaiting`, and emits a `daemon.resumed` event for
each.

Quests in `Finding` or `Booking` at boot time were interrupted by the
previous daemon's shutdown — see [§8](#8-resume-semantics).

### 2.8 Bind HTTP listener

Per [ADR-0009](../adr/0009-reverse-proxy-native-http.md), the daemon
binds to `127.0.0.1:7765` by default. `--bind 0.0.0.0:7765` opts the
operator into network exposure and prints a startup warning that no
TLS is in front. The daemon does not terminate TLS itself.

### 2.9 Boot banner

The daemon prints a single multi-line banner to stdout (and to the
structured log) before signaling readiness. The banner is the
operator's audit trail for "what did this process actually start as":

```
resy-snipe 2.0.0 (commit abc1234, schema v9)
  bind:        127.0.0.1:7765
  data-dir:    /var/lib/resy-snipe
  config:      /etc/resy-snipe/config.toml
  signer:      subprocess (px-signer v0.4.1)
  secrets:     unlocked (Argon2id, key-version=2)
  users:       4 active, 1 disabled
  resumed:     7 quests (3 Scheduled, 4 Discovering)
ready.
```

After printing, the daemon calls `sd_notify(READY=1)` (no-op on
non-systemd) and begins accepting requests.

---

## 3. Config sources

Config lives in TOML at one of:

1. `$XDG_CONFIG_HOME/resy-snipe/config.toml` (per-user, default for
   `resy-snipe serve --foreground`).
2. `/etc/resy-snipe/config.toml` (system, default for the systemd
   unit).
3. `--config <path>` (explicit override).

Every key has a flag and an env-var equivalent. Precedence is flag >
env > config > default.

### 3.1 Sample config

```toml
# /etc/resy-snipe/config.toml

[daemon]
bind          = "127.0.0.1:7765"
data_dir      = "/var/lib/resy-snipe"
log_format    = "json"           # "json" | "text"
log_level     = "info"           # "debug" | "info" | "warn" | "error"
shutdown_drain_seconds = 15

[secrets]
mode          = "passphrase"     # "passphrase" | "keyfile" | "insecure"
keyfile       = ""               # absolute path; mode=keyfile only
argon2id_memory_kb = 65536
argon2id_time      = 3
argon2id_parallel  = 1

[http]
base_path     = "/"
trusted_proxies = [
  "127.0.0.0/8",
  "::1/128",
  "10.0.0.0/8",
  "172.16.0.0/12",
  "192.168.0.0/16",
]
external_url  = "https://resy-snipe.example.com"

[signer]
bin           = ""               # path to signer binary; empty=Noop
restart_backoff_seconds = 5

[engine]
max_concurrent_bookings = 8
default_lookahead_days  = 60

[notify]
# see design/notify.md
```

### 3.2 Key reference

| Key                              | Flag                  | Env                          | Default                     |
|----------------------------------|-----------------------|------------------------------|-----------------------------|
| `daemon.bind`                    | `--bind`              | `RESY_SNIPE_BIND`            | `127.0.0.1:7765`            |
| `daemon.data_dir`                | `--data-dir`          | `RESY_SNIPE_DATA_DIR`        | `$XDG_DATA_HOME/resy-snipe` |
| `daemon.log_format`              | `--log-format`        | `RESY_SNIPE_LOG_FORMAT`      | `json`                      |
| `daemon.log_level`               | `--log-level`         | `RESY_SNIPE_LOG_LEVEL`       | `info`                      |
| `daemon.shutdown_drain_seconds`  | `--shutdown-drain`    | `RESY_SNIPE_SHUTDOWN_DRAIN`  | `15`                        |
| `secrets.mode`                   | `--secrets-mode`      | `RESY_SNIPE_SECRETS_MODE`    | `passphrase`                |
| `secrets.keyfile`                | `--keyfile`           | `RESY_SNIPE_KEYFILE`         | (empty)                     |
| `secrets.argon2id_*`             | `--argon2-{mem,time,parallel}` | `RESY_SNIPE_ARGON2_*` | m=64MB t=3 p=1            |
| `http.base_path`                 | `--base-path`         | `RESY_SNIPE_BASE_PATH`       | `/`                         |
| `http.trusted_proxies`           | `--trusted-proxies`   | `RESY_SNIPE_TRUSTED_PROXIES` | RFC1918 + loopback          |
| `http.external_url`              | `--external-url`      | `RESY_SNIPE_EXTERNAL_URL`    | (none)                      |
| `signer.bin`                     | `--signer-bin`        | `RESY_SNIPE_SIGNER_BIN`      | (empty → Noop)              |
| `signer.restart_backoff_seconds` | `--signer-backoff`    | `RESY_SNIPE_SIGNER_BACKOFF`  | `5`                         |
| `engine.max_concurrent_bookings` | `--max-bookings`      | `RESY_SNIPE_MAX_BOOKINGS`    | `8`                         |
| `engine.default_lookahead_days`  | `--lookahead-days`    | `RESY_SNIPE_LOOKAHEAD_DAYS`  | `60`                        |

Unknown keys are an error, not a warning. The daemon refuses to start
with `unknown config key foo.bar at line 12`.

---

## 4. HTTP transport contract

### 4.1 Routes

One HTTP route per Service-layer verb. See
[service-layer.md](service-layer.md) for the verb semantics and
sentinel-error mapping.

| Method | Path                                | Verb                          | Auth     |
|--------|-------------------------------------|-------------------------------|----------|
| POST   | `/v1/venues/resolve`                | `Service.ResolveVenue`        | bearer   |
| POST   | `/v1/quests/plan`                   | `Service.PlanQuest`           | bearer   |
| POST   | `/v1/quests`                        | `Service.SubmitQuest`         | bearer   |
| GET    | `/v1/quests`                        | `Service.ListQuests`          | bearer   |
| GET    | `/v1/quests/{id}`                   | `Service.GetQuest`            | bearer   |
| DELETE | `/v1/quests/{id}`                   | `Service.CancelQuest`         | bearer   |
| GET    | `/v1/quests/{id}/events`            | `Service.SubscribeEvents` (SSE) | bearer |
| POST   | `/v1/auth/tokens`                   | `Service.IssueToken`          | operator |
| DELETE | `/v1/auth/tokens/{id}`              | `Service.RevokeToken`         | operator |
| GET    | `/v1/users`                         | `Service.ListUsers`           | operator |
| POST   | `/v1/users`                         | `Service.CreateUser`          | operator |
| GET    | `/healthz`                          | liveness                      | none     |
| GET    | `/readyz`                           | readiness                     | optional |
| GET    | `/metrics`                          | Prometheus                    | optional |
| GET    | `/debug/pprof/*`                    | runtime profiling             | loopback |

The route table is the only place HTTP and Service couple; adding a
verb means adding one row plus a handler. The handler is a thin
adapter: parse JSON → call Service → map error → write JSON.

### 4.2 Authentication

`Authorization: Bearer <token>`. Tokens are opaque, generated by the
daemon, and stored hashed in the `tokens` table:

```sql
CREATE TABLE tokens (
  id          TEXT PRIMARY KEY,         -- ULID, public id
  user_id     TEXT NOT NULL REFERENCES users(id),
  hash        BLOB NOT NULL,            -- BLAKE2b(token)
  scopes      TEXT NOT NULL,            -- 'user' | 'operator'
  label       TEXT NOT NULL,            -- operator-set, e.g. "phall-laptop"
  created_at  INTEGER NOT NULL,
  last_seen   INTEGER,
  revoked_at  INTEGER
);
```

Token issuance returns the bearer string exactly once
(`tok_<base32-of-32-random-bytes>`). The daemon stores only the hash.
Lost tokens are revoked + reissued, never recovered.

The `operator` scope is required for `/v1/users` and `/v1/auth/*`
admin endpoints. The `user` scope can read and modify only its own
user_id's resources — enforced in the Service layer, not the
transport. See [multi-user.md](multi-user.md) for the tenancy model.

### 4.3 Errors

Every error response is the same shape:

```json
{
  "code":    "quest_not_found",
  "message": "no quest with id qst_01HXAB… for user usr_01HW…",
  "details": { "quest_id": "qst_01HXAB…" }
}
```

`code` is a stable string keyed off the sentinel error in
`internal/providers` and `internal/service`. The HTTP status mapping
lives in [service-layer.md](service-layer.md#error-mapping). The
short version:

| Sentinel                             | HTTP | Code                       |
|--------------------------------------|------|----------------------------|
| `service.ErrUnauthenticated`         | 401  | `unauthenticated`          |
| `service.ErrForbidden`               | 403  | `forbidden`                |
| `service.ErrNotFound`                | 404  | `not_found`                |
| `service.ErrConflict`                | 409  | `conflict`                 |
| `service.ErrValidation`              | 422  | `validation_failed`        |
| `providers.ErrRateLimited`           | 429  | `rate_limited`             |
| `providers.ErrSessionExpired`        | 502  | `provider_session_expired` |
| `providers.ErrUpstreamUnavailable`   | 502  | `provider_unavailable`     |
| any other                            | 500  | `internal`                 |

`details` is JSON; its shape is per-code and documented alongside
each Service verb. The daemon never writes raw upstream HTML or
provider error bodies into `details`.

### 4.4 Versioning

URL prefix is the version: `/v1/`. Breaking wire changes ⇒ `/v2/`.
The daemon may serve `/v1` and `/v2` simultaneously during a
deprecation window; routes mounted at both prefixes share the same
handler when behavior is unchanged. Adding a field to a response is
not breaking; removing one is.

The `X-Resy-Snipe-Version` response header carries the daemon
version. Clients log it for support but do not branch on it.

### 4.5 Server-Sent Events

`GET /v1/quests/{id}/events` returns `text/event-stream`. Each event
is a JSON-encoded `domain.Event`. The connection is held open; on
quest terminal status (`Booked`, `Failed`, `Cancelled`) the daemon
sends a final event and closes the stream. Reconnection is the
client's responsibility — the daemon does not maintain per-client
state across reconnects.

The CLI uses SSE for `resy-snipe quest watch <id>`. The MCP server
uses it for the `quest.events` resource (M3).

---

## 5. Trusted-proxy CIDR list

Per [ADR-0009](../adr/0009-reverse-proxy-native-http.md), the daemon
trusts `X-Forwarded-Proto`, `X-Forwarded-Host`, and
`X-Forwarded-For` headers **only** when the immediate peer's source
IP falls in the trusted-proxy list.

Default list:

```
127.0.0.0/8
::1/128
10.0.0.0/8
172.16.0.0/12
192.168.0.0/16
fc00::/7
```

Override with `--trusted-proxies 10.0.0.0/8,172.16.0.0/12` or the
config-file `http.trusted_proxies` array. An empty list means "trust
no proxy" — every request is treated as direct, and `X-Forwarded-*`
headers are ignored. This is the right setting for `--bind 0.0.0.0`
with no proxy in front.

The trusted-proxy decision affects:

1. **Audit-log client IP**: with a trusted proxy, the audit log
   records the leftmost `X-Forwarded-For`. Without, it records the
   TCP peer.
2. **Rate-limit keying**: same.
3. **`Location:` and email link generation**: proto/host come from
   `X-Forwarded-Proto`/`X-Forwarded-Host` if trusted, else from the
   request target. Operators wanting deterministic external URLs
   should set `http.external_url`.

Misconfiguring this is the most common deployment bug — the audit
log fills with `127.0.0.1` when the operator forgets to add their
proxy network. The `/readyz` body includes `trusted_proxies` so
operators can verify.

---

## 6. Healthchecks

| Endpoint    | Auth     | Checks                                                                                  |
|-------------|----------|-----------------------------------------------------------------------------------------|
| `/healthz`  | none     | Process is up. Returns `200 ok\n`.                                                      |
| `/readyz`   | optional | DB writable, signer responsive, secrets unlocked, engine running. Returns JSON details. |

`/healthz` is intentionally cheap — it exists so reverse proxies and
orchestrators can check liveness without auth or DB access. It
returns `200` as long as the HTTP server is serving requests.

`/readyz` is the deeper check:

```json
{
  "version":         "2.0.0",
  "schema_version":  9,
  "db_writable":     true,
  "secrets":         "unlocked",
  "signer":          "ok",
  "engine":          "running",
  "users_active":    4,
  "quests_resumed":  7,
  "trusted_proxies": ["127.0.0.0/8", "10.0.0.0/8"]
}
```

Each subcheck has a budget (DB ping ≤ 100ms, signer ping ≤ 200ms).
If a subcheck times out or fails, `/readyz` returns `503` with the
failing fields populated. `/healthz` keeps returning `200` — process
liveness is a different question from readiness.

`/readyz` auth is optional: without a token the response omits
counts (`users_active`, `quests_resumed`) so the endpoint cannot be
used to enumerate the system from the outside.

---

## 7. Graceful shutdown

The daemon handles `SIGTERM` and `SIGINT` identically. `SIGKILL` is
the operator's failure mode — there is no clean response to it.

On signal:

1. Stop accepting new HTTP connections (`net/http.Server.Shutdown`).
   Existing connections drain.
2. Set Engine to "no new work" — `Submit` returns
   `service.ErrShuttingDown`. In-flight quests continue.
3. Wait up to `daemon.shutdown_drain_seconds` (default 15) for the
   Engine to settle. The Engine reports goroutine count via the
   structured log every second during drain.
4. **Detach in-flight `/3/book` calls**: per
   [laws.md §12](../../laws.md#concurrency) and the engine's
   `context.WithoutCancel` pattern, an in-flight Book request is not
   cancelled by daemon shutdown — letting it finish minimizes the
   "we double-charged a card and the daemon doesn't know" failure
   mode. The Book result is written to the DB before the goroutine
   exits even if HTTP is already torn down.
5. Close the SQLite connection. WAL checkpoints automatically.
6. Release the file lock.
7. Exit `0`.

If drain exceeds the deadline, the daemon logs
`shutdown_drain_exceeded` with the goroutine count and exits `0`
anyway — the OS will clean up. The next boot's Resume step
([§8](#8-resume-semantics)) handles any quests that were mid-flight.

The systemd unit ships with `TimeoutStopSec=30s` to give the daemon
its drain budget plus a safety margin.

---

## 8. Resume semantics

On boot, after migrations and engine start, the daemon scans:

```sql
SELECT id, user_id, status FROM quests
WHERE status IN ('Submitted', 'Scheduled', 'Awaiting', 'Discovering',
                 'Finding', 'Booking');
```

Then:

| Status         | Action on resume                                                         |
|----------------|--------------------------------------------------------------------------|
| `Submitted`    | Re-enqueue. Engine picks up as if newly submitted.                       |
| `Scheduled`    | Re-arm `Clock.AfterFunc(release_at - now)`. If `release_at < now`, fire immediately. |
| `Awaiting`     | Resume continuous-poll loop.                                             |
| `Discovering`  | Resume discovery poll loop.                                              |
| `Finding`      | Emit `daemon.interrupted` event, transition to `Failed` with reason `daemon_restart_during_finding`. |
| `Booking`      | Emit `daemon.interrupted` event, transition to `Awaiting` so the user can retry; the in-flight `/3/book` may have succeeded — the resumed engine queries `/3/details` for the slot to reconcile. |

The asymmetry is deliberate: `Finding` is mid-search and harmless to
mark `Failed` (the user will retry). `Booking` may have *actually
booked* — naively retrying would double-book. So the daemon
reconciles via the provider's slot status before deciding. See
[engine.md §Booking-race](engine.md#booking-race) for the
reconciliation protocol.

Every resumed quest emits `daemon.resumed` (or
`daemon.interrupted`) with the prior status, so the audit log
captures the discontinuity. Notifications fire for `Failed`
transitions per the user's notify config.

---

## 9. Deployment shapes

### 9.1 Local dev

```bash
resy-snipe serve --foreground --dev
```

`--foreground` keeps stdout/stderr attached to the terminal.
`--dev` is shorthand for:

- `--log-format text --log-level debug`
- TTY passphrase prompt
- `--data-dir ./.resy-snipe-dev` (project-relative)
- Allows `--insecure-no-encryption` without `RESY_SNIPE_PROD=1` guard

A second terminal runs the CLI:

```bash
export RESY_SNIPE_TOKEN=$(resy-snipe user issue-token --label dev)
resy-snipe quest list
```

The dev mode is the v2 equivalent of v1's standalone-binary
loop ([ADR-0003 §Notes](../adr/0003-daemon-first-cli-as-client.md#notes)).

### 9.2 systemd

Unit shipped at `deploy/systemd/resy-snipe.service`. Key directives:

```ini
[Service]
Type=notify
User=resy-snipe
ExecStart=/usr/local/bin/resy-snipe serve
Restart=on-failure
TimeoutStopSec=30s
LoadCredential=passphrase:/etc/resy-snipe/passphrase
Environment=RESY_SNIPE_PASSPHRASE_FILE=%d/passphrase
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/resy-snipe
```

`Type=notify` matches the daemon's `sd_notify(READY=1)` call after
the boot banner ([§2.9](#29-boot-banner)). `LoadCredential=` keeps
the passphrase out of `/proc/<pid>/environ`. `TimeoutStopSec=30s`
covers the drain budget plus margin ([§7](#7-graceful-shutdown)).

### 9.3 Docker / docker-compose

Multi-stage `Dockerfile`: `builder` (Go toolchain) → `runtime`
(`gcr.io/distroless/base-debian12`, uid 65532). The signer binary,
if any, is mounted at runtime, not baked in.

Compose example at `deploy/docker/docker-compose.yml.example`:

```yaml
services:
  resy-snipe:
    image: ghcr.io/phall/resy-snipe:2.0.0
    restart: unless-stopped
    ports:
      - "127.0.0.1:7765:7765"
    volumes:
      - resy-snipe-data:/var/lib/resy-snipe
    environment:
      RESY_SNIPE_BIND: "0.0.0.0:7765"
    secrets: [passphrase]
    command: ["serve", "--keyfile=/run/secrets/passphrase"]

secrets:
  passphrase: { file: ./secrets/passphrase }

volumes:
  resy-snipe-data:
```

The container binds `0.0.0.0` inside its network namespace; Docker
port mapping constrains exposure. A front-of-house Caddy/Traefik
handles TLS per [ADR-0009](../adr/0009-reverse-proxy-native-http.md).

### 9.4 Tailscale

Simplest shape. The daemon binds loopback; `tailscale serve`
proxies it onto the tailnet:

```bash
resy-snipe serve --bind 127.0.0.1:7765 &
tailscale serve --bg --https=443 http://127.0.0.1:7765
```

Tailscale handles TLS, identity, and access control. The daemon
needs no proxy config beyond the default trusted-proxy CIDR list
(loopback is already trusted).

---

## 10. Observability

Cross-link: [observability.md](observability.md) has the full
metrics catalog and log key reference.

- **Logs**: structured JSON to stdout. Keys per
  [laws.md §Logging](../../laws.md#logging) (canonical
  `domain.LogKey*`). Level via `daemon.log_level`. No secret
  values, ever.
- **Metrics**: Prometheus on `GET /metrics`. Counters for HTTP
  requests by code/route, histograms for handler latency, gauges
  for engine queue depth and active quests by status. Auth on
  this endpoint is optional — operators can lock it down behind
  the proxy.
- **Profiling**: `GET /debug/pprof/*` is registered only when the
  request comes from a loopback peer. Off-loopback requests get
  `404`. There is no flag to broaden this; if you need pprof from
  elsewhere, port-forward.
- **Tracing**: out of scope for v2. Hooks (OpenTelemetry context
  propagation in HTTP middleware) are stubbed but not exported.

---

## 11. Backup story

`data.db` is the entire state. Two supported approaches:

### 11.1 Snapshot

```bash
sqlite3 /var/lib/resy-snipe/data.db ".backup '/backup/data.db.$(date +%F)'"
```

`.backup` cooperates with WAL — readers don't block, the daemon
keeps running, the snapshot is consistent. Plain `cp data.db
data.db.bak` is also safe with WAL; include `data.db-wal` and
`data.db-shm` for the latest committed transactions or run
`PRAGMA wal_checkpoint` first.

### 11.2 Continuous (litestream)

For zero-RPO, run litestream alongside the daemon. It attaches to
the WAL and streams every committed page to S3 (or any supported
target). Restore is `litestream restore` against an empty data dir.

### 11.3 Secrets

Per [ADR-0008](../adr/0008-secrets-sealed-at-rest-operator-key.md),
`data.db` contains *sealed* secrets — encrypted with the operator's
key. The unwrap key (passphrase / keyfile) is **not** part of the
backup unless the operator chooses to put it there. A backup
without the key is a credential-free dataset; a backup with the
key in the same archive is a credential dump. The operator owns
that decision.

---

## 12. Upgrade story

### 12.1 Forward upgrade

1. Pull the new binary (`docker pull`, `apt install`, `brew
   upgrade`, `go install`).
2. Restart the daemon (`systemctl restart resy-snipe`, or compose
   `up -d`).
3. The daemon runs migrations on boot ([§2.4](#24-migrations)).
4. Operators verify with `/readyz` and the boot banner's
   `schema v<n>` line.

There is no separate `migrate` subcommand — migrations are part
of `serve` boot. This is deliberate: there is no state where
"the binary is upgraded but migrations haven't run." The boot
sequence guarantees they happen together.

### 12.2 Refused downgrade

If the operator points an older binary at a newer schema, the
daemon exits with code `3` and message
`schema v<db> is newer than this binary's max v<bin>; downgrade
refused`. The operator must roll the binary forward, not the
schema back.

If the operator genuinely needs to downgrade (e.g. a critical
v2.1 bug), the path is:

1. Restore the pre-upgrade `data.db` backup.
2. Run the older binary against it.

There is no "schema rollback" tool. Migrations are forward-only.

### 12.3 Token / credential preservation

Tokens, sealed secrets, and audit log all live in `data.db`. A
clean upgrade preserves them. A reinstall against a fresh
`data.db` does not.

---

## 13. Anti-patterns

The daemon does not, and will not:

1. **Embed a reverse proxy.** No `--tls-cert`, no ACME, no
   redirect-to-HTTPS. TLS is the operator's proxy's job
   ([ADR-0009](../adr/0009-reverse-proxy-native-http.md)).
2. **Auto-renew certificates.** No ACME client, no upstream CA
   polling, no "letsencrypt" in the dependency tree.
3. **Mount external KV stores.** No Redis, no etcd, no Consul. SQLite
   is the only datastore
   ([ADR-0006](../adr/0006-sqlite-only-no-external-deps.md)).
4. **Phone home.** Outbound HTTP goes only to provider APIs (Resy),
   the loopback signer, and operator-configured notifier targets. No
   telemetry, no update checks, no analytics.
5. **Auto-start from the CLI.** `resy-snipe quest add` does not spawn
   a daemon. Shell-spawned daemons inherit weird environments and
   confuse ownership
   ([ADR-0003 §Alternatives](../adr/0003-daemon-first-cli-as-client.md#alternatives-considered)).
6. **Run as root.** Shipped systemd unit and Docker image run as an
   unprivileged user.
7. **Read `data.db` from non-`serve` subcommands.** Single-writer
   discipline. Admin commands go through HTTP, not direct SQL.

---

## See also

- [ADR-0003: Daemon-first, CLI as client](../adr/0003-daemon-first-cli-as-client.md)
- [ADR-0006: SQLite-only, no external deps](../adr/0006-sqlite-only-no-external-deps.md)
- [ADR-0008: Secrets sealed at rest](../adr/0008-secrets-sealed-at-rest-operator-key.md)
- [ADR-0009: Reverse-proxy native HTTP](../adr/0009-reverse-proxy-native-http.md)
- [ADR-0010: One daemon, many users](../adr/0010-one-daemon-many-users.md)
- [design/service-layer.md](service-layer.md) — verbs, sentinels, error mapping
- [design/secrets.md](secrets.md) — sealing protocol, KDF, rotation
- [design/multi-user.md](multi-user.md) — tenancy, tokens, audit
- [design/observability.md](observability.md) — logs, metrics, pprof
- [design/engine.md](engine.md) — state machine, booking race
- [docs/architecture.md](../../architecture.md) — package layering
- [docs/laws.md](../../laws.md) — project conventions
