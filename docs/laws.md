# Laws

Conventions every contributor follows. The shorter the rule, the more
load-bearing it is.

## Layering

1. **Engine never imports a concrete provider.** It depends on
   `internal/providers`. New provider ⟹ new sibling package, never an
   `if provider == "resy"` branch.
2. **Domain depends on nothing inside the project.** No imports of
   store, providers, engine, or clock from `internal/domain/*.go`.
3. **Store imports only domain.** SQLite-specific code stays in
   `internal/store`. Engine code that needs SQL is wrong.
4. **`cmd/` is wiring only.** Business logic in `cmd/resy-snipe/*.go`
   is a smell — push it down into a package that can be tested without
   building a CLI.

## Interfaces

5. **Define interfaces at the consumer.** `internal/resy.SessionStore`
   is the slim interface the resy adapter needs; `cmd/` adapts the
   real `*store.SQLiteStore` to it. The resy adapter does not import
   `internal/store`.
6. **Compile-time `var _ Iface = (*Impl)(nil)` checks** for any type
   that satisfies an interface across a package boundary. See
   [`provider_adapter.go`](../cmd/resy-snipe/provider_adapter.go) for
   the canonical example.

## Time

7. **Inject `clock.Clock`. Never call `time.Now()`** outside
   `internal/clock`. Enforced by `just gates`. See
   [I-5](invariants.md#time-outside-internalclock-is-forbidden).
8. **Tests use `clock.NewFake`** to drive scheduling deterministically.
   Real-clock tests are an explicit exception (see `engine/race_test.go`)
   and should be commented as such.

## Errors

9. **Sentinel errors at package boundaries.** Adapters classify into
   the `providers.Err…` family; engine code branches with
   `errors.Is`. No `strings.Contains` on a body. See
   [I-7](invariants.md#errors-are-sentinels).
10. **Wrap with `%w`**, not `%s`, when an error crosses a function
    boundary. Strip context with care; never strip a sentinel.

## Concurrency

11. **No background goroutines outlive their function.** `RunBookingRace`
    waits on its own goroutines via `sync.WaitGroup` before returning;
    `engine.Run` cancels its own timer in `waitAndFire`. If you need a
    goroutine, you also need the wait + the cancel path.
12. **Cancellation is a deliberate decision.** Use
    `context.WithoutCancel` when you specifically want to detach (in-
    flight Book — see [I-4](invariants.md#in-flight-book-is-detached-from-cancel)).
    Otherwise propagate.

## Persistence

13. **Status changes are atomic with their Event row.** `Store.Transition-
    Snipe(ctx, candidate, ev)` is the only path; one transaction, both
    or neither.
14. **No `json.RawMessage` outside `internal/store`.** Payloads are
    typed `domain.SlotPayload` everywhere else. Decoding happens at
    the SQL boundary. See [I-9](invariants.md#no-jsonrawmessage-in-memory).

## Logging

15. **Use `*slog.Logger` from DI; never the global default.** The
    engine and CLI both build their logger at construction.
16. **Use the canonical keys** from `domain.LogKey*` (e.g.
    `domain.LogKeySnipeID`) so log queries can join across services
    without string drift.

## Tests

17. **`-race` is non-negotiable.** Every test runs under it. CI runs
    `go test -race ./...`.
18. **Pin `t.Parallel()`** in any test that does not depend on global
    state. Most tests can.
19. **Synchronization, not sleeps.** `runtime.Gosched()` and polling
    helpers (`waitUntil`, `goroutineCountStable`) instead of `time.Sleep`
    when ordering matters.
20. **Tests are single-rooted at the public API** of the package they
    test. Reaching into private internals from `_test.go` is allowed
    but flagged in review.

## Lint

21. **`golangci-lint` with `.golangci.yml` is the floor.** No
    `//nolint:lint-name` without a one-line reason on the same line.
    The lefthook pre-commit hook runs the full lint set.
22. **Modernize warnings get fixed, not silenced.** `atomic.Int64` over
    `int64 + atomic.AddInt64`, `range int` over `for i := 0; i < N`,
    etc. Go's idioms move; we move with them.

## Documentation

23. **`doc.go` per package** describing what the package does in 1-2
    paragraphs. `go doc resy-snipe/internal/engine` should explain the
    package without reading source.
24. **Comments explain WHY, not WHAT.** A good function name + clear
    types are the WHAT. The comment should answer "why is this code
    here that wouldn't be obvious to a careful reader." See the
    project-level `Don't add comments` rule.
25. **Cross-package decisions live in `docs/`.** Single-package
    architecture lives in the package's `doc.go`. Don't paste
    function bodies into either.
