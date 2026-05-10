// Package planner turns a user Goal plus a resolved Venue into a
// domain.ReleaseStrategy (and, eventually, a complete domain.Plan).
// The planner is a pure function of (Goal, Venue, clock, calendar
// snapshot, cached observations): no in-memory state survives a call,
// no I/O happens here. Callers fetch any provider state ahead of time
// and hand it in as input.
//
// Dependency rules (laws.md §Layering, design/planner.md §dependency-
// rules): this package may import internal/domain, internal/providers,
// internal/clock, and stdlib. It does NOT import internal/resy,
// internal/store, internal/resolver, internal/engine, internal/notify,
// or internal/service. The Service layer composes Resolver + Planner +
// Engine; the planner sits below that seam.
//
// M1-06 ships strategy selection only — the three-way decision among
// ContinuousRelease, ExplicitRelease, and DiscoveredRelease per
// design/planner.md §strategy-selection. Drop-moment math lands in
// M1-07; slot expansion and plan-hash wiring land in M1-08.
package planner
