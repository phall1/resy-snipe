# Release strategies

Resy doesn't expose a single answer to "when will inventory open." A
high-demand venue might:

- Drop new tables at a known wall-clock time (`midnight ET`, `9am ET`).
- Drop new tables at an *empirically* known time-of-day, with the
  date offset varying (`30 days out, ~midnight, ±5 minutes`).
- Continuously trickle inventory throughout the day as cancellations
  release seats.

resy-snipe handles all three with the `domain.ReleaseStrategy` sealed
union ([`release.go`](../internal/domain/release.go)). Each variant
maps to an engine code path that does the right kind of waiting.

## ExplicitRelease

```go
type ExplicitRelease struct {
    At time.Time
}
```

Wake up at `At`, transition to `Awaiting`, hand off to the booking
race. The engine implements this as `Clock.AfterFunc(delay, fire)` —
no polling, no jitter. If `At` is in the past, the wake fires
synchronously.

**Use when** you know the venue's drop time. Default when the user
passes `-snipe-time`.

**Trade-off**: zero anti-bot footprint (one Find call at fire time)
but useless if the actual drop is even seconds off.

Code: [`run.go:fireRelease`](../internal/engine/run.go) for Explicit;
the booking race is in [`race.go`](../internal/engine/race.go).

## DiscoveredRelease

```go
type DiscoveredRelease struct {
    ProbeFrom  time.Time   // start polling
    ProbeUntil time.Time   // give up
}
```

Poll `Provider.Calendar(VenueRef, DateRange{Start: target, End:
target})` every `PollFloor` (default 100 ms) between `ProbeFrom` and
`ProbeUntil`. The first response that lists the target date as
available is the release moment — the engine:

1. Captures the wall-clock `now` as `observed_at`.
2. Writes an `observed_release_times` row keyed by venue, with
   `(local_time_of_day, days_offset)` so the *next* snipe for this
   venue can default to `ExplicitRelease` at the observed local time.
3. Transitions to `Awaiting`.

**Use when** you don't know the drop time but have a polling window
to bracket it. Default when no `-snipe-time` is provided.

**Trade-off**: anti-bot floor matters (`PollFloor` is clamped to
≥ 100 ms — see [invariants.md](invariants.md#pollfloor-is-floor)).
Calendar is a coarse endpoint and Resy's rate limiter is forgiving
there compared to Find/Book.

Code: [`release.go:runDiscoveredRelease`](../internal/engine/release.go).
The observed-release feedback loop is the long-term trick — after one
Discovered run for a venue, every subsequent snipe defaults to
Explicit at the right local time.

## ContinuousRelease

```go
type ContinuousRelease struct {
    Until time.Time
}
```

Poll `Provider.Find(FindRequest)` at `PollFloor` until either:

- A non-empty slot list comes back → transition to `Awaiting`, hand
  off to the booking race.
- `Until` elapses → transition to `Expired` with reason
  `continuous_window_expired`.

**Use when** the venue trickles inventory throughout the day (no
single drop) and you want to grab the first cancellation that lands
on a matching slot.

**Trade-off**: highest anti-bot exposure of the three strategies.
Find is the most anti-bot-watched endpoint; the engine clamps
`PollFloor` to ≥ 100 ms and won't let a per-snipe override go below.

Code: [`release.go:runContinuousRelease`](../internal/engine/release.go).

## How the engine chooses

`engine.Run` ([`run.go`](../internal/engine/run.go)) type-switches on
`Intent.Release`:

```
ExplicitRelease    → wait until r.At, then Awaiting (no polling)
DiscoveredRelease  → enter Discovering, poll Calendar, then Awaiting
ContinuousRelease  → poll Find immediately, then Awaiting
```

On `Awaiting`, control returns to the CLI which calls
`engine.RunBookingRace` to drive `Awaiting → Finding → Booking →
Booked / Failed`. The booking race is the same regardless of which
strategy got us there.

## Default selection in `cmd/`

`cmd/resy-snipe/intent.go:applyDefaults` picks a strategy when none
was supplied:

- `-snipe-time` provided → `ExplicitRelease` at that wall-clock.
- otherwise → `DiscoveredRelease` over a `defaultRetryWindow` (30 m)
  centered on now.

The user can always override with `-release-strategy`.
