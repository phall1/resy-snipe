package planner

import (
	"context"
	"fmt"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// VenueReleaseConfig is the operator/resolver-supplied prior on when a
// venue releases its calendar — release_days_ahead plus a venue-local
// time-of-day. M1-06 treats it as optional context the caller may
// supply; later milestones may grow a richer source (the resolver's
// /3/venue payload, an operator-curated table, etc.).
//
// Defined inside the planner package because domain.Venue does not yet
// carry these fields, and the design (planner.md §strategy-selection
// rule 2) keys strategy choice on the prior's presence. Once M1-01
// extends domain.Venue with a ReleaseConfig field this struct collapses
// into that one.
type VenueReleaseConfig struct {
	// DaysAhead is how many calendar days before the target reservation
	// date the venue opens bookings. A value of 30 means "the 2026-06-09
	// reservation opens on 2026-05-10".
	DaysAhead int
	// TimeLocal is the wall-clock time-of-day, in the venue's timezone,
	// at which the release fires.
	TimeLocal domain.WallTime
}

// IsZero reports whether c carries no useful information.
func (c VenueReleaseConfig) IsZero() bool { return c == VenueReleaseConfig{} }

// StrategyInput bundles every signal SelectStrategy reads. The caller
// (Service layer) is responsible for fetching CalendarSnapshot and
// looking up ObservedReleaseAvg / ReleaseConfig from its caches; the
// planner is pure given these inputs.
type StrategyInput struct {
	// Goal is the user's target; only Date and TimePrefs participate in
	// strategy selection (party-size and table filters affect slot
	// expansion in M1-08, not the strategy decision).
	Goal domain.Goal
	// Venue is the resolved venue; Venue.TZ is required and load-bearing
	// for the past-date check and (in M1-07) the drop-moment math.
	Venue domain.Venue
	// Now is the wall-clock instant the decision is taken at. Callers
	// pass clock.Clock.Now() per laws.md rule 7.
	Now time.Time
	// CalendarSnapshot is the result of providers.Provider.Calendar for
	// a date range covering Goal.Date. The zero value is treated as "no
	// data" — the planner falls through to Explicit/Discovered.
	CalendarSnapshot providers.Calendar
	// ObservedReleaseAvg, when non-nil, is the prior-observed release
	// instant for this venue (UTC). Its presence flips rule-2 from
	// "needs config" to "needs observation"; the design treats either
	// signal as sufficient for Explicit.
	ObservedReleaseAvg *time.Time
	// ReleaseConfig, when non-zero, is the operator-supplied prior on
	// the venue's release_days_ahead / release_time_local. See
	// VenueReleaseConfig.
	ReleaseConfig VenueReleaseConfig
	// ContinuousUntil, when non-zero, caps the engine's
	// cancellation-chase window. A zero value falls back to end-of-day
	// in the venue's timezone (the design's documented default).
	ContinuousUntil time.Time
}

// SelectStrategy implements the three-rule decision documented in
// design/planner.md §strategy-selection.
//
//  1. If CalendarSnapshot shows Goal.Date is already bookable, return
//     ContinuousRelease (engine starts polling immediately and races
//     cancellations).
//  2. If we have an observed-release-time or a venue release config,
//     return ExplicitRelease at a best-effort drop moment (M1-07 will
//     refine the math).
//  3. Otherwise return DiscoveredRelease — engine polls the calendar
//     starting some buffer before the projected drop window.
//
// SelectStrategy is pure: no I/O, no clock reads, no goroutines. ctx
// is accepted for signature symmetry with the rest of the planner
// surface (Plan will need it once the canonicalizer wires in) and so
// the Service layer can plumb deadlines cleanly; M1-06 does not honor
// cancellation because there is no blocking work to abort.
func SelectStrategy(ctx context.Context, in StrategyInput) (domain.ReleaseStrategy, []string, error) {
	_ = ctx // see godoc; M1-06 has no blocking work to cancel.

	if in.Venue.TZ == nil {
		return nil, nil, domain.ErrVenueMissingTZ
	}

	// Past-date check against the venue's own calendar — an operator
	// in America/Los_Angeles can legitimately plan a New-York venue
	// up to three hours past midnight UTC of the venue's previous day.
	targetMidnight := in.Goal.Date.In(in.Venue.TZ)
	venueToday := startOfDayIn(in.Now, in.Venue.TZ)
	if targetMidnight.Before(venueToday) {
		return nil, nil, fmt.Errorf("%w: target=%s venue-today=%s",
			ErrTargetInPast, in.Goal.Date, venueToday.Format("2006-01-02"))
	}

	notes := make([]string, 0, 2)

	// Rule 1 — Continuous.
	if calendarShowsBookable(in.CalendarSnapshot, in.Goal.Date) {
		until := in.ContinuousUntil
		if until.IsZero() {
			// Default to end-of-day in venue tz per
			// design/planner.md §edge-cases (today + no book_on_date).
			until = endOfDayIn(in.Now, in.Venue.TZ)
		}
		notes = append(notes, fmt.Sprintf(
			"strategy=Continuous because Resy is already serving slots for %s; we'll race cancellations until %s",
			in.Goal.Date, until.UTC().Format(time.RFC3339)))
		return domain.ContinuousRelease{Until: until.UTC()}, notes, nil
	}

	// Rule 2 — Explicit (either an observed prior or a configured one).
	switch {
	case in.ObservedReleaseAvg != nil:
		at := bestEffortExplicitDrop(in)
		notes = append(notes, fmt.Sprintf(
			"strategy=Explicit at %s (using observed release average; drop math finalized in M1-07)",
			at.UTC().Format(time.RFC3339)))
		return domain.ExplicitRelease{At: at.UTC()}, notes, nil
	case !in.ReleaseConfig.IsZero():
		at := bestEffortExplicitDrop(in)
		notes = append(notes, fmt.Sprintf(
			"strategy=Explicit at %s (using configured release_days_ahead=%d, release_time_local=%s; drop math finalized in M1-07)",
			at.UTC().Format(time.RFC3339),
			in.ReleaseConfig.DaysAhead, in.ReleaseConfig.TimeLocal))
		return domain.ExplicitRelease{At: at.UTC()}, notes, nil
	}

	// Rule 3 — Discovered. The engine polls the calendar; M1-07 will
	// tighten ProbeFrom/ProbeUntil once it knows the drop-moment math.
	probeFrom, probeUntil := bestEffortDiscoveredProbe(in)
	notes = append(notes, fmt.Sprintf(
		"strategy=Discovered because Resy hasn't observed a release for venue %s yet; probing %s..%s",
		in.Venue.AsRef(),
		probeFrom.UTC().Format(time.RFC3339),
		probeUntil.UTC().Format(time.RFC3339)))
	return domain.DiscoveredRelease{
		ProbeFrom:  probeFrom.UTC(),
		ProbeUntil: probeUntil.UTC(),
	}, notes, nil
}

// calendarShowsBookable reports whether snapshot has an explicit entry
// for date that is marked Available. A snapshot with no entry for the
// date is treated as "no data" — strategy selection falls through to
// rules 2/3.
func calendarShowsBookable(snap providers.Calendar, date domain.Date) bool {
	for _, d := range snap.Days {
		if d.Date == date {
			return d.Available
		}
	}
	return false
}

// startOfDayIn returns the midnight that began the current calendar day
// in loc, as observed at now. Used for the past-date check.
func startOfDayIn(now time.Time, loc *time.Location) time.Time {
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

// endOfDayIn returns 23:59:59 in loc for the calendar day now falls in.
// Used as the default ContinuousRelease.Until per the design's
// edge-case table.
func endOfDayIn(now time.Time, loc *time.Location) time.Time {
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 23, 59, 59, 0, loc)
}

// bestEffortExplicitDrop returns a placeholder drop moment for an
// Explicit strategy. M1-06 ships only the strategy choice; M1-07 will
// replace this stub with the ComputeDrop algorithm documented in
// design/planner.md §drop-moment-math.
//
// Current behavior:
//   - If ObservedReleaseAvg is non-nil, use it verbatim.
//   - Else, if ReleaseConfig is set, anchor at (target − DaysAhead) at
//     TimeLocal in the venue's tz — the same shape ComputeDrop will
//     return, just without the input validation the real function will
//     grow.
//   - Else (defensive) return midnight venue-local on the day-before
//     the target. SelectStrategy never reaches this branch because the
//     caller-side conditions above gate it.
func bestEffortExplicitDrop(in StrategyInput) time.Time {
	if in.ObservedReleaseAvg != nil {
		return *in.ObservedReleaseAvg
	}
	if !in.ReleaseConfig.IsZero() {
		anchor := in.Goal.Date.In(in.Venue.TZ).AddDate(0, 0, -in.ReleaseConfig.DaysAhead)
		return time.Date(
			anchor.Year(), anchor.Month(), anchor.Day(),
			in.ReleaseConfig.TimeLocal.Hour,
			in.ReleaseConfig.TimeLocal.Minute,
			in.ReleaseConfig.TimeLocal.Second,
			0,
			in.Venue.TZ,
		)
	}
	dayBefore := in.Goal.Date.In(in.Venue.TZ).AddDate(0, 0, -1)
	return dayBefore
}

// bestEffortDiscoveredProbe brackets a probe window around our best
// guess of when the venue will release. With no observed-release-time
// and no config, the planner falls back to midnight venue-local on the
// day before the target — a conservative prior. M1-07 will tighten
// this using /4/find's book_on_date once that field is plumbed through
// the providers seam.
//
// Defaults probeLead=90s, probeWidth=5m per design/planner.md
// §strategy-selection rule 3.
func bestEffortDiscoveredProbe(in StrategyInput) (probeFrom, probeUntil time.Time) {
	const (
		probeLead  = 90 * time.Second
		probeWidth = 5 * time.Minute
	)
	// Anchor: midnight venue-local on the day before the target. This
	// is the most common Resy default and biases the probe toward the
	// 30-day-out 00:00-venue-local case. M1-07 supersedes this with
	// book_on_date.
	anchor := in.Goal.Date.In(in.Venue.TZ).AddDate(0, 0, -1)
	dropGuess := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, in.Venue.TZ)
	if dropGuess.Before(in.Now) {
		dropGuess = in.Now
	}
	probeFrom = dropGuess.Add(-probeLead)
	probeUntil = probeFrom.Add(probeWidth)
	return probeFrom, probeUntil
}
