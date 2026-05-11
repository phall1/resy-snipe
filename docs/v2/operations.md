# Operations runbook (v2)

Operator's manual for `resy-snipe` v2. Audience: a homelab sysadmin
who has the binary installed and now needs to configure, run, back
up, upgrade, and troubleshoot it.

For *why* the daemon looks the way it does, see
[design/daemon.md](design/daemon.md) and the ADRs it references.
This document is the *how*.

> **Implementation status.** v2 is mid-build. Sections that depend
> on code that hasn't landed yet are marked **NOT YET WIRED** with
> a pointer to the beads issue and a fallback recipe (raw SQL, raw
> HTTP, or "wait"). Cross-check `git log --oneline | grep '(v2)'`
> against your binary if something behaves differently.

---

## 1. Overview

`resy-snipe serve` is a long-lived daemon holding every homelab
user's quests, accounts, and sealed Resy credentials in one SQLite
file. The CLI, the MCP server, and any future front-end are HTTP
clients ([ADR-0003](adr/0003-daemon-first-cli-as-client.md)).
Nothing else opens `data.db`. Why daemon-first: one writer to
SQLite, one signer subprocess, one resume path on boot, one place
to look when something is stuck. The daemon does not terminate
TLS ([ADR-0009](adr/0009-reverse-proxy-native-http.md)) and runs
as an unprivileged user. Default layout: binary at
`/usr/local/bin/resy-snipe`, config at `/etc/resy-snipe/`,
state at `/var/lib/resy-snipe/`, HTTP on `127.0.0.1:7765`, with
Caddy / nginx / Tailscale fronting TLS.

---

## 2. Installation

### 2.1 Prerequisites

- Linux x86_64 or arm64. macOS works for dev; the systemd recipe
  assumes Linux.
- A non-root system user to run the daemon as.
- No external services. SQLite is embedded via
  `modernc.org/sqlite`; no `libsqlite3`, no Redis, no PostgreSQL
  ([ADR-0006](adr/0006-sqlite-only-no-external-deps.md)).
- A reverse proxy (Caddy / nginx / `tailscale serve`) if you want
  network exposure.

### 2.2 Install the binary

**From source** (current path during v2):

```bash
git clone https://github.com/phall/resy-snipe.git
cd resy-snipe && just build
sudo install -m 0755 bin/resy-snipe /usr/local/bin/resy-snipe
resy-snipe version
```

Release artifacts (`apt` / `brew`) are **NOT YET WIRED** (M4).
Build from a tagged commit until then.

### 2.3 Filesystem layout

```
/etc/resy-snipe/config.toml       # 0644 root:root
/etc/resy-snipe/secret.key        # 0400 resy-snipe:resy-snipe (keyfile mode)
/var/lib/resy-snipe/data.db       # SQLite + WAL sidecars (-wal, -shm)
/var/lib/resy-snipe/data.db.lock  # flock; daemon owns exclusively
/usr/local/bin/resy-snipe
/etc/systemd/system/resy-snipe.service
```

