# MCP front-end

The Model Context Protocol front-end is the agent-facing transport for
resy-snipe v2. It is a peer to the HTTP API, not a wrapper over it
([ADR-0004](../adr/0004-mcp-as-peer-front-end.md)). Both transports
consume the same in-process `Service` interface; this document
specifies what that interface looks like when projected as MCP tools,
resources, prompts, and notifications.

Reader's mental model: every MCP tool handler is a thin adapter that
parses JSON, calls one `Service` method, and marshals the result.
There is no MCP-specific business logic — if a behaviour cannot be
expressed at the Service layer, it does not belong in MCP.

## 1. Purpose

Expose the Service-layer verbs as MCP tools and resources so Claude,
Claude Code, and future LLM agents can drive resy-snipe natively. The
agent surface is **strictly smaller** than the full `Service`
interface; it is the curated subset that composes a quest and reports
its progress. Operator-only verbs (invite, rotate, audit) are not
exposed — see §13.

## 2. Transports

MCP defines two transports; we implement both.

- **stdio** (`resy-snipe mcp`). A subprocess Claude Code spawns per
  session. Reads JSON-RPC on stdin, writes on stdout, logs on stderr.
  Authenticates from the env var `RESY_SNIPE_TOKEN` or, if absent,
  from `~/.config/resy-snipe/token`. Exits when the parent closes
  stdin. Connects to the daemon over the same loopback Unix socket
  the CLI uses; the subprocess holds **no** Service state of its own
  — it is a JSON-RPC framing layer that proxies into the daemon's
  `Service` over the local socket.
- **Streamable HTTP** (`POST /mcp`, served by the daemon). Long-lived,
  daemon-attached. Used by cloud-resident agents that talk to the
  homelab over Tailscale ([ADR-0007](../adr/0007-self-hosted-only-no-saas.md)).
  Authenticates with `Authorization: Bearer <token>` exactly as the
  HTTP API does ([design/http-api.md](http-api.md)).

Both transports share one server implementation in `internal/mcp`;
the only difference is the framing layer.

## 3. Why two transports

stdio is the right shape for **local** Claude Code: each user's
editor spawns its own subprocess, lifecycle is owned by the editor,
no port to expose, no mTLS to configure. The user's token is already
on their machine.

Streamable HTTP is the right shape for **remote** agents: a
long-lived agent in the cloud (a "watcher" running on a phone, a
scheduled Claude job) cannot fork a subprocess on the homelab. It
opens a persistent HTTP/2 stream to `https://snipe.phall.example/mcp`
over Tailscale and receives notifications on the same channel.

Both consume the daemon's `Service`. Neither speaks to the other.
The daemon's HTTP API and the daemon's MCP HTTP transport are
distinct routes (`/v1/...` vs `/mcp`) on the same listener.

## 4. Tools

The agent surface is seven tools. Each is one `Service` method,
one-to-one. Inputs and outputs are JSON Schema'd; tool handlers are
~10 lines of Go (parse, call, marshal). Tool names are
`snake_case_lowercase`, MCP convention.

### `resolve_venue`

Resolve a Resy URL, slug, or human-readable query to a canonical
`Venue` with cached metadata (timezone, known release windows, party
sizes, recent observed drops). Idempotent, no side effects.

**When to call**: as the first step of any quest creation, to confirm
the agent and user are talking about the same venue. Also call it
standalone when the user asks "what do we know about Carbone?".

**Input schema**:

```json
{
  "type": "object",
  "properties": {
    "query": { "type": "string", "minLength": 1 },
    "hint": {
      "type": "object",
      "properties": {
        "city":    { "type": "string" },
        "country": { "type": "string", "enum": ["US", "UK", "CA"] }
      }
    }
  },
  "required": ["query"]
}
```

