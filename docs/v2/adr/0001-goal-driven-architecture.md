# ADR 0001: Goal-driven architecture — separate `Goal` from `Intent`

**Status**: Accepted
**Date**: 2026-05-10
**Decision-makers**: phall
**Related**: [ADR-0002](0002-resolver-planner-engine-layering.md),
[design/overview.md](../design/overview.md),
[design/planner.md](../design/planner.md)

## Context

In v1, `domain.Intent` is both **what the user wants** ("a 7pm Friday
res at Astoria DC for 2") and **what the system is going to do** (fire
PrepareSlot+Book at this venue id, at this exact wall-clock moment,
using this release strategy). Conflating them forces the user to be the
planner: they look up venue ids, compute drop times from release
windows, and pick `explicit` vs `discovered` vs `continuous`. For a
tool whose value prop is "I shouldn't have to think about Resy
mechanics," that's the wrong contract.

## Decision

Split the user's request from the execution plan.

```go
// What the user wants. Stable across replans.
type Goal struct {
    VenueQuery   VenueQuery   // URL, slug+city, or name
    Date         Date         // target reservation date
    Party        int
    TimePrefs    TimeWindow   // e.g. 18:30–21:00, with priority order
    Account      AccountID    // which Resy login pays
    Constraints  Constraints  // table types, max wait, etc.
}

// What we'll do — produced by the Planner from a Goal.
type Intent struct { /* exists today, unchanged */ }
```

Goals are persisted (a Quest is a Goal + history). Intents are derived
artifacts that can be regenerated whenever we need to replan.

## Consequences

**Positive**
- Replanning is a function: given the same `Goal` plus updated venue
  state, produce a new `Intent`. No state migration.
- The agent surface (MCP) speaks `Goal`, which is much closer to how
  humans and LLMs naturally describe what they want.
- The CLI's job shrinks: parse a URL, read flags, build a `Goal`, hand
  it to the Service layer. No date/strategy math.
- `dry-run` becomes meaningful: "here's the Plan we'd execute," not
  "here's what you typed back at you."

**Negative**
- One more domain type and one more layer (the Planner). +~500 LOC.
- A Goal that resolves to "no available drop window" needs a clear
  failure mode — currently impossible because the user always specifies
  a snipe-time.

**Neutral**
- v1 `Intent` shape is preserved. Engine code is unchanged. The
  planner just becomes a new producer of Intents alongside the CLI's
  current `toIntent`.

## Alternatives considered

1. **Keep one type; add optional fields** (`Intent.SnipeTime` becomes
   `*time.Time`, planner fills it if nil). *Rejected:* a single type
   pretending to be two leaks ambiguity into every consumer ("did the
   user pin this or did the planner derive it?"). Forces every
   downstream check to disambiguate.
2. **Computed Intents only; never persist Goals.** *Rejected:* breaks
   replan. If a venue's release time changes after we plan but before
   we fire, we can't recover the user's original request.
3. **A "GoalSpec" YAML/TOML config users hand-edit.** *Rejected:* this
   is a CLI/agent tool, not Kubernetes. Goals live in SQLite and are
   created via verbs.

## Notes

`Goal.Account` is required because rate-limiting in
[design/planner.md](../design/planner.md) is per-Resy-account. The
Planner needs to know which account it's planning for so the
`PerAccountRateLimiter` is the right one.

`VenueQuery` is a small union (URL / slug+city / freeform name) handled
by the Resolver — see [ADR-0002](0002-resolver-planner-engine-layering.md).
