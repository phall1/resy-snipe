# Milestone M2 — Daemonize: HTTP API, secrets, audit, deployment

**Status**: Scoped (sub-issues materialize when M1 closes)
**Owner**: phall
**Beads epic**: `resy-snipe-0li`
**Depends on**: M1
**Blocks**: M3 (MCP needs the HTTP transport pattern as a precedent), M4

## Goal in one sentence

Move the Service layer from "in-process behind the CLI" to "running in a
long-lived daemon with an HTTP API, encrypted secrets, audit log, and
deployment artifacts," so quests survive laptop sleep / terminal close /
operator inattention.

## What ships

- `resy-snipe serve` subcommand — long-lived daemon process.
- HTTP transport over the Service layer at `127.0.0.1:port` by default.
  Bearer-token auth. Error model and routes per `design/daemon.md`.
- The CLI becomes a thin HTTP client. `resy-snipe quest …` no longer
  embeds the engine; it requires a running daemon (with a clear error
  if not).
- Encrypted secrets store (per `design/secrets.md`). Operator unlocks
  with passphrase or `--keyfile`.
- Audit log table populated for every Service call.
- `/healthz`, `/readyz`, `/metrics`.
- Deployment artifacts: `Dockerfile`, `docker-compose.yml.example`,
  `systemd/resy-snipe.service`, `Caddyfile.example`,
  `nginx.conf.example`.
- `docs/v2/operations.md` runbook.

## What does NOT ship in M2

- MCP (M3).
- TUI (M4).
- Operator-facing notifications (push / Slack) (M4).
- NTP wrapper / signer auto-detect / residential proxy (M5).
- `resy-snipe doctor` (M4 — needs the daemon HTTP surface stable first).

## Acceptance criteria (sketch — finalize at M2 kickoff)

- A daemon, started via `systemctl start resy-snipe`, accepts HTTP
  requests from the CLI on the same box.
- A backup-restore round-trip works: `cp data.db data.db.bak`, stop
  daemon, simulate disk loss, restore from `data.db.bak` + keyfile,
  daemon resumes with all quests intact.
- Resy passwords stored in plaintext in M1 are migrated to sealed
  storage on first M2 boot (one-shot).
- `docker compose up` from a checkout produces a running daemon
  reachable at `http://localhost:8080/healthz`.
- Audit log: pulling 100 quests' worth of actions from the audit table
  reconstructs the user-visible history.
- Tenancy test still passes; HTTP transport never bypasses Service-layer
  user_id scoping.

## Sub-issues

Filed when M1 closes. Sketch:

- HTTP server + routing
- Bearer-token auth + token-rotation admin verbs
- Secrets sealing implementation + KDF + rotation subcommand
- Plaintext→sealed migration (one-shot, gated by version flag)
- Audit log writer + index choice + retention policy
- `/healthz`, `/readyz`, `/metrics` endpoints
- `Dockerfile` + multi-arch build
- `systemd` unit + `LoadCredential` integration
- Reverse-proxy example configs
- `operations.md` runbook
- E2E tests against the daemon via HTTP

## Cross-references

- ADRs: 0003, 0006, 0008, 0009, 0010
- Design: daemon.md, secrets.md, multi-user.md, observability.md
