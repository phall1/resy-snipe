# resy-snipe v2 — design tree

v2 is the re-architecture from "scheduled HTTP client that needs you to
be the planner" to "goal-seeking reservation-orchestration daemon with
two equal front-ends: humans (CLI/TUI) and agents (MCP)." Same engine
underneath; what changes is everything in front of it.

This directory is the contract. Code that diverges from these docs is
either wrong or supersedes them — pick one and update the other.

## How to read this

1. **Start with [design/overview.md](design/overview.md)** — the whole
   architecture in one diagram + 5 paragraphs.
2. **Skim [adr/](adr/)** — every load-bearing decision is one ADR. If
   you're touching a layer, the ADR tells you what's nailed down and
   what's open.
3. **Read the design doc for the layer you're working in.** Each
   `design/<layer>.md` is the spec the implementation must match.
4. **Find your work in [milestones/](milestones/)** — M1–M5 each
   enumerate concrete deliverables, acceptance criteria, and beads ids.

## Index

### Architecture decisions (ADRs)

| # | Title | Status |
|---|---|---|
| [0001](adr/0001-goal-driven-architecture.md) | Goal-driven architecture: separate `Goal` from `Intent` | Accepted |
| [0002](adr/0002-resolver-planner-engine-layering.md) | Resolver + Planner + Engine as three serial layers | Accepted |
| [0003](adr/0003-daemon-first-cli-as-client.md) | Daemon is the system; CLI is a thin client | Accepted |
| [0004](adr/0004-mcp-as-peer-front-end.md) | MCP is a peer-class front-end, not a wrapper | Accepted |
| [0005](adr/0005-multi-user-data-model-from-day-one.md) | Multi-user data model lands in M1, not later | Accepted |
| [0006](adr/0006-sqlite-only-no-external-deps.md) | SQLite WAL is the only datastore | Accepted |
| [0007](adr/0007-self-hosted-only-no-saas.md) | Self-hosted only; no SaaS | Accepted |
| [0008](adr/0008-secrets-sealed-at-rest-operator-key.md) | Secrets sealed at rest; key controlled by operator | Accepted |
| [0009](adr/0009-reverse-proxy-native-http.md) | HTTP server is reverse-proxy native, not internet-facing | Accepted |
| [0010](adr/0010-one-daemon-many-users.md) | One daemon process serves all friends-and-family on a box | Accepted |
| [0011](adr/0011-operator-issued-invites-no-self-registration.md) | New users join via operator-issued invite tokens | Accepted |
| [0012](adr/0012-plan-first-ux.md) | Plan is an artifact users approve before execution | Accepted |

### Layer designs

| Layer | Doc | Owns |
|---|---|---|
| Overview | [overview.md](design/overview.md) | The whole picture, dependency rules, request flow |
| Resolver | [resolver.md](design/resolver.md) | URL/slug/name → `domain.Venue` |
| Planner | [planner.md](design/planner.md) | `Goal` + `Venue` → `domain.Intent` |
| Service layer | [service-layer.md](design/service-layer.md) | RPC surface shared by HTTP API, CLI, MCP |
| Daemon | [daemon.md](design/daemon.md) | Process lifecycle, config, transports, health |
| MCP | [mcp.md](design/mcp.md) | Resources, tools, streaming, agent UX |
| Multi-user | [multi-user.md](design/multi-user.md) | Users, accounts, sessions, audit log, sharing |
| Secrets | [secrets.md](design/secrets.md) | Sealed-at-rest store, KDF, rotation |
| Observability | [observability.md](design/observability.md) | Logs, metrics, healthz, doctor |

### Milestones

| ID | Milestone | Beads |
|---|---|---|
| M1 | [Goal-driven local CLI + multi-user schema](milestones/m1-goal-driven.md) | TBD |
| M2 | [Daemonize: HTTP, secrets, audit, deployment](milestones/m2-daemon.md) | TBD |
| M3 | [MCP front-end](milestones/m3-mcp.md) | TBD |
| M4 | [Operator UX & TUI](milestones/m4-operator-ux.md) | TBD |
| M5 | [Precision & polish](milestones/m5-precision.md) | TBD |

## Out of scope (and why)

- **Hosted SaaS.** [ADR-0007](adr/0007-self-hosted-only-no-saas.md).
- **Cross-user Resy account sharing in v1.** Single user owns single
  Resy account in v1. Multi-grant comes later. See
  [ADR-0010 Notes](adr/0010-one-daemon-many-users.md#notes) and
  [design/multi-user.md](design/multi-user.md).
- **Web UI.** TUI is the human surface; agents use MCP. A web UI is
  a v3 problem if anyone asks for it.
- **Federated multi-instance.** Each homelab box runs its own daemon.
  No cross-instance coordination.

## Conventions for this tree

- **ADRs are immutable once accepted.** Override by writing a new ADR
  whose Status is "Accepted, supersedes 00NN" and update the old one's
  status to "Superseded by 00MM."
- **Design docs evolve.** When code diverges, update the doc in the
  same PR. A divergence that lasts past a PR is a bug.
- **Milestone docs freeze on milestone-start.** Acceptance criteria
  don't move once a milestone is in flight; new scope = a new milestone
  or a follow-up.

## Pointers back to v1

- [docs/architecture.md](../architecture.md) — current package layering
  (still authoritative for v1 code paths)
- [docs/laws.md](../laws.md) — current conventions (most carry into v2)
- [docs/state-machine.md](../state-machine.md) — engine state machine
  (unchanged in v2)
- [docs/getting-started.md](../getting-started.md) — current user
  walkthrough (will be rewritten end of M1)
