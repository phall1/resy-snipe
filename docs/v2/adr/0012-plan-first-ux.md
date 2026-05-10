# ADR 0012: Plan is an artifact users approve before execution

**Status**: Accepted
**Date**: 2026-05-10
**Decision-makers**: phall
**Related**: [ADR-0001](0001-goal-driven-architecture.md),
[ADR-0002](0002-resolver-planner-engine-layering.md),
[ADR-0004](0004-mcp-as-peer-front-end.md),
[design/planner.md](../design/planner.md)

## Context

In v1, "submit" is one verb: parse flags, build Intent, run engine.
The user only sees what's about to happen by reading their own flags
back. There's no system-generated artifact saying "here's the plan I'm
about to execute" — and certainly no way for an agent to show a user
"is this what you want?" before committing.

For both human and agent UX, the system needs to produce a **Plan** —
a serializable, human-readable, hashable artifact derived from a Goal
+ Venue, that says exactly what the engine will do. The user (or
agent on the user's behalf) reviews the Plan, then commits.

## Decision

The Service layer exposes two distinct verbs:

- **`PlanQuest(goal) → Plan`** — pure function, no side effects, no
  persistence. Returns a Plan with a content-hash.
- **`CreateQuest(goal, [planHash]) → Quest`** — actually persists and
  schedules. If `planHash` is provided, the daemon recomputes the Plan
  and refuses if the hash doesn't match (prevents TOCTOU between
  "show user the plan" and "commit").

The Plan is a serializable snapshot of:

```go
type Plan struct {
    Goal           Goal           // echoed back, normalized
    Venue          Venue          // resolved venue, including tz
    DropMoment     time.Time      // when we'll fire (UTC, with confidence)
    Strategy       ReleaseStrategy// Explicit/Discovered/Continuous
    FireSchedule   []time.Time    // each (slot,time) attempt the engine
                                  // will issue, expanded
    SigningRequired bool          // best-guess based on venue + drop
    Hash           string         // sha256(canonicalized Plan)
    Notes          []string       // human-readable caveats from planner
                                  // ("Resy hasn't observed a release for
                                  //  this venue yet — strategy is
                                  //  Discovered with poll every 30s")
}
```

CLI: `resy-snipe quest plan <url>` prints the Plan as a table; user
inspects, then `resy-snipe quest create <url>` creates with the hash
pinned (prompted, or `--auto-confirm` for scripts).

MCP: `plan_quest` returns the Plan as a structured tool result;
agent presents it to the user; user says "yes"; agent calls
`create_quest` with the `plan_hash`.

## Consequences

**Positive**
- The agent UX is correct out of the box: Claude can never accidentally
  commit a quest the user hasn't seen.
- The Plan is a useful debugging surface — when a quest fails, the
  Plan + Events together explain everything.
- The hash catches drift: if the venue's release window changes between
  plan and create, create fails loudly and the agent re-plans.
- Replan during a long-lived quest is symmetric: same `PlanQuest` call,
  produces a new Plan, operator/agent decides whether to apply.

**Negative**
- One extra round-trip in the quest-creation flow. Negligible.
- Plan serialization shape is now public surface — changes need to
  preserve the hash invariant or explicitly opt out (the daemon emits
  a `plan_hash_version` so old hashes can be detected and re-planned).

**Neutral**
- The Planner ([design/planner.md](../design/planner.md)) is a pure
  function from `(Goal, Venue) → Plan` with explicit dependencies on
  the current time and the venue's `/4/find` snapshot. Both inputs
  are captured in the Plan so the hash is reproducible.

## Alternatives considered

1. **Single `CreateQuest(goal)` that persists immediately and shows
   the plan in the response.** *Rejected:* user can't dry-run.
   Agents that want to confirm have to "create and immediately delete
   if user disagrees," which is ugly and pollutes the audit log.
2. **`PlanQuest` returns a free-text human summary; no structured
   shape.** *Rejected:* the agent surface needs structure
   (`venue.name`, `drop_moment`, etc.) for the LLM to reason about.
   Free text loses that.
3. **`CreateQuest` always validates against a `goal_hash`, no separate
   plan step.** *Rejected:* the goal is small ("astoria-dc Friday"),
   the plan is large ("here's the 3 retry windows we'll fire at, here's
   the strategy choice"). Pinning the goal doesn't catch venue-state
   drift; pinning the plan does.

## Notes

The Plan hash uses canonical JSON (RFC 8785 JCS-style canonicalization)
to make it stable across SDK versions. Plans are not persisted by
default — only Quests are. A Plan re-derived from a stored Quest +
fresh venue state is the basis for the daemon's "should I replan?"
decision (see [design/planner.md](../design/planner.md) §replan).

CLI confirmation defaults to interactive (prints plan, asks "create?
[y/N]"). The `--yes` flag skips the prompt for scripts. The MCP
`create_quest` tool requires an explicit `plan_hash` argument to
succeed without confirmation — agents must either compute it from
their previous `plan_quest` result, or pass `confirm: true` which
inlines a fresh plan computation server-side and immediately commits
(useful for "create-without-prompting" agent flows where the user has
pre-authorized).
