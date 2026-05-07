package domain

import "time"

// ExplicitRelease fires at exactly At. Used when the venue's release
// time is known (either user-supplied or learned and cached from a
// prior DiscoveredRelease run).
type ExplicitRelease struct {
	At time.Time
}

func (ExplicitRelease) isReleaseStrategy() {}

// DiscoveredRelease polls the venue calendar between ProbeFrom and
// ProbeUntil and fires the moment the target date appears as
// available. The engine then writes the observed wall-clock time into
// the observed_release_times cache so the next snipe for this venue
// can use ExplicitRelease.
type DiscoveredRelease struct {
	ProbeFrom  time.Time
	ProbeUntil time.Time
}

func (DiscoveredRelease) isReleaseStrategy() {}

// ContinuousRelease keeps polling find/details until either a slot is
// booked or Until is reached. Used for low-stakes long-running
// snipes where there is no known release time.
type ContinuousRelease struct {
	Until time.Time
}

func (ContinuousRelease) isReleaseStrategy() {}
