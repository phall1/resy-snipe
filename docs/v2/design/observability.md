# Observability

**Layer**: cross-cutting (`internal/observe`, plus hooks in
`internal/service`, `internal/transport/http`, `internal/store`)
**Status**: Design — implementation lands in M2
**Related ADRs**: [0006](../adr/0006-sqlite-only-no-external-deps.md),
[0007](../adr/0007-self-hosted-only-no-saas.md),
[0009](../adr/0009-reverse-proxy-native-http.md),
[0010](../adr/0010-one-daemon-many-users.md)
**Related design**: [daemon.md](daemon.md),
[service-layer.md](service-layer.md), [secrets.md](secrets.md),
[multi-user.md](multi-user.md)

## Purpose

A homelab operator must answer three questions in under thirty
seconds, without external tooling:

1. Is the daemon healthy right now?
2. Has it sniped recently? When was the last successful booking?
3. Did anything fail today? What and why?

The layer is sized to that operator. `journalctl -u resy-snipe -f` on
the box must be enough. Prometheus + Grafana are common in homelabs
but optional — the daemon exposes data; the operator chooses whether
to scrape. Consistent with
[ADR-0006](../adr/0006-sqlite-only-no-external-deps.md) (no external
infra) and [ADR-0007](../adr/0007-self-hosted-only-no-saas.md) (no
phone-home).

## Pillars

Each pillar has a default that works without external dependencies
and an extension point for operators who wire up a stack.

| Pillar  | Default                              | Extension                         |
|---------|--------------------------------------|-----------------------------------|
| Logs    | structured JSON via `slog` to stdout | journald / Loki / `docker logs`   |
| Metrics | `/metrics` Prometheus exposition     | operator scrapes if they want     |
| Health  | `/healthz` (liveness)                | proxy / k8s probe                 |
| Ready   | `/readyz` (deep, loopback default)   | dashboards / uptime checks        |
| Doctor  | `resy-snipe doctor` subcommand       | one-shot, no daemon needed        |
| Audit   | `audit_events` row per Service call  | see [multi-user.md](multi-user.md) |

Audit is the tenancy contract per
[ADR-0010](../adr/0010-one-daemon-many-users.md); referenced here,
defined there.

## Logging contract

### Format

All logs are structured JSON via Go's `log/slog`, written to stdout.
That is the only format. `fmt.Printf`, `log.Printf`, and naked
`println` are forbidden — the `gates` recipe greps for them.

Stdout is the only sink because every supervisor (systemd, Docker,
k8s, runit) already captures it. A file sink duplicates that work
and creates a "where do logs live?" decision the operator should not
have to make.

### Standard fields on every record

Added by the root logger via `slog.Handler.WithAttrs`:

| Field     | Source                                                       |
|-----------|--------------------------------------------------------------|
| `time`    | RFC3339Nano UTC (`slog.TimeKey`)                             |
| `level`   | `DEBUG` / `INFO` / `WARN` / `ERROR` — no `FATAL` post-boot   |
| `msg`     | sentence case, no trailing period                            |
| `service` | `"resy-snipe"`                                               |
| `version` | build version, injected via `-ldflags`                       |
| `commit`  | git short sha, injected via `-ldflags`                       |

Request-scoped via `logger.With(...)`: `user_id` (after auth),
`quest_id`, `account_id`, `request_id` (server-generated),
`transport` (`http` / `mcp` / `cli`).

