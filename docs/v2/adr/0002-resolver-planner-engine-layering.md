# ADR 0002: Resolver + Planner + Engine as three serial layers

**Status**: Accepted
**Date**: 2026-05-10
**Decision-makers**: phall
**Related**: [ADR-0001](0001-goal-driven-architecture.md),
[design/overview.md](../design/overview.md),
[design/resolver.md](../design/resolver.md),
[design/planner.md](../design/planner.md)

## Context

[ADR-0001](0001-goal-driven-architecture.md) commits to a separate
`Goal` and `Intent`. The transformation `Goal → Intent` decomposes
naturally into two distinct concerns:

1. **Identity** — "who is this venue?" (URL/slug/name → `domain.Venue`).
2. **Plan** — "when and how do we book it?" (`Goal + Venue → Intent`).

Both touch the network (`/3/venue` for resolution, `/4/find` for
planning), both have caching opportunities, both fail in different
ways. Forcing them through a single function would re-create the v1
problem of one layer knowing too much.

## Decision

Three layers, called serially:

```
Goal ──► Resolver ──► (Goal, Venue) ──► Planner ──► Intent ──► Engine
```

- **`internal/resolver`** owns `VenueQuery → domain.Venue`. New package.
- **`internal/planner`** owns `(Goal, Venue) → domain.Intent`. New
  package.
- **`internal/engine`** owns `Intent → execution`. Exists today,
  unchanged.

The Service layer ([design/service-layer.md](../design/service-layer.md))
composes them. No layer reaches across — the Engine never resolves a
venue, the Resolver never plans, the Planner never books.

## Consequences

**Positive**
- Each layer is independently testable with fixtures it controls.
  Resolver tests don't need a Planner; Planner tests use a fake
  `Venue`.
- Caching is layered cleanly: Resolver caches `(slug,city) → Venue` for
  hours; Planner caches `(venue, date) → drop_moment` for the duration
  of a quest; Engine caches nothing (each Intent is one-shot).
- Replan is a partial recomputation: Resolver result usually still
  valid, Planner re-runs against fresh `/4/find`, Engine restarts.
- Adds a clean seam for future provider expansion: an OpenTable
  Resolver + Planner produce Resy-shaped Intents the Engine doesn't
  notice.

**Negative**
- Three packages where v1 had one (the CLI's `toIntent`).
- Two extra interfaces to mock in integration tests.

**Neutral**
- Layering rules remain identical to v1's
  [docs/laws.md](../../laws.md): Resolver and Planner sit between
  `cmd/`/Service and Engine, both depend only on `domain` + adapters,
  neither imports `internal/resy` directly (they use a provider seam).

## Alternatives considered

1. **Single `Planner` package that owns both.** *Rejected:* breaks the
   "venue identity is a user-facing concept" framing. Users want to
   resolve a URL without committing to a plan ("just tell me what this
   place is"). Folding it into Planner means MCP can't expose
   `resolve_venue` as a clean tool.
2. **A `VenueDirectory` package that's identity + release config + cache,
   and a `Scheduler` that's just the Goal→Intent function.** *Rejected:*
   same package count, worse names. "Resolver" and "Planner" map to
   verbs; "VenueDirectory" implies it owns the venue list (it doesn't —
   Resy does).
3. **Inline both into the Service layer.** *Rejected:* business logic
   in a wiring layer is a smell ([Law 4](../../laws.md)). The Service
   layer is RPC plumbing; planning is domain logic.

## Notes

The Planner's external dependency on `/4/find` is mediated by the same
`providers.Provider` interface the Engine uses, so the test seam is
already there. The Resolver's dependency on `/3/venue` is a new
addition to the provider interface — see
[design/resolver.md](../design/resolver.md) for the contract.
