// Package domain contains the core, dependency-free types that describe a
// snipe: Intent, SnipeState, SlotPayload union, Venue, BookingPolicy,
// ReleaseStrategy union, and Event. The v2 goal-driven additions
// (Goal, VenueQuery, TimeWindow, Constraints) and the planner artifact
// (Plan, with content-addressable Hash) also live here. These types
// are shared between the engine, providers, and store and must not
// import any sibling internal/ package.
package domain