**Output**: a `Venue` (see [design/domain.md](domain.md)) plus
`candidates: Venue[]` if the query was ambiguous (≥ 2 plausible
matches above the resolver's confidence threshold).

**Error semantics**: `venue_not_found` if no candidate cleared the
threshold; `ambiguous` if two or more tied — agent should re-prompt
the user with the candidate list.

### `plan_quest`

Pure function. Given a `Goal`, returns a `Plan`
([ADR-0012](../adr/0012-plan-first-ux.md)). No side effects, no
persistence, no scheduling. The Plan is hashable and the hash is
returned in the response.

**When to call**: every time before `create_quest`. The agent shows
the Plan to the user; the user confirms; the agent then calls
`create_quest` with the `plan_hash` pinned.

**Input schema**:

```json
{
  "type": "object",
  "properties": {
    "goal": {
      "type": "object",
      "properties": {
        "venue":         { "type": "string", "description": "URL, slug, or venue id" },
        "date":          { "type": "string", "format": "date" },
        "party_size":    { "type": "integer", "minimum": 1, "maximum": 20 },
        "earliest_time": { "type": "string", "pattern": "^\\d{2}:\\d{2}$" },
        "latest_time":   { "type": "string", "pattern": "^\\d{2}:\\d{2}$" },
        "preferred_slot_types": {
          "type": "array",
          "items": { "type": "string", "enum": ["dining-room", "bar", "patio", "any"] }
        },
        "strategy_hint": {
          "type": "string",
          "enum": ["explicit", "discovered", "continuous", "auto"],
          "default": "auto"
        }
      },
      "required": ["venue", "date", "party_size"]
    }
  },
  "required": ["goal"]
}
```

**Output**: a `Plan` with `hash`, `drop_moment`, `strategy`,
`fire_schedule`, `signing_required`, `notes`. See ADR-0012 §Plan.

**Error semantics**: `venue_not_found`, `goal_invalid`,
`unsupported_strategy_for_venue` (e.g. user asked for `explicit` but
no observed release window exists; planner suggests `discovered`).

### `create_quest`

Persists and schedules a Quest. Requires either `plan_hash` (pinned
to a previously computed Plan, daemon recomputes and refuses on
drift) or explicit `confirm: true` (server inlines a fresh plan and
commits). One of the two **must** be present;
`create_quest({goal})` with neither is a hard error.

**When to call**: only after the user has explicitly approved the
Plan. Never call this from a tool-completion loop without user
acknowledgement.

**Input schema**:

```json
{
  "type": "object",
  "properties": {
    "goal":            { "$ref": "#/definitions/Goal" },
    "plan_hash":       { "type": "string", "pattern": "^[a-f0-9]{64}$" },
    "confirm":         { "type": "boolean" },
    "idempotency_key": { "type": "string", "minLength": 8, "maxLength": 64 }
  },
  "required": ["goal"],
  "oneOf": [
    { "required": ["plan_hash"] },
    { "required": ["confirm"]   }
  ]
}
```

**Output**: `{ "quest_id": "qst_...", "state": "Pending", "plan": Plan }`.

**Error semantics**: `plan_drift` (hash mismatch — agent must
re-`plan_quest`), `goal_conflicts_with_existing_quest` (idempotency
collision with a different goal), `quota_exceeded`. Returns the
existing quest unchanged on idempotency-key replay with identical
goal.

### `get_quest`

Read-only fetch of a single Quest by id, including current state and
the most recent N events (default 20). For full event history, use
the `resy://quests/{id}/events` resource.

**Input**:

```json
{
  "type": "object",
  "properties": {
    "quest_id":         { "type": "string", "pattern": "^qst_[A-Za-z0-9]+$" },
    "include_events_n": { "type": "integer", "minimum": 0, "maximum": 200, "default": 20 }
  },
  "required": ["quest_id"]
}
```

**Output**: `QuestState` with embedded recent events.

### `list_quests`

Returns `QuestSummary[]` for the authenticated user, optionally
filtered by state, venue, or date range. Pagination via `cursor`.

**Input**:

```json
{
  "type": "object",
  "properties": {
    "filter": {
      "type": "object",
      "properties": {
        "state":      { "type": "array", "items": { "type": "string", "enum": ["Pending","Armed","Firing","Won","Lost","Cancelled","Failed"] } },
        "venue_id":   { "type": "string" },
        "date_from":  { "type": "string", "format": "date" },
        "date_to":    { "type": "string", "format": "date" }
      }
    },
    "cursor": { "type": "string" },
    "limit":  { "type": "integer", "minimum": 1, "maximum": 100, "default": 20 }
  }
}
```

**Output**: `{ "quests": QuestSummary[], "next_cursor": string|null }`.

### `cancel_quest`

Idempotent. Cancels a Pending or Armed quest. No effect on a quest
that has already terminated. The `reason` is recorded in the audit
log and surfaced in events.

**Input**:

```json
{
  "type": "object",
  "properties": {
    "quest_id": { "type": "string" },
    "reason":   { "type": "string", "maxLength": 500 }
  },
  "required": ["quest_id"]
}
```

**Output**: `{ "quest_id": "...", "state": "Cancelled", "cancelled_at": "..." }`.

**Error semantics**: `quest_already_terminal` is **not** an error —
returns the current state. `quest_not_found` and
`forbidden_other_user` are.

### `simulate`

Agent-facing what-if. Given a Goal, runs the Planner and the engine's
fire-schedule synthesizer in dry-run mode against the venue's last
known state, returning a `SimulationReport`: predicted drop moment,
expected wins/losses against simulated load, recommended strategy,
and the would-be `fire_schedule`. **Not in the HTTP API yet**; this
is an MCP-only tool because its primary consumer is the agent
("would this work? what's the risk?").

**Input**: same shape as `plan_quest`, plus optional
`scenarios: ("typical"|"high_demand"|"observed_history")[]`.

**Output**:

```json
{
  "predicted_drop":     "2026-05-15T15:00:00Z",
  "drop_confidence":    0.82,
  "recommended_strategy": "explicit",
  "win_probability":    0.41,
  "fire_schedule_preview": [ "2026-05-15T14:59:59.500Z", "2026-05-15T14:59:59.700Z" ],
  "notes": [
    "Last 14 observed releases for this venue all landed within 250ms of T-0.",
    "Signing will be required (PerimeterX challenge observed in 12/14 prior runs)."
  ]
}
```

`simulate` is read-only; it never persists or schedules.

## 5. Resources

Resources are read-only and URI-addressable. The agent fetches them
with `resources/read`; the agent may also subscribe with
`resources/subscribe` for those that change (quests, events).

| URI                                | Returns                                   |
| ---------------------------------- | ----------------------------------------- |
| `resy://venues/{id}`               | Resolved venue with cached metadata       |
| `resy://accounts`                  | Resy accounts owned by the authed user    |
| `resy://quests`                    | All quests for the authed user (summary)  |
| `resy://quests/{id}`               | Quest detail + last N events              |
| `resy://quests/{id}/events`        | Full event history, paged                 |

`resy://accounts` exposes only the public-safe shape: `account_id`,
`email`, `created_at`, `last_used_at`, never the password or session
cookies. The Service layer enforces this projection — MCP does not
filter, the Service simply does not return the secret fields.

`resy://quests/{id}/events` supports `page` and `since_event_id`
query suffixes (e.g. `resy://quests/qst_abc/events?since=42`) for
incremental polling fallback when subscriptions are unavailable.

## 6. Prompts

MCP prompts are pre-baked templates the server offers the client.
We ship three:

- **"Snipe a reservation"** — collects venue, date, party size, time
  window; runs `resolve_venue` → `plan_quest`; presents Plan; asks
  to confirm; runs `create_quest`.
- **"Watch for cancellations"** — collects venue + date range; runs
  `plan_quest` with `strategy_hint: continuous`; subscribes to
  events.
- **"Postmortem this quest"** — takes a `quest_id`; reads
  `resy://quests/{id}/events`; produces a natural-language summary
  of why the quest landed as it did. See §10.

Prompts are advisory: agents are free to ignore them. They exist so
a fresh Claude session can offer the user "want to snipe a res?" as
a one-click affordance.

## 7. Streaming and notifications

The server emits `notifications/quest_state_changed` whenever the
underlying `Service.SubscribeQuest` callback fires. The notification
is keyed by `quest_id`; the client subscribes per-quest via
`resources/subscribe` on `resy://quests/{id}`.

Notification shape:

```json
{
  "method": "notifications/quest_state_changed",
  "params": {
    "quest_id":  "qst_8xK3aZ2qR",
    "state":     "Firing",
    "event_id":  17,
    "event": {
      "kind":      "PrepareSlotAttempt",
      "at":        "2026-05-15T14:59:59.812Z",
      "attempt":   3,
      "outcome":   "challenge_required",
      "details":   { "challenge_provider": "perimeterx" }
    },
    "ts": "2026-05-15T14:59:59.815Z"
  }
}
```

Implementation: the MCP server holds one `chan domain.Event`
subscription per `(client, quest_id)` pair, supplied by
`Service.SubscribeQuest(ctx, questID, callback)`. When the callback
fires, the server marshals and pushes a notification on the
JSON-RPC connection. On client disconnect (stdio EOF or HTTP stream
close), the server cancels its subscription and `SubscribeQuest`
returns. No goroutine outlives the connection — see laws.md §11.

The MCP transport adapter for events is the same shape as the
HTTP-SSE adapter, by design ([ADR-0004](../adr/0004-mcp-as-peer-front-end.md)
notes). Both wrap one `Service.SubscribeQuest` call.

## 8. Auth flow

Every MCP method except `serverInfo` requires authentication. The
authenticated session is scoped to the bearer token's user:
`list_quests` returns only that user's quests; `create_quest` files
the new quest under that user; `cancel_quest` rejects with
`forbidden_other_user` for quests the caller does not own
([ADR-0011](../adr/0011-operator-issued-invites-no-self-registration.md)).

Token sources:

- HTTP transport: `Authorization: Bearer <token>` header on every
  request, validated on each call. No cookie auth.
- stdio transport: `RESY_SNIPE_TOKEN` env var, or
  `~/.config/resy-snipe/token` file (mode 0600). The subprocess reads
  the token at startup and forwards it to the daemon over the local
  Unix socket as a `mcp.attach` JSON-RPC call (one round-trip, before
  any tool calls). The daemon validates and binds a Service session
  to the connection.

Anonymous calls allowed: `serverInfo` (returns protocol version,
capabilities, server build hash). Used by Claude to negotiate.

There is no `login` tool. Interactive password capture is a human
flow, not an agent flow ([ADR-0011](../adr/0011-operator-issued-invites-no-self-registration.md)
§Notes). Agents inherit the user's already-issued bearer token.

## 9. Plan-first agent flow

The killer UX. Walk-through of the canonical interaction.

User says:

> snipe astoria-dc next friday for 2 around 7pm

The agent's tool-calls, in order:

**1. Resolve the venue.**

```json
// → resolve_venue
{
  "name": "resolve_venue",
  "arguments": {
    "query": "astoria-dc",
    "hint": { "city": "Washington", "country": "US" }
  }
}
```

```json
// ← result
{
  "content": [{ "type": "text", "text": "Resolved Astoria (Washington, DC)" }],
  "structured_content": {
    "venue": {
      "id":       "rsv_astoria_dc",
      "name":     "Astoria",
      "city":     "Washington, DC",
      "tz":       "America/New_York",
      "known_release_windows": ["T-30d 10:00 ET"]
    }
  },
  "is_error": false
}
```

**2. Plan the quest.**

```json
// → plan_quest
{
  "name": "plan_quest",
  "arguments": {
    "goal": {
      "venue":         "rsv_astoria_dc",
      "date":          "2026-05-22",
      "party_size":    2,
      "earliest_time": "18:30",
      "latest_time":   "20:00"
    }
  }
}
```

```json
// ← result
{
  "content": [{ "type": "text", "text": "Planned: drop at 2026-04-22T14:00:00Z, strategy=explicit" }],
  "structured_content": {
    "plan": {
      "hash":         "9f1c4a...sha256",
      "drop_moment":  "2026-04-22T14:00:00Z",
      "strategy":     "explicit",
      "fire_schedule": [
        "2026-04-22T13:59:59.500Z",
        "2026-04-22T13:59:59.700Z",
        "2026-04-22T13:59:59.900Z"
      ],
      "signing_required": true,
      "notes": [
        "Astoria's release window is well-observed; 14/14 prior drops within 200ms of T-0.",
        "PerimeterX challenge expected; signer will be engaged."
      ]
    }
  },
  "is_error": false
}
```

**3. Agent presents the Plan to the user.** "Here's what I'll do.
Drop is April 22 at 10am ET — about 4 hours from now. I'll fire
three attempts spaced 200ms apart. Confirm?"

**4. User confirms.**

**5. Create the quest, pinning the hash.**

```json
// → create_quest
{
  "name": "create_quest",
  "arguments": {
    "goal":            { /* same as plan_quest */ },
    "plan_hash":       "9f1c4a...sha256",
    "idempotency_key": "claude-2026-05-10T19:42-astoria-2"
  }
}
```

```json
// ← result
{
  "content": [{ "type": "text", "text": "Quest qst_8xK3aZ2qR armed for 2026-04-22T14:00:00Z." }],
  "structured_content": { "quest_id": "qst_8xK3aZ2qR", "state": "Armed", "plan": { /* echoed */ } },
  "is_error": false
}
```

**6. Subscribe.**

```json
// → resources/subscribe
{ "uri": "resy://quests/qst_8xK3aZ2qR" }
```

The agent now sleeps until `notifications/quest_state_changed`
arrives (see §7), at which point it formats and sends a message to
the user.

If the venue's `/4/find` snapshot has changed between plan and
create, `create_quest` returns `plan_drift`; the agent re-runs
`plan_quest` and re-prompts the user with the new Plan.

## 10. Postmortem flow

After a quest reaches a terminal state, the user can ask "what
happened?". The agent's job is the natural-language summary; the
events are already structured.

```json
// → resources/read
{ "uri": "resy://quests/qst_8xK3aZ2qR/events" }
```

The events come back as a JSON array. The agent reads them in order,
notes the failure points, and produces text like:

> Carbone quest lost the race by 80ms on the third PrepareSlot
> retry. The first two attempts hit the PerimeterX challenge and the
> signer succeeded both times; the third PrepareSlot returned
> `slot_taken` 80ms after the second won the race for a different
> user. See event 17 for the timing detail.

The agent does not need to interpret raw HTTP responses — every
event has `kind`, `outcome`, and a structured `details` payload. The
agent's leverage is *language*, not *parsing*.

The "Postmortem this quest" prompt (§6) is exactly this flow,
templated.

## 11. Standing-watch flow

For continuous quests ("watch for cancellations at Carbone for the
next 30 days"), the agent creates a `Continuous`-strategy quest,
subscribes, and walks away.

```json
// → create_quest
{ "name": "create_quest", "arguments": {
    "goal": { "venue": "carbone-nyc", "date": "2026-06-01..2026-06-30",
              "party_size": 2, "strategy_hint": "continuous" },
    "plan_hash": "..."
}}
// → resources/subscribe
{ "uri": "resy://quests/qst_continuous_xyz" }
```

When the engine's continuous poll sees a slot, the engine fires;
the quest transitions to `Won` (or `Lost` on race loss); the
notification fires; Claude wakes (via Claude Code's MCP
notification handler or, for cloud agents, the streamable HTTP
push), pings the user.

The agent does not poll. The daemon does the polling work; the
agent receives one push when state changes. This is the difference
between v1 ("you keep your laptop open") and v2 ("the homelab keeps
its lights on").

## 12. Tool schemas (consolidated)

JSON Schema definitions referenced inline above. The published
schemas live in `internal/mcp/schemas/*.json`; the daemon's
`tools/list` response embeds them so MCP clients can validate
client-side. Schemas are checked into the repo and treated as a
versioned public contract — see §15.

The `Goal` definition referenced by `create_quest`:

```json
"Goal": {
  "type": "object",
  "properties": {
    "venue":         { "type": "string" },
    "date":          { "type": "string", "format": "date" },
    "party_size":    { "type": "integer", "minimum": 1 },
    "earliest_time": { "type": "string" },
    "latest_time":   { "type": "string" },
    "preferred_slot_types": { "type": "array", "items": { "type": "string" } },
    "strategy_hint": { "type": "string" }
  },
  "required": ["venue", "date", "party_size"]
}
```

## 13. What is NOT exposed to MCP

The following are deliberately absent from the MCP surface:

- **`InviteUser`, `RotateToken`, `RevokeToken`** — operator-only,
  HTTP API only ([ADR-0011](../adr/0011-operator-issued-invites-no-self-registration.md)).
  Agents that automate user management are out of scope.
- **`Login`** — interactive password capture is a human flow. Agents
  inherit a token; they do not collect credentials.
- **Audit log read** — `GET /v1/admin/audit` is admin-only HTTP. An
  agent acting on the operator's behalf can read it via HTTP, but
  that is the operator's choice on the operator's session, not a
  general agent affordance.
- **Direct DB access** — never. The Service is the contract.
- **Provider-internal verbs** — `resy.SignPayload`, `resy.PrepareSlot`
  raw, etc. The agent operates at the quest layer; the engine
  operates at the provider layer.

If a future verb is operator-only, it goes on the HTTP API and not
on MCP. If it is agent-relevant, it goes on both.

## 14. Error model

Every tool error is structured:

```json
{
  "is_error": true,
  "content": [{ "type": "text", "text": "Plan hash drift detected; replan." }],
  "structured_content": {
    "code":        "plan_drift",
    "message":     "Plan hash drift detected; replan.",
    "retry_after": null,
    "details": {
      "expected_hash": "9f1c4a...",
      "current_hash":  "ab7e02..."
    }
  }
}
```

`code` is one of the Service-layer sentinels
([design/service-layer.md](service-layer.md) §errors), mapped 1:1.
The MCP transport adds nothing — it does not translate, it does not
swallow, it does not enrich. `errors.Is(err, service.ErrPlanDrift)`
in the handler becomes `code: "plan_drift"` in the response, full
stop.

`retry_after` is set on `quota_exceeded` and `provider_throttled`;
otherwise null.

The MCP `is_error: true` convention is honoured per protocol;
agents that ignore `is_error` and only read `structured_content` are
still safe because the `code` field unambiguously says "this is a
failure".

## 15. Versioning

The server's `serverInfo` response declares:

```json
{
  "protocolVersion": "2024-11-05",
  "serverInfo": {
    "name":    "resy-snipe",
    "version": "v2.3.1+abc1234",
    "resy_snipe_mcp_api_version": "1"
  },
  "capabilities": {
    "tools":     { "listChanged": true },
    "resources": { "subscribe": true, "listChanged": true },
    "prompts":   { "listChanged": false },
    "logging":   {}
  }
}
```

`resy_snipe_mcp_api_version` is **our** version of the tool surface,
independent of the MCP protocol version. Bumped when a tool's input
or output schema changes in a non-additive way. Additive changes
(new optional input field, new optional output field) do not bump
it; the daemon emits a `tools/list_changed` notification so clients
re-read the schemas.

The daemon binary version (`v2.3.1+abc1234`) tracks the build; agents
that want strict pinning can refuse to operate on a build hash they
don't recognize. Friends-and-family scale, this is rarely needed.

## 16. Implementation sketch

Go package `internal/mcp`:

```go
package mcp

import (
    "context"
    "io"
    "net/http"

    "resy-snipe/internal/service"
)

type Server struct {
    svc service.Service
    log *slog.Logger
}

func NewServer(svc service.Service, log *slog.Logger) *Server {
    return &Server{svc: svc, log: log}
}

func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
    // JSON-RPC framing, dispatch into s.handleTool / s.handleResource
}

func (s *Server) ServeHTTP(ctx context.Context) http.Handler {
    // Streamable HTTP framing, same dispatch
}
```

Each tool handler is a thin function:

```go
func (s *Server) handlePlanQuest(ctx context.Context, raw json.RawMessage) (toolResult, error) {
    var in struct{ Goal domain.Goal `json:"goal"` }
    if err := json.Unmarshal(raw, &in); err != nil {
        return errResult(service.ErrGoalInvalid, err.Error())
    }
    plan, err := s.svc.PlanQuest(ctx, in.Goal)
    if err != nil {
        return mapErr(err) // sentinel → MCP code
    }
    return okResult(plan), nil
}
```

The dispatch table is a `map[string]func(context.Context, json.RawMessage) (toolResult, error)`
populated at construction. Adding a tool is: add a method on
`Service`, add a handler, add a JSON Schema file, register in the
table. ~30 lines per tool.

Streaming uses the same pattern as the HTTP-SSE adapter; both wrap
`Service.SubscribeQuest` and translate to the wire format.

The two transports share **all** of this code; only the framing
(stdio bytes vs HTTP request/response stream) differs.

## 17. Test plan

Three layers of test coverage.

**Unit / round-trip**: an in-memory MCP client driving an in-memory
MCP server backed by a fake `Service` (canned `PlanQuest` returns,
canned `CreateQuest` returns, canned event channels). For every
tool: assert the request schema validates, the response schema
validates, and the call reaches the fake Service with the expected
arguments.

**Schema validation**: a CI gate that loads every JSON Schema in
`internal/mcp/schemas/`, validates them as JSON Schema draft 2020-12,
and validates a corpus of example payloads (`testdata/mcp/*.json`)
against them. Catches the "I changed the handler but forgot the
schema" class of bugs.

**Streaming integration**: stand up the daemon with a fake provider
that replays a recorded quest's event sequence; connect an MCP
client; subscribe to the quest; assert every emitted event arrives
as a `notifications/quest_state_changed` in order, with monotonic
`event_id`. Run on both stdio and HTTP transports — same test
harness, two transport adapters.

A separate end-to-end test runs Claude Desktop's actual MCP client
against the daemon over stdio with a recorded fixture. Not in CI;
run manually before each release. Catches the "we ship valid MCP
but Claude can't actually use it" class of bugs.

---

**Cross-references**:
[ADR-0004 MCP as peer front-end](../adr/0004-mcp-as-peer-front-end.md) ·
[ADR-0011 Operator-issued invites](../adr/0011-operator-issued-invites-no-self-registration.md) ·
[ADR-0012 Plan-first UX](../adr/0012-plan-first-ux.md) ·
[design/service-layer.md](service-layer.md) ·
[design/http-api.md](http-api.md) ·
[design/planner.md](planner.md) ·
[architecture.md](../../architecture.md) ·
[laws.md](../../laws.md)