Domain-specific fields use the canonical keys in
`internal/domain/logfields.go` (existing: `snipe_id`, `venue_ref`,
`attempt`, `resy_request_id`, `intent_hash`, `provider`). Renaming
any breaks a snapshot test (per [§Test plan](#test-plan)) — log
queries in operator dashboards depend on the stability. New keys land
in that file, never as inline string literals at the call site —
that is what causes drift.

### Levels

| Level   | When                                                          |
|---------|---------------------------------------------------------------|
| `DEBUG` | Developer-only, off by default. Wire details, planner picks.  |
| `INFO`  | State changes the operator cares about. Quest created/booked. |
| `WARN`  | Recoverable. A retry, transient signer error, 429 backoff.    |
| `ERROR` | Failed without retry, or terminal failure.                    |

No `FATAL` outside boot. A serving daemon logs `ERROR` and keeps
running. Boot errors exit 1 after one final `ERROR` record.

### Forbidden content

The logger panics in tests if a record contains:

- secret values — see [design/secrets.md](secrets.md). Logging code
  that needs a secret reference uses the `version_id`, not the bytes.
- full HTTP request/response bodies. Snippets (first 256 bytes) with
  explicit truncation marker are allowed at DEBUG only.
- customer-support PII beyond `email_hash` — never raw email, phone,
  or name. The audit log holds the linkable form; logs hold the hash.

Enforced by a `slog.Handler` middleware in
`internal/observe/redact.go` that scans attribute values against a
deny-list of secret-store key prefixes before emission.

## Metrics catalog

Prometheus exposition at `/metrics`. Per
[ADR-0009](../adr/0009-reverse-proxy-native-http.md), loopback by
default, auth-gated on non-loopback bind. No separate metrics port —
one HTTP listener, one auth model.

The metric names below are the contract. Renaming is a breaking
change to operator dashboards; new dimensions are additive.

### Counters

```
# Two separate counters so dashboards compute creation/completion
# lag without subtracting gauges.
resy_snipe_quests_created_total{user, account}
resy_snipe_quests_completed_total{user, status}
  # status = booked | failed | expired | canceled

# Status-class bucketing keeps cardinality bounded vs. full codes.
resy_snipe_resy_requests_total{endpoint, status_class}
  # endpoint = find | calendar | details | book | auth
  # status_class = 2xx | 3xx | 4xx | 5xx | network

# Signer health. Noop signer does not emit.
resy_snipe_signer_invocations_total{result}
  # result = ok | error | timeout

# {ok} dimension is the first tenant-misuse signal.
resy_snipe_audit_events_total{action, ok}
```

### Histograms

Buckets are bounded — quests are seconds-to-hours but the histogram
must not retain unbounded series.

```
# End-to-end quest lifetime: submit → terminal.
resy_snipe_quest_duration_seconds{strategy, status}
  # buckets: 0.1, 0.5, 1, 5, 10, 30, 60, 300, 1800, 3600, 14400

# Wire latency to Resy, by endpoint.
resy_snipe_resy_request_duration_seconds{endpoint}
  # buckets: 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10

# ResolveVenue + Plan composition. Alert if 99p > 1s.
resy_snipe_planner_duration_seconds
  # buckets: 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5
```

### Gauges

```
# "Is anything in flight?" signal.
resy_snipe_quests_active{strategy}

resy_snipe_users_count

# Canary for "anti-bot signer broke." NaN if signer is Noop.
resy_snipe_signer_age_seconds

# Sampled every 60s. Growth-rate anomaly = runaway logger / audit.
resy_snipe_db_size_bytes

# Signed seconds. Emitted only if NTP wrapper enabled. Critical:
# Resy release timing is millisecond-tight.
resy_snipe_clock_skew_seconds
```

### Build info

```
# Pinned at 1; join target for version dimensions.
resy_snipe_build_info{version, commit, go_version, schema_version} = 1
```

Joining `build_info` against `quests_completed_total` shows which
schema processed which quests after an upgrade.

### Cardinality

`{user}` and `{account}` are bounded by tenant count (≤ ~50 per
[ADR-0006](../adr/0006-sqlite-only-no-external-deps.md)). All other
labels are closed sets. Total active series at saturation: low
thousands — well inside an embedded Prometheus / VictoriaMetrics.

## `/healthz`

Liveness probe. No auth. Always returns 200 with `{"ok":true}` once
the HTTP listener is accepting. Does not check DB, engine, or any
subsystem.

Liveness is "is the process accepting?", nothing more. A failing DB
or signer is not a reason to restart — it is a reason to investigate.
Coupling DB health to liveness causes restart loops that mask the
real failure. Failing this endpoint means the process is wedged at
the network layer; the supervisor should restart it.

```
GET /healthz
200 OK
content-type: application/json

{"ok":true}
```

## `/readyz`

Deep health. Loopback by default per
[ADR-0009](../adr/0009-reverse-proxy-native-http.md); same auth posture
as `/metrics`. Checks every subsystem and returns 200 if all ready,
503 otherwise. Body shape is identical at both codes — the operator
can read state regardless of overall status.

```
GET /readyz
200 OK   (or 503 Service Unavailable)
content-type: application/json

{
  "ok": true,
  "checked_at": "2026-05-10T19:46:02.123456Z",
  "checks": {
    "db_writable":      {"ok": true,  "detail": "tx ok in 1ms"},
    "secrets_unlocked": {"ok": true,  "detail": "active version 3"},
    "signer":           {"ok": true,  "detail": "last ok 14s ago"},
    "engine":           {"ok": true,  "detail": "running, 4 goroutines"},
    "schema":           {"ok": true,  "detail": "v17"}
  }
}
```

The checks:

1. **`db_writable`** — `BEGIN; SELECT 1; ROLLBACK;` against the live
   pool. A read-only DB (backup-restored without write perms) fails.
2. **`secrets_unlocked`** — the in-memory operator key is loaded.
   `nil` means the daemon booted without unlock; new sessions will
   fail. See [design/secrets.md](secrets.md).
3. **`signer`** — last successful invocation within `signer.MaxAge`
   (default 5m), OR the signer is the Noop default (`ok=true`,
   detail `"no-op signer"`).
4. **`engine`** — goroutine count meets boot minimum (`Run` +
   scheduler + subscriber dispatcher). Below that means a panic
   recovery did not restart the worker.
5. **`schema`** — `PRAGMA user_version` matches the binary's
   expected version. Mismatch means a migration did not run; the
   daemon boots read-only and `/readyz` reports it.

Each check times out at 1s; timeout is recorded as
`{"ok": false, "detail": "timeout"}`.

## `resy-snipe doctor`

Standalone diagnostic subcommand. Does not require the daemon to be
running. Exits 0 if every check passes, non-zero on any failure. The
operator's first move when something is weird, and the post-install
confirmation step before starting the daemon.

### Checks

| #  | Check                  | Detail                                                  |
|----|------------------------|---------------------------------------------------------|
| 1  | binary                 | `version`, `commit`, `go_version`                       |
| 2  | binary schema          | migration step the binary expects                       |
| 3  | config file            | path, parse, unknown-key warnings                       |
| 4  | data dir               | path, writable, free space                              |
| 5  | DB file                | path, size, `PRAGMA integrity_check`                    |
| 6  | DB schema              | `PRAGMA user_version` vs expected; reports drift        |
| 7  | DB journal mode        | confirms `journal_mode = WAL`                           |
| 8  | secrets store          | reachable, `active_version`, no unlock attempt          |
| 9  | signer binary          | path, exec bit, smoke-test if env set                   |
| 10 | resy reachability      | TCP + TLS handshake to `api.resy.com:443`               |
| 11 | clock skew             | NTP-wrapper skew if enabled, else `n/a`                 |

Signer smoke test runs only if `RESY_SNIPE_SIGNER_BIN` is set. Per
[secrets.md](secrets.md), `doctor` never asks for the unlock
passphrase; it confirms the secrets file is parseable, nothing more.

### Output

Human-tabular by default; `--json` for scripts. Both honor exit codes.

#### Successful run

```
$ resy-snipe doctor

resy-snipe doctor
=================
binary
  version          v2.1.3
  commit           a1b2c3d
  go_version       go1.24.2
  schema_expected  v17

config
  path             /home/phall/.config/resy-snipe/config.toml         [ok]
  parse            ok
  unknown_keys     none

data
  dir              /home/phall/.local/share/resy-snipe                [ok]
  writable         yes
  free_space       42.7 GiB

database
  path             /home/phall/.local/share/resy-snipe/data.db        [ok]
  size             3.4 MiB
  integrity_check  ok
  schema_version   v17                                                [ok]
  journal_mode     wal                                                [ok]

secrets
  path             /home/phall/.local/share/resy-snipe/secrets.bin    [ok]
  active_version   3
  unlock_required  yes (will prompt at daemon start)

signer
  bin              /usr/local/bin/resy-sign                           [ok]
  executable       yes
  smoke_test       ok (signed in 38ms)

network
  api.resy.com:443 tcp ok, tls ok (cert valid 84d)                    [ok]

clock
  ntp_skew         +0.012s                                            [ok]

result: 11/11 checks passed
exit: 0
```

#### Failed run

Only the differing sections shown — `binary`, `config`, `data`,
`secrets` are identical to the successful run.

```
database
  path             /home/phall/.local/share/resy-snipe/data.db        [ok]
  size             3.4 MiB
  integrity_check  ok
  schema_version   v15                                                [FAIL]
                   binary expects v17; run `resy-snipe migrate up`
  journal_mode     wal                                                [ok]

signer
  bin              /usr/local/bin/resy-sign                           [FAIL]
  executable       no
                   chmod +x /usr/local/bin/resy-sign
  smoke_test       skipped (binary not executable)

network
  api.resy.com:443 tcp ok, tls FAIL (i/o timeout after 5s)            [FAIL]
                   check firewall / outbound connectivity

clock
  ntp_skew         +1.842s                                            [WARN]
                   skew > 1s — Resy release timing may slip

result: 8/11 checks passed, 3 failed, 1 warning
exit: 1
```

Exit 1 if any check is `[FAIL]`. `[WARN]` reports but does not fail.
Per-check exit codes are not encoded — scripts read `--json`.

### `--json` output

```json
{
  "ok": false,
  "checks": [
    {"id": "db_schema",    "ok": false, "detail": {"expected":"v17","got":"v15"}},
    {"id": "signer",       "ok": false, "detail": {"reason":"not executable"}},
    {"id": "network_resy", "ok": false, "detail": {"reason":"tls timeout"}}
  ],
  "summary": {"passed": 8, "failed": 3, "warned": 1}
}
```

## Tracing

Out of scope for v2. Distributed tracing is overkill for one process
and most operators have no Jaeger/Tempo to ship to. The Service layer
is structured so a single OpenTelemetry hook is one call away: each
Service method is the natural span. A future
`internal/observe/trace` can wrap each Service call in
`otel.Tracer("service").Start(ctx, methodName)` without touching
business logic. Leave a TODO at the Service constructor; do not ship
the dependency until an operator asks.

## Alerting recommendations

The daemon does not ship alerting rules. Metrics are stable enough
that operators write their own. The operator owns their alert manager;
below are starting points, not commitments.

```yaml
# prometheus.rules.yml — operator-supplied, NOT shipped.
# Names elided; see prometheus docs for `for` / annotations conventions.
- alert: ResySnipeDown               # up{job="resy-snipe"} == 0      for 2m
- alert: ResySnipeSignerStale        # resy_snipe_signer_age_seconds > 3600
- alert: ResySnipeQuestFailureSpike  # rate(failed[10m]) > 3 * rate(booked[10m])
- alert: ResySnipeDBGrowingFast      # deriv(resy_snipe_db_size_bytes[1h]) > 1MiB
```

## Log aggregation patterns

All logs are stable JSON on stdout; the operator picks ingestion.

| Stack                  | Command                                            |
|------------------------|----------------------------------------------------|
| systemd / journald     | `journalctl -u resy-snipe -f -o json`              |
| Docker                 | `docker logs -f resy-snipe`                        |
| Loki + Promtail        | Promtail's `journal` target                        |
| Vector                 | `kubernetes_logs` or `journald` source             |
| Plain file (legacy)    | `resy-snipe serve >> /var/log/resy-snipe.log 2>&1` |

Field names are stable (per [§Test plan](#test-plan)), so queries
pin on `quest_id`, `user_id`, `resy_request_id` without rewriting
across upgrades.

## Sample log lines

Wire-shape; standard fields (`service`, `version`, `commit`) elided
for width on records after the first.

```json
{"time":"2026-05-10T19:46:02.123Z","level":"INFO","msg":"daemon started","service":"resy-snipe","version":"v2.1.3","commit":"a1b2c3d","bind":"127.0.0.1:8484"}
```

```json
{"time":"...","level":"INFO","msg":"quest created","user_id":"u_phall","quest_id":"q_01HX...","venue_ref":"carbone-nyc","strategy":"continuous","intent_hash":"sha256:7f3a..."}
```

```json
{"time":"...","level":"WARN","msg":"resy request retried","user_id":"u_phall","quest_id":"q_01HX...","provider":"resy","endpoint":"find","attempt":2,"resy_request_id":"r-9c...","status":429,"backoff_ms":850}
```

```json
{"time":"...","level":"INFO","msg":"quest booked","user_id":"u_phall","quest_id":"q_01HX...","account_id":"a_01HW...","venue_ref":"carbone-nyc","duration_ms":48326}
```

```json
{"time":"...","level":"ERROR","msg":"signer invocation failed","user_id":"u_phall","quest_id":"q_01HZ...","provider":"resy","endpoint":"book","attempt":1,"err":"signer: subprocess exited 2","stderr_snippet":"px3: handshake failed"}
```

`err` is the unwrapped error. `stderr_snippet` is truncated to 256
bytes per [§Forbidden content](#forbidden-content). `provider` and
`endpoint` use canonical keys from `internal/domain/logfields.go`.

## Test plan

Three layers of test coverage, all under `-race` per Law 17.

### 1. Snapshot test on field stability

`internal/domain/logfields_test.go` holds a `map[string]string`
snapshot of every `LogKey*` constant to its string value. Renaming
`LogKeySnipeID` from `"snipe_id"` to `"snipeId"` breaks the test.
A new key requires deliberate updates in two places (the constant
and the snapshot) — that friction is the point. Log keys are an
external contract.

### 2. `/healthz` returns 200 regardless of subsystem state

Build a daemon with broken DB, broken signer, and locked secrets.
`/healthz` must still return 200 with body `{"ok":true}`.
Load-bearing for the liveness/readiness split: a restart must not
be triggered by a transient subsystem failure.

### 3. `/readyz` integration: flip one subsystem

The test boots a real daemon, asserts `/readyz` returns 200 and
`signer.ok=true`, then sets the signer's last-success time to one
hour ago via `clock.Fake`. The next call must return 503 with
`signer.ok=false` and a detail mentioning `"last ok"`. Other
subsystems must still report `ok=true` independently.

Runs against `clock.Fake` per Law 8 — staleness is clock-driven,
not wall-clock-driven.

### 4. `doctor` exit codes per failure mode

Table-driven test, one row per failure mode:

| name                    | setup                       | wantExit | wantFail        |
|-------------------------|-----------------------------|----------|-----------------|
| `all_ok`                | none                        | 0        | —               |
| `schema_drift`          | DB at v15, binary at v17    | 1        | `db_schema`     |
| `signer_not_executable` | strip exec bit              | 1        | `signer`        |
| `resy_unreachable`      | block outbound 443          | 1        | `network_resy`  |
| `clock_skew_warn`       | NTP wrapper reports +2s     | 0        | —               |

The clock-skew row asserts warn-not-fail: 2s skew is `[WARN]`, exit
0, `summary.warned` non-zero.

## Cross-references

- [ADR-0006](../adr/0006-sqlite-only-no-external-deps.md) — no
  external observability infra required.
- [ADR-0007](../adr/0007-self-hosted-only-no-saas.md) — no
  phone-home; metrics are pull, logs are stdout.
- [ADR-0009](../adr/0009-reverse-proxy-native-http.md) — `/healthz`
  unauth; `/readyz` and `/metrics` loopback-default.
- [ADR-0010](../adr/0010-one-daemon-many-users.md) — audit is the
  tenancy contract; deferred to [multi-user.md](multi-user.md).
- [design/daemon.md](daemon.md) — HTTP route table.
- [design/service-layer.md](service-layer.md) — every Service call
  emits one log + one metric + one audit row.
- [design/secrets.md](secrets.md) — redaction rules.
- [design/multi-user.md](multi-user.md) — `audit_events` schema.
- [internal/domain/logfields.go](../../../internal/domain/logfields.go)
  — canonical structured-log keys.