`data.db` is the entire system state — quests, users, tokens,
sealed secrets, audit log. Back it up
([§6](#6-backup-and-restore)).

### 2.4 User and group

A `deploy/systemd/resy-snipe.sysusers` drop-in is planned
(**NOT YET WIRED**, M2-22). Until then:

```bash
sudo useradd --system --shell /usr/sbin/nologin \
    --home-dir /var/lib/resy-snipe --create-home resy-snipe
sudo chown -R resy-snipe:resy-snipe /var/lib/resy-snipe
sudo chmod 0700 /var/lib/resy-snipe
sudo install -d -m 0755 /etc/resy-snipe
```

### 2.5 systemd unit

A maintained unit at `deploy/systemd/resy-snipe.service` is
planned (**NOT YET WIRED**, M2-22 — the directory exists and is
empty). Until that lands, write
`/etc/systemd/system/resy-snipe.service` (modeled on
[design/daemon.md §9.2](design/daemon.md#92-systemd)):

```ini
[Unit]
Description=resy-snipe daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
User=resy-snipe
Group=resy-snipe
ExecStart=/usr/local/bin/resy-snipe serve --config /etc/resy-snipe/config.toml
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/resy-snipe

[Install]
WantedBy=multi-user.target
```

`sudo systemctl daemon-reload && sudo systemctl enable resy-snipe`.
Do not start yet — generate the keyfile first ([§4.1](#41-generate-the-keyfile)).

> **NOT YET WIRED.** The `serve` subcommand is not dispatched
> from `cmd/resy-snipe/main.go` as of M2 wave 3. The
> `internal/daemon.Boot` orchestrator and the HTTP transport
> skeleton are shipped; the CLI entry point lands in M2 wave 4
> (beads M2-15). Until then `systemctl start resy-snipe` fails
> with "subcommand not recognized."

---

## 3. Configuration

Source of truth:
[`internal/daemon/config.go`](../../internal/daemon/config.go).

### 3.1 Sources and precedence

Highest first: CLI flag > `RESY_SNIPE_*` env > config file >
compiled-in default. Lookup order for the file:
`--config <path>` → `$XDG_CONFIG_HOME/resy-snipe/config.toml` →
`/etc/resy-snipe/config.toml`. Unknown TOML keys are an error.

### 3.2 `config.toml`

```toml
[daemon]
bind                    = "127.0.0.1:7765"   # host:port; loopback by default
data_dir                = "/var/lib/resy-snipe"
log_format              = "json"             # "json" | "text"
log_level               = "info"             # debug | info | warn | error
shutdown_drain_seconds  = 15

[secrets]
mode                    = "keyfile"          # "passphrase" | "keyfile" | "insecure"
keyfile                 = "/etc/resy-snipe/secret.key"
argon2id_memory_kb      = 65536              # passphrase mode only
argon2id_time           = 3
argon2id_parallel       = 1

[http]
base_path               = "/"
trusted_proxies         = [
  "127.0.0.0/8", "::1/128",
  "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7",
]
external_url            = ""

[signer]
bin                     = ""                 # absolute path; empty = Noop
restart_backoff_seconds = 5

[engine]
max_concurrent_bookings = 8
default_lookahead_days  = 60
```

Field reference:

- `daemon.bind` — bind address; loopback unless fronted by a proxy.
- `daemon.data_dir` — where `data.db` and the flock live. Must be
  daemon-writable.
- `daemon.log_format` — `json` for log shippers, `text` for
  tail-by-eye.
- `daemon.log_level` — slog levels (debug/info/warn/error).
- `daemon.shutdown_drain_seconds` — graceful-stop budget.
- `secrets.mode` — master key source; see
  [§4.1](#41-generate-the-keyfile).
- `secrets.keyfile` — path to a 64-hex-char keyfile; required
  when `mode=keyfile`.
- `secrets.argon2id_*` — Argon2id parameters for passphrase mode.
  Min memory 8192 KiB, min time 1, min parallel 1.
- `http.base_path` — path prefix for all routes.
- `http.trusted_proxies` — see [§3.4](#34-trusted-proxy-cidr).
- `http.external_url` — external base URL for link generation.
- `signer.bin` — absolute path to PerimeterX signer; empty = Noop.
- `signer.restart_backoff_seconds` — signer restart backoff.
- `engine.max_concurrent_bookings` — cap on in-flight `/3/book`
  across all users.
- `engine.default_lookahead_days` — planner default window.

### 3.3 Environment overrides

Every config key has a `RESY_SNIPE_*` equivalent (e.g.
`RESY_SNIPE_BIND`, `RESY_SNIPE_DATA_DIR`, `RESY_SNIPE_LOG_LEVEL`,
`RESY_SNIPE_SECRETS_MODE`, `RESY_SNIPE_KEYFILE`,
`RESY_SNIPE_TRUSTED_PROXIES` as CSV, `RESY_SNIPE_SIGNER_BIN`,
`RESY_SNIPE_MAX_BOOKINGS`, …). Empty values are treated as "not
set"; to clear a default, use a flag. Full list:
[`internal/daemon/config.go`](../../internal/daemon/config.go)
`ApplyEnv`.

Two extras that aren't config keys:

- `RESY_SNIPE_PASSPHRASE` — passphrase for `mode = "passphrase"`.
  Read once, zeroed in memory immediately. Deliver via systemd
  `LoadCredential=` or Docker secrets, never a shell rc.
- `RESY_SNIPE_PROD=1` — tripwire. Refuses to boot in
  `mode = "insecure"` or with `--insecure-no-encryption`. Set on
  every non-dev box.

### 3.4 Trusted-proxy CIDR

`X-Forwarded-Proto/Host/For` are trusted only when the immediate
TCP peer falls in `http.trusted_proxies`. Default covers loopback
and RFC1918; add your proxy's network if it lives elsewhere
(Tailscale: `100.64.0.0/10`). Misconfiguring this is the most
common deployment bug — see
[§8.4](#84-x-forwarded-for-is-ignored) and
[design/daemon.md §5](design/daemon.md#5-trusted-proxy-cidr-list).

---

## 4. First boot

### 4.1 Generate the keyfile

Recommended deployment is keyfile mode (no terminal prompt, no
env var, no risk of `tee`'d shell history). 32 random bytes,
hex-encoded:

```bash
sudo install -d -m 0755 /etc/resy-snipe
sudo sh -c 'head -c 32 /dev/urandom | xxd -p -c 64 > /etc/resy-snipe/secret.key'
sudo chown resy-snipe:resy-snipe /etc/resy-snipe/secret.key
sudo chmod 0400 /etc/resy-snipe/secret.key
```

The daemon refuses keyfiles that are world- or group-readable
(see
[`internal/secrets/kdf.go`](../../internal/secrets/kdf.go)
`ReadKeyfile`). **Back up this file now** — a `data.db` without
its keyfile is opaque ciphertext for the `secrets` table. See
[§6.3](#63-backing-up-the-keyfile). For passphrase mode, leave
`secrets.mode = "passphrase"` and deliver `RESY_SNIPE_PASSPHRASE`
via systemd `LoadCredential=`.

### 4.2 Start the daemon

```bash
sudo systemctl start resy-snipe
sudo systemctl status resy-snipe
```

(See the **NOT YET WIRED** note in [§2.5](#25-systemd-unit). For
now, exercise via `go test ./internal/daemon/...`.)

### 4.3 The boot banner

On every successful boot the daemon prints to stderr (and the
structured log):

```
resy-snipe 2.0.0 (commit abc1234, schema v4)
  bind:        127.0.0.1:7765
  data-dir:    /var/lib/resy-snipe
  db:          /var/lib/resy-snipe/data.db
  signer:      noop
  secrets:     unlocked (keyfile, key-version=1)
ready.
```

If `secrets:` reads `dev-mode (insecure)` the daemon is running
without sealing — only acceptable in dev. See
[design/secrets.md §Dev-mode](design/secrets.md#dev-mode).

### 4.4 Seed the operator user

The first user is created explicitly; the migration does **not**
auto-seed:

```bash
sudo systemctl stop resy-snipe        # single-writer rule
sudo -u resy-snipe resy-snipe user seed --email you@example.com
# Seeded operator usr_abc12345 (you@example.com); bound 0 v1 account row(s).
sudo systemctl start resy-snipe
```

Operator row: `role='admin'`, `invited_by=NULL`. Only one ever
exists ([ADR-0011](adr/0011-operator-issued-invites-no-self-registration.md)).
Re-running with the same email is idempotent.

As of M2 wave 3 `user seed` opens `data.db` directly, hence the
stop/start dance. Once M2-18 ships it will run over HTTP against
a live daemon.

### 4.5 Issue the first operator bearer token

The CLI subcommand `resy-snipe user issue-token` is **NOT YET
WIRED** (beads M2-18). The HTTP route is shipped
(`POST /v1/auth/tokens` — see
[`internal/transport/http/tokens.go`](../../internal/transport/http/tokens.go))
but requires an existing bearer to call, creating a chicken-and-
egg. Bootstrap in SQL (table schema:
[design/multi-user.md §tokens](design/multi-user.md#tokens)):

```bash
sudo systemctl stop resy-snipe
TOKEN=$(head -c 32 /dev/urandom | base64 | tr -d '=+/' | head -c 43)
HASH=$(printf '%s' "$TOKEN" | sha256sum | awk '{print $1}')
sudo -u resy-snipe sqlite3 /var/lib/resy-snipe/data.db <<SQL
INSERT INTO tokens (token_hash, user_id, scope, label, created_at, expires_at)
VALUES (
  x'$HASH',
  (SELECT id FROM users WHERE email='you@example.com'),
  'cli', 'bootstrap',
  CAST(strftime('%s','now') AS INTEGER)*1000,
  CAST(strftime('%s','now','+365 days') AS INTEGER)*1000
);
SQL
sudo systemctl start resy-snipe
echo "Bearer: $TOKEN"   # the only time you see it
export RESY_SNIPE_TOKEN=$TOKEN
```

After M2-18 lands the equivalent will be
`resy-snipe user issue-token --label bootstrap --scope operator`.
Stash the token in your password manager.

---

## 5. Day-2 operations

### 5.1 Issuing and revoking tokens

Each token: one user, one scope (`api` / `mcp` / `cli`), one
free-form label. Plaintext shown once at issue; DB stores
`sha256(token)`.

HTTP (shipped today):

```bash
# Issue
curl -X POST https://snipe.example/v1/auth/tokens \
  -H "Authorization: Bearer $RESY_SNIPE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"label":"laptop","scope":"cli"}'

# List          curl … GET    /v1/auth/tokens
# Revoke        curl … DELETE /v1/auth/tokens/tok_abc123
```

CLI (`resy-snipe user {issue,revoke,list}-token`) — **NOT YET
WIRED**, M2-18.

### 5.2 Inviting users

Operator invites; new users redeem the invite to set their own
password
([ADR-0011](adr/0011-operator-issued-invites-no-self-registration.md)).

CLI scaffolding exists (`resy-snipe user invite`,
`resy-snipe user accept-invite`) but currently prints
`requires the daemon Service layer (coming in M1-10)`. HTTP
routes `POST /v1/users` / invite-redemption are documented in
[design/multi-user.md §invites](design/multi-user.md#invites)
but **NOT YET WIRED** (M2-17/M2-18).

Manual workaround during the gap: `INSERT` into `users` with
`role='user'` and a Service-produced argon2id password hash.
Brittle; wait for the HTTP surface if you can.

### 5.3 Rotating the master secret

Library primitive shipped:
[`internal/secrets/rotate.go`](../../internal/secrets/rotate.go)
— `Sealer.Rotate(ctx, newKey)` re-encrypts every `secrets` row
under `newKey` inside one SQL transaction and bumps
`secrets_meta.active_version`. Atomic.

`resy-snipe secrets rotate` CLI — **NOT YET WIRED** (M2-19).
Until then, rotation requires a small Go program constructing
the Sealer with the old key and calling `Rotate(ctx, newKey)`.
After success, point `secrets.keyfile` at the new path and
restart. Boot sentinel-opens one row; a wrong keyfile fails with
`event="secrets.boot_unwrap_failed"`.

### 5.4 Health checks

| Endpoint   | Auth     | Use                                       |
|------------|----------|-------------------------------------------|
| `/healthz` | none     | Liveness. `200 ok`.                       |
| `/readyz`  | optional | DB writable, secrets unlocked, engine OK. |
| `/metrics` | optional | Prometheus. **NOT YET WIRED** (M2-21).    |

```bash
curl -s http://127.0.0.1:7765/healthz
# ok
curl -s http://127.0.0.1:7765/readyz | jq
# { "version": "...", "schema_version": 4, "db_writable": true,
#   "secrets": "unlocked", "trusted_proxies": [...] }
```

Without a token, `/readyz` omits per-user counts so the endpoint
cannot enumerate the system from outside.

### 5.5 Audit-log review

Every Service call writes one `audit_events` row
([design/multi-user.md §audit_events](design/multi-user.md#audit_events)).
The reader API (`resy-snipe audit list`) is **NOT YET WIRED**;
query directly via `sqlite3`. Timestamps are unix-millis.

```sql
-- Failed auth in the last 24h.
SELECT datetime(created_at/1000,'unixepoch') AS at,
       user_id, action, error_code, ip, user_agent
  FROM audit_events
 WHERE ok=0 AND action LIKE 'auth.%'
   AND created_at >= (CAST(strftime('%s','now','-1 day') AS INTEGER)*1000)
 ORDER BY created_at DESC;

-- Recent quest activity for one user.
SELECT datetime(created_at/1000,'unixepoch') AS at,
       action, target_id, ok, error_code
  FROM audit_events
 WHERE user_id='usr_abc12345' AND action LIKE 'quest.%'
 ORDER BY created_at DESC LIMIT 50;

-- Admin actions on other tenants (operator audit).
SELECT datetime(created_at/1000,'unixepoch') AS at,
       user_id AS actor, target_user_id AS subject, action, ok
  FROM audit_events
 WHERE target_user_id IS NOT NULL AND target_user_id != user_id
 ORDER BY created_at DESC LIMIT 50;
```

---

## 6. Backup and restore

### 6.1 What to back up

Two things — both required to restore:

1. `/var/lib/resy-snipe/data.db` — quests, users, sealed
   secrets, audit log.
2. `/etc/resy-snipe/secret.key` — the unwrap key. Without it,
   accounts in `data.db` are unrecoverable.

> **Do not put both in the same backup snapshot.** That is a
> credential dump
> ([ADR-0008](adr/0008-secrets-sealed-at-rest-operator-key.md)).
> Different targets, different credentials.

### 6.2 Snapshotting `data.db`

WAL-aware, daemon may keep running:

```bash
sudo -u resy-snipe sqlite3 /var/lib/resy-snipe/data.db \
  ".backup '/backup/resy-snipe/data.db.$(date +%F)'"
```

With the daemon stopped, plain `cp` of all three files
(`data.db`, `-wal`, `-shm`) also works, or run
`PRAGMA wal_checkpoint(TRUNCATE);` first. For zero-RPO, run
`litestream` alongside
([design/daemon.md §11.2](design/daemon.md#112-continuous-litestream)).

### 6.3 Backing up the keyfile

Copy the keyfile to a separate target (hardware token, `pass`
entry on another machine, age-encrypted file in a *different*
restic repo). The runbook target: keyfile and `data.db` are
never co-located in the same backup snapshot.

### 6.4 Restore

1. Stop the daemon. Confirm nothing holds `data.db.lock`.
2. Restore the keyfile to `/etc/resy-snipe/secret.key`, mode
   0400, owned by `resy-snipe`.
3. Restore `data.db` to `/var/lib/resy-snipe/data.db`, owned by
   `resy-snipe`. Remove stale `data.db-wal` / `-shm` from the
   previous run — the backup is the truth.
4. Start the daemon. Banner should read
   `secrets: unlocked (keyfile, key-version=N)`.
5. `curl /readyz`; spot-check audit log row counts.

Lost keyfile, intact `data.db`: see
[§8.5](#85-secrets-unlock-failed).

---

## 7. Upgrade and downgrade

### 7.1 Forward upgrade

Migrations are forward-only and run automatically on boot
([ADR-0006](adr/0006-sqlite-only-no-external-deps.md)). There is
no separate `migrate` subcommand.

```bash
sudo systemctl stop resy-snipe
sudo -u resy-snipe sqlite3 /var/lib/resy-snipe/data.db \
  ".backup '/var/lib/resy-snipe/data.db.pre-upgrade'"
sudo install -m 0755 bin/resy-snipe /usr/local/bin/resy-snipe
sudo systemctl start resy-snipe
journalctl -u resy-snipe -n 50    # confirm the banner's schema v<n>
```

`/readyz` reports `schema_version` for scripted checks.

### 7.2 Refused downgrade

Older binary against newer schema: daemon exits non-zero with
`store: schema is newer than this binary; downgrade refused`
([`internal/store/migrate.go`](../../internal/store/migrate.go)
`ErrSchemaNewer`). To genuinely downgrade:

1. Restore the pre-upgrade `data.db` backup.
2. Run the older binary against it.

There is no schema-rollback tool. Discipline: back up before
upgrading.

### 7.3 What survives

Users, tokens, sealed secrets, audit log, quests, idempotency
keys all live in `data.db` and survive a clean upgrade. A
reinstall against a fresh `data.db` loses all of it, including
your tokens.

---

## 8. Troubleshooting

### 8.1 Daemon won't start: flock held

**Symptom.** `daemon: acquire flock: file already locked`, exit
code 2.

**Cause.** Another `serve` process — or a stale one — holds
`data.db.lock`.

```bash
ss -tlnp | grep 7765
sudo lsof /var/lib/resy-snipe/data.db.lock
ps -fp <pid>
sudo kill -TERM <pid>
# only if drain hangs past TimeoutStopSec:
sudo kill -KILL <pid>
```

After a SIGKILL the boot path reconciles any quest left in
`Booking` state ([design/daemon.md §8](design/daemon.md#8-resume-semantics)).

### 8.2 401 on every request

**Symptom.** `{"code":"unauthenticated"}` everywhere.

**Cause.** Token revoked, expired, hash mismatch, or wrong scope
(operator-only routes reject `user`-scope tokens with 403).

```sql
SELECT t.scope, t.label, t.revoked_at, t.expires_at, u.email
  FROM tokens t JOIN users u ON u.id=t.user_id
 WHERE t.token_hash = ?;   -- sha256 of the bearer you're sending
```

If revoked or expired, mint a fresh token
([§4.5](#45-issue-the-first-operator-bearer-token)). For scope
problems, see route table in
[design/daemon.md §4.1](design/daemon.md#41-routes).

### 8.3 "schema is newer than this binary"

Older binary at a newer DB. Roll the binary forward or restore a
pre-upgrade backup. See [§7.2](#72-refused-downgrade).

### 8.4 X-Forwarded-For is ignored

**Symptom.** Audit log records `127.0.0.1` (or the proxy's IP)
for every request.

**Cause.** The proxy's source IP is not in
`http.trusted_proxies`.

**Fix.**

```toml
[http]
trusted_proxies = ["127.0.0.0/8", "::1/128", "100.64.0.0/10"]  # +Tailscale
```

Reload. Confirm via `curl /readyz` — response includes the
effective list. See
[design/daemon.md §5](design/daemon.md#5-trusted-proxy-cidr-list).

### 8.5 Secrets unlock failed

**Symptom.** `event="secrets.boot_unwrap_failed"`; daemon
refuses to start.

**Cause.** Keyfile changed, passphrase wrong, or keyfile rotated
without running `secrets rotate` first.

**Fix.**

1. Restore the previous keyfile from backup and start the daemon.
2. If no backup: stop the daemon, `DELETE FROM secrets;` (and
   optionally `DELETE FROM sessions;`), restart, re-add every
   account from scratch. Quests in non-terminal states will fail
   their next tick with `ErrAuthRequired`; cancel and recreate.

Argon2id is not a brute-force target. Forgotten passphrase + no
keyfile backup is unrecoverable. See
[design/secrets.md §"I forgot my passphrase"](design/secrets.md#i-forgot-my-passphrase).

### 8.6 Mixed-version secrets rows

**Symptom.** `secrets: mixed-version rows: (usr_…,acct_…,kind)@vN, …`.

**Cause.** A rotate was interrupted outside the COMMIT (rare —
the library makes this impossible) or an operator hand-edited
rows with `sqlite3`.

**Fix.** Re-run the rotate; the library will re-encrypt the
stale rows under the active key.

### 8.7 Dev-mode banner in production

**Symptom.** `secrets: dev-mode (insecure)` on a machine you
consider production. Recoverable secrets are plaintext.

**Fix.** Set `RESY_SNIPE_PROD=1` in the systemd unit — the
daemon will refuse to boot in insecure mode under that tripwire.
Configure a real `secrets.mode`, generate a keyfile
([§4.1](#41-generate-the-keyfile)), restart.

---

## 9. Filing bugs

- **Beads** (`bd`) — internal task tracking. `bd ready` for open
  work, `bd show <id>` for detail.
- **GitHub Issues** — <https://github.com/phall/resy-snipe>.
  Include the boot banner
  (`journalctl -u resy-snipe | grep -A6 'resy-snipe '`), the
  `/readyz` response (redact tokens), reproduction steps, and
  the smallest log excerpt that shows the failure. Logs are JSON
  by default; `jq` is your friend.

Resy API protocol bugs go to Resy, not here.

---

## See also

- [design/daemon.md](design/daemon.md) — daemon spec.
- [design/secrets.md](design/secrets.md) — sealing, KDF, rotate,
  dev-mode rules.
- [design/multi-user.md](design/multi-user.md) — users, tokens,
  accounts, audit-log schema.
- [design/service-layer.md](design/service-layer.md) — sentinel
  errors and HTTP mapping.
- [adr/0003-daemon-first-cli-as-client.md](adr/0003-daemon-first-cli-as-client.md)
- [adr/0006-sqlite-only-no-external-deps.md](adr/0006-sqlite-only-no-external-deps.md)
- [adr/0008-secrets-sealed-at-rest-operator-key.md](adr/0008-secrets-sealed-at-rest-operator-key.md)
- [adr/0010-one-daemon-many-users.md](adr/0010-one-daemon-many-users.md)
- [adr/0011-operator-issued-invites-no-self-registration.md](adr/0011-operator-issued-invites-no-self-registration.md)
