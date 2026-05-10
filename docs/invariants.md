# Invariants

Properties that the system guarantees and that callers, refactorers,
and reviewers can rely on. Each one names the file that enforces it.
If a refactor breaks an invariant, either the invariant is wrong
(rare, but possible — call it out) or the refactor is wrong.

## Persistence is the source of truth

**I-1**: An in-memory `engine.SnipeState` only changes after the
matching row update has committed.

Enforced by [`engine/state.go:Transition`](../internal/engine/state.go) —
the candidate clone is built first, persist runs second, the in-memory
swap (`s.inner = candidate`) is third. A persist failure leaves the
in-memory state untouched.

**Why it matters**: process death after step 3 is recoverable
(reload reads the new state); death between step 1 and step 3 is also
recoverable (reload reads the *old* state). There is no half-state.

## Status transitions are validated

**I-2**: Every status change goes through `domain.CanTransition` and
the table in `status.go:allowedTransitions`.

Enforced by `engine/state.go:Transition` and `run.go:transitionToScheduled`.
The transition table is checked total at package init — adding a status
without an entry panics at program start.

**Why it matters**: bug-class elimination. A typo'd transition
(`StatusBooked → StatusFinding`) fails fast at the call site, not
silently mid-snipe.

## Idempotency keys are deterministic

**I-3**: `IdempotencyKey(intentHash, slotPayload, attempt)` is a pure
function.

Enforced by [`domain/idempotency.go`](../internal/domain/idempotency.go).
Same inputs ⟹ same key forever. The Resy adapter sends this key with
every `/3/book` so a connection-reset retry produces a duplicate-key
match server-side rather than a double booking.

**Why it matters**: retries are safe. A bug-loop that re-fires the
same `attempt` is a no-op; a legitimate next attempt increments
`attempt` and gets a new key.

## In-flight Book is detached from cancel

**I-4**: A `/3/book` call already in flight when SIGTERM arrives is
NOT canceled by the parent context.

Enforced by [`engine/race.go:RunBookingRace`](../internal/engine/race.go) —
`raceCtx` is built with `context.WithoutCancel(ctx)` so the parent's
`Done` channel does not propagate. `PrepareSlot` (which fetches a new
`book_token` we'd waste) **does** see the cancel.

**Why it matters**: bailing mid-`POST` to `/3/book` risks a
confirmed-but-orphaned reservation. The user gets charged but the
client doesn't see the confirmation. We accept up-to-5s of extra
runtime in exchange for never producing that state.

## Time outside `internal/clock` is forbidden

**I-5**: No source file outside `internal/clock` may call
`time.Now()`.

Enforced by [`justfile:gates`](../justfile) which greps the source
tree. CI runs the same check.

**Why it matters**: every test wants deterministic time. A stray
`time.Now()` makes a test flaky in a way that's invisible until the
clock-of-day matters.

## PollFloor is a floor

**I-6**: `engine.pollInterval()` returns `max(policy.PollFloor,
100ms)`.

Enforced by [`engine/release.go:pollInterval`](../internal/engine/release.go).
A misconfigured snipe (or a Phase 2 daemon snipe with a corrupted
config row) cannot push the engine below the empirical anti-bot
floor.

**Why it matters**: defense in depth. The CLI defaults to 100ms;
this clamp ensures even a bad config doesn't burn the account.

## Errors are sentinels

**I-7**: Provider methods return errors that either equal one of the
sentinels in [`providers/provider.go`](../internal/providers/provider.go)
or wrap one with `%w`. Engine code branches with `errors.Is`, never
`strings.Contains`.

Enforced by reviewer + the Resy adapter's
[`errors.go:classifyHTTP`](../internal/resy/errors.go) which is the
single classification site for HTTP responses.

**Why it matters**: a body change on Resy's side cannot break engine
branching. Adding a new failure mode is a one-line edit to the
sentinel list and the classifier.

## Sealed unions stay sealed

**I-8**: `domain.ReleaseStrategy` and `domain.SlotPayload` are sealed
unions — every variant lands in the engine's type-switch and the
store's payload codec. A new variant must update both in lockstep, or
the engine panics with `unknown ReleaseStrategy %T` / the codec
returns a "slot=unknown(%T)" string.

Enforced by [`engine/release.go:runStrategy`](../internal/engine/release.go),
[`engine/run.go:scheduledAtFromIntent`](../internal/engine/run.go), and
[`store/payload_codec.go`](../internal/store/payload_codec.go).

**Why it matters**: sealed-union enforcement in Go is structural —
the compiler doesn't help. The runtime panics + the canonical-hash
"unknown" sentinel are the next-best line of defense.

## No `json.RawMessage` in memory

**I-9**: No source file outside `internal/store` references
`json.RawMessage`. Slot payloads are typed (`domain.SlotPayload`)
everywhere except at the persistence boundary.

Enforced by [`justfile:gates`](../justfile) which greps the source
tree.

**Why it matters**: a `RawMessage` in the engine path means the
engine had to know the encoding of a payload it doesn't otherwise
care about — that's the SQL boundary leaking up.

## Notifier must not block

**I-10**: `notify.Notifier` implementations must not perform blocking
I/O on `Transition` / `Result`.

Documented in [`notify/notifier.go`](../internal/notify/notifier.go).
The engine emits notifications synchronously on `engine.emit`'s call
path; a blocking notifier stalls every snipe in flight. Buffered
transports (network, disk) wrap the call site, not the engine.

The stdout impl is mutex-serialized stdout writes — fast enough that
in-line delivery is safe.

## Every HTTP call has a deadline

**I-11**: every Resy HTTP request executes under a context with a
deadline. If the caller's context already carries one, that deadline
is honored; otherwise `resy.Client.do` derives a 30s ceiling
(`defaultPerCallTimeout`) before issuing the request.

Enforced by
[`resy/client.go:doWithExtraHeaders`](../internal/resy/client.go) plus
the hard `http.Client.Timeout: 30 * time.Second` on the underlying
client (`client.go:NewClient`).

**Why it matters**: a misbehaving Resy server cannot stall the engine
indefinitely. The earlier "fail closed if the caller forgot a
deadline" form of this invariant turned out to be brittle — engine
code legitimately wants to pass a snipe-lifetime context, and the
CLI's login subcommand has an interactive prompt loop where forcing
a deadline races the typist. Deriving the per-call ceiling at the
adapter boundary preserves the safety property without forcing every
caller to know about it.
