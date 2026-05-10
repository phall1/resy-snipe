# Milestone M3 — MCP front-end

**Status**: Scoped (sub-issues materialize when M2 closes)
**Owner**: phall
**Beads epic**: `resy-snipe-0li`
**Depends on**: M2 (HTTP transport must exist; MCP-HTTP shares the auth
flow)
**Blocks**: nothing — M4 and M5 are parallel

## Goal in one sentence

Make the system natively callable by Claude / Claude Code / future
LLM agents via MCP, with the plan-first agent UX from ADR-0012 wired
end-to-end.

## What ships

- `resy-snipe mcp` subcommand — stdio MCP server.
- `resy-snipe serve` daemon also exposes Streamable HTTP MCP at
  `/mcp/`.
- Tool surface per `design/mcp.md` §tools: `resolve_venue`,
  `plan_quest`, `create_quest`, `get_quest`, `list_quests`,
  `cancel_quest`, `simulate`.
- Resources: `resy://venues/{id}`, `resy://quests`, `resy://quests/{id}`,
  `resy://quests/{id}/events`, `resy://accounts`.
- Streaming notifications: `quest_state_changed` over MCP for any
  subscribed quest.
- Token-based auth from env (`RESY_SNIPE_TOKEN`) for stdio; Bearer
  header for HTTP-MCP.
- Example Claude Code `mcp.json` config snippet in `docs/v2/`.

## What does NOT ship in M3

- TUI (M4).
- Notifications outside MCP (M4).
- Login via MCP (operator-only — see ADR-0011 reasoning, design/mcp.md
  §what-not-to-expose).

## Acceptance criteria

- Claude Code, given the `mcp.json` snippet pointing at `resy-snipe
  mcp`, can call all 7 tools.
- The plan-first flow works end-to-end: agent calls `plan_quest`, shows
  user the Plan, calls `create_quest` with `plan_hash`. Hash mismatch
  is surfaced as a structured MCP error.
- A subscribed agent receives `quest_state_changed` notifications when
  a quest transitions states, without polling.
- The agent surface is **not** a superset of HTTP — admin verbs are
  invisible to MCP. Verified by inspecting the published tool list.

## Sub-issues

Filed when M2 closes. Sketch:

- MCP stdio transport
- MCP Streamable HTTP transport
- Tool handlers (one per Service verb in scope)
- Resource handlers (read-only Service.Get/List wrappers)
- Notification fan-out from engine subscribe to MCP
  `notifications/quest_state_changed`
- Tool/resource JSON Schema definitions
- `docs/v2/agent-quickstart.md` (Claude Code attach + first quest)
- E2E test: an in-memory MCP client driving the full plan→create→event
  flow.

## Cross-references

- ADRs: 0004, 0011, 0012
- Design: mcp.md, service-layer.md, daemon.md
