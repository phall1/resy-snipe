package planner

import (
	"errors"
	"fmt"
	"time"

	"resy-snipe/internal/domain"
)

// ErrInvalidReleaseDaysAhead is returned by ComputeDrop when the caller
// passes a negative releaseDaysAhead. A zero value is valid and means
// "the venue releases bookings on the target date itself"; a positive
// value is the common case ("30 days ahead"); a negative value has no
// meaningful interpretation in the release-window model.
var ErrInvalidReleaseDaysAhead = errors.New("planner: releaseDaysAhead must be >= 0")

// ErrMissingLocation is returned by ComputeDrop when the caller passes
// a nil *time.Location. The planner refuses to fabricate a default; the
// venue's tz is load-bearing for correctness (per design/planner.md
// §drop-moment-math: "anchoring in the venue's location first").
var ErrMissingLocation = errors.New("planner: tz is required")

// ComputeDrop converts (target reservation date, days-offset,
// venue-local time-of-day, venue tz) into the UTC instant the engine
// should fire on, per design/planner.md §drop-moment-math.
//
// Algorithm:
//
//  1. Anchor the release date to the venue's calendar:
//     anchorDate = target.AddDate(0, 0, -releaseDaysAhead) in tz.
//     This treats releaseDaysAhead as calendar days (not 24h*N), which
//     matters across DST transitions — Go's AddDate operates on the
//     calendar, not the absolute clock.
//  2. Compose a wall-clock instant at the venue's local time-of-day on
//     the anchor date.
//  3. Project to UTC; engine wake-ups are UTC throughout.
//
// Worked example (design/planner.md §worked-example):
//
//	target            = 2026-06-09
//	releaseDaysAhead  = 30
//	releaseTimeLocal  = 00:00:00
//	tz                = America/New_York
//	→ anchor          = 2026-05-10
//	→ drop in tz      = 2026-05-10T00:00:00 EDT (UTC-4)
//	→ drop.UTC()      = 2026-05-10T04:00:00Z
//
// DST behavior is delegated to Go's stdlib time.Date:
//
//   - Spring-forward gap (e.g. 2026-03-08 02:00→03:00 in NY): a wall
//     time like 02:30 does not exist on the calendar. Go's time.Date
//     keeps the pre-transition zone offset (EST, UTC-5) and produces
//     the absolute instant equivalent to "01:30 EST" = 06:30Z — i.e.
//     it does not silently advance into EDT. We accept this behavior
//     unchanged: it is deterministic, documented in the stdlib, and
//     matches the design's stance that "DST is a non-issue because we
//     anchor to the venue-local date and construct the wall-clock
//     instant inside tz". Operators who care about the gap should set
//     a wall time outside it.
//   - Fall-back overlap (e.g. 2026-11-01 01:30 happens twice in NY):
//     time.Date picks the earlier of the two occurrences (the one still
//     in EDT), matching common operator expectations that "01:30 on
//     fall-back day" means the first 01:30.
//
// ComputeDrop is a pure function: no I/O, no clock reads, no package
// globals, no wall-clock dependencies of any kind.
func ComputeDrop(
	target domain.Date,
	releaseDaysAhead int,
	releaseTimeLocal domain.WallTime,
	tz *time.Location,
) (time.Time, error) {
	if tz == nil {
		return time.Time{}, ErrMissingLocation
	}
	if releaseDaysAhead < 0 {
		return time.Time{}, fmt.Errorf("%w: got %d", ErrInvalidReleaseDaysAhead, releaseDaysAhead)
	}

	// Step 1 — anchor on the venue's calendar.
	anchor := target.In(tz).AddDate(0, 0, -releaseDaysAhead)

	// Step 2 — compose wall-clock instant in tz.
	drop := time.Date(
		anchor.Year(), anchor.Month(), anchor.Day(),
		releaseTimeLocal.Hour,
		releaseTimeLocal.Minute,
		releaseTimeLocal.Second,
		0, // no sub-second precision in WallTime
		tz,
	)

	// Step 3 — project to UTC.
	return drop.UTC(), nil
}

// DiscoveredProbeWindow brackets a polling window around an approximate
// drop instant, per design/planner.md §strategy-selection rule 3.
//
// Defaults:
//   - probeLead  = 90s (start polling 90 seconds before approxDrop)
//   - probeWidth = 5m  (poll for five minutes total)
//
// Returned times are in UTC. ApproxDrop is treated as the best
// estimate; the engine will record the actual observed instant when it
// first sees the target date available.
func DiscoveredProbeWindow(approxDrop time.Time) (probeFrom, probeUntil time.Time) {
	const (
		probeLead  = 90 * time.Second
		probeWidth = 5 * time.Minute
	)
	probeFrom = approxDrop.Add(-probeLead).UTC()
	probeUntil = probeFrom.Add(probeWidth)
	return probeFrom, probeUntil
}
