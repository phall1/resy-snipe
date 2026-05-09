# State machine

Every snipe is one row in `snipes` walking a finite graph of `Status`
values. The transition table lives in
[`internal/domain/status.go`](../internal/domain/status.go), is
enforced by `domain.CanTransition`, and is checked total at package
init — adding a new status without an entry panics at program start.

## Graph

```
                          ┌───────────────┐
                          │   Submitted   │
                          └───┬───────────┘
                              │
              ┌───────────────┼───────────────┐
              │               │               │
              ▼               ▼               ▼
        ┌─────────┐    ┌──────────────┐ ┌──────────┐
        │Scheduled│    │ Discovering  │ │ Canceled │
        └────┬────┘    └──────┬───────┘ └──────────┘
             │                │
             │   ┌────────────┘
             │   │
             ▼   ▼
          ┌──────────┐
          │ Awaiting │
          └─────┬────┘
                │
                ▼
          ┌──────────┐
          │ Finding  │ ◀──── (re-enter on re-find)
          └─────┬────┘
                │
                ▼
          ┌──────────┐
          │ Booking  │ ◀──── (re-enter on next slot)
          └─────┬────┘
                │
                ▼
          ┌──────────┐    Terminal: Booked, Failed, Canceled, Expired.
          │  Booked  │    No outgoing transitions.
          └──────────┘
```

Failure transitions to `Failed`, expiration to `Expired`, and cancel
to `Canceled` are legal from every non-terminal state (with the
exception that `Booking` cannot expire — once we have a `book_token`
we either confirm it or fail with a reason).

The full edge list is in
[`status.go:allowedTransitions`](../internal/domain/status.go).

## Atomicity

`SnipeState.Transition` (in
[`internal/engine/state.go`](../internal/engine/state.go)) is the
only path that mutates `Status`. Its sequence is:

1. **Validate** — reject if `domain.CanTransition(from, to)` is false.
2. **Clone** — build a candidate `domain.SnipeState` with the new
   status and updated timestamps.
3. **Persist** — `Store.TransitionSnipe(ctx, candidate, ev)` writes
   the row update and the matching `Event` row in **one SQL
   transaction**. Failure leaves the live state unchanged.
4. **Swap** — only after persist commits, the engine swaps
   `s.inner = candidate`.
5. **Emit** — fire `engine.emit(Notification{...})` so subscribers
   (the CLI's notifierBridge today) see only committed state.

This is what makes the engine resumable: any process exit happens
either before step 3 (no change) or after step 3 (change is durable).

## Events

Each transition emits a typed `domain.Event` written to the
`events` table for audit / replay. The event types are
([`event.go`](../internal/domain/event.go)):

| Event             | Emitted by                                    |
|-------------------|-----------------------------------------------|
| `submitted`       | `engine.Submit`                               |
| `scheduled`       | `engine.Run` → `transitionToScheduled`        |
| `discovered`      | `runDiscoveredRelease` (entering Discovering) |
| `released`        | release strategy — target date became available |
| `found`           | `RunBookingRace` enters Finding               |
| `book_attempted`  | `RunBookingRace` enters Booking               |
| `booked`          | First successful Confirm wins the race        |
| `failed`          | Any path that gives up                        |
| `canceled`        | User cancellation (not yet wired via CLI)     |
| `expired`         | Polling window elapsed without a hit          |

`event_type` and `status` are intentionally distinct surfaces — a
single status (e.g. `Failed`) can be reached for several reasons, and
the event carries the reason in `Attrs`.

## Subscriber contract

`engine.Subscribe(fn)` ([`subscribe.go`](../internal/engine/subscribe.go))
delivers a `Notification{SnipeID, From *Status, To Status, Event}`
to every registered subscriber after persist commits, in registration
order. `From == nil` only on the bootstrap `Submitted` emission (no
prior status). Subscribers must not block — see
[laws.md](laws.md#notifier-must-not-block).
