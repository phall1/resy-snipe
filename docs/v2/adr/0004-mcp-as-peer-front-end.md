# ADR 0004: MCP is a peer-class front-end, not a wrapper

**Status**: Accepted
**Date**: 2026-05-10
**Decision-makers**: phall
**Related**: [ADR-0003](0003-daemon-first-cli-as-client.md),
[ADR-0012](0012-plan-first-ux.md),
[design/mcp.md](../design/mcp.md),
[design/service-layer.md](../design/service-layer.md)

## Context

The system has two real users: humans (CLI/TUI) and agents (Claude,
Claude Code, future LLM clients). Both want the same verbs — resolve a
venue, plan a quest, create a quest, watch its events, cancel it. The
question is whether the agent surface is:

a. **A wrapper around the human-facing API** (MCP tools that internally
   call the HTTP API), or
b. **A peer transport over the same Service interface** (MCP and HTTP
   are siblings, both consume the same in-process Service).

(a) is easier to ship but bakes the human contract into the agent
contract — every API quirk leaks. (b) requires designing one Service
interface that's good for both, which is harder up front but means
neither surface drags the other down.

## Decision

MCP is a peer to HTTP, not a wrapper. Both transports consume the same
in-process `Service` interface (Go interface, shared types, in-memory
calls). Neither speaks to the other.

```
   CLI ─► HTTP ─┐
                ├─► Service ─► Resolver/Planner/Engine
   MCP ────────┘
```

The `Service` interface ([design/service-layer.md](../design/service-layer.md))
is the single source of truth for what the system can do. HTTP and
MCP are presentation concerns.

## Consequences

**Positive**
- One shipping artifact for "the API of the system" — the Go
  `Service` interface. Adding a verb adds one method; both transports
  pick it up.
- MCP gets first-class, structured, schema-annotated input/output
  natively (no parsing JSON over HTTP twice).
- Agents and humans get the same semantics — no "the agent can do X
  but the CLI can't" drift.
- Streaming (quest events) is a single internal pubsub the daemon
  emits; HTTP exposes it as SSE/WebSocket, MCP as
  `notifications/`. Both consume the same `chan domain.Event`.

**Negative**
- Designing a Service interface that's idiomatic for both Go callers
  and JSON-shaped agent calls requires care. Easy to leak Go-isms
  (channels, contexts, structs with private fields) into shapes that
  don't translate.
- More upfront wiring than a thin MCP-over-HTTP shim.

**Neutral**
- HTTP transport is M2; MCP transport is M3. Doing them in that order
  exercises the Service interface twice — the second transport often
  exposes leaks the first hid.

## Alternatives considered

1. **MCP wraps HTTP.** *Rejected:* every HTTP idiom (status codes,
   query params, content negotiation) leaks into MCP tool definitions.
   Streaming becomes an SSE-over-HTTP-over-MCP pile. Two layers of
   error translation.
2. **MCP only; CLI uses MCP too.** *Rejected:* MCP is JSON-RPC over
   stdio/HTTP; perfectly fine for agents but verbose and subprocess-y
   for a shell pipeline. `resy-snipe quest list | jq` is cleaner over
   plain HTTP.
3. **No MCP; ship a Go SDK and let agents shell out to the CLI.**
   *Rejected:* defeats the entire premise. Agents calling subprocesses
   is a fragile hack and gives up structured streaming.

## Notes

Design constraint that falls out of this ADR: the Service interface
must use **plain serializable types** in its method signatures —
`domain.Goal`, `domain.Plan`, `domain.QuestID`, etc. No `chan T`, no
private fields, no Go-only union via interface. Streaming exposes a
`func(Event)` callback or returns a typed iterator that both HTTP-SSE
and MCP-streaming can adapt. See
[design/service-layer.md](../design/service-layer.md) for the
interface definition and the streaming pattern.

For "the agent surface should be smaller and more curated than the
human one," see [design/mcp.md](../design/mcp.md) — MCP exposes the
~7 verbs that compose a quest, not every Service method. That's the
front-end's job, not the Service interface's.
