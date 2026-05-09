# Anti-bot

Resy fronts its API with PerimeterX (now HUMAN Security) and a
collection of header-signing schemes. This doc captures what we
detect, what we tolerate, and the thing we don't yet handle.

## What we detect

Every Resy HTTP response goes through
[`internal/resy/errors.go:classifyHTTP`](../internal/resy/errors.go),
which is the single classification site. The relevant outputs:

| Signal | Maps to |
|---|---|
| HTTP 401 (any non-Login call) | `providers.ErrAuthExpired` |
| HTTP 403 + body marker (`captcha`, `challenge`, `denied`, `blocked`, `perimeterx`, `px-captcha`) | `providers.ErrAntiBotChallenge` |
| HTTP 403 (no marker) | `providers.ErrAuthExpired` |
| HTTP 429 | `providers.ErrRateLimited` |
| Any 4xx with `book_token expired` body | `providers.ErrBookTokenExpired` |
| Any 4xx with `slot taken` semantics | `providers.ErrSlotTaken` |

Body markers: lowercased substring match, evaluated in order
([`errors.go:108`](../internal/resy/errors.go) for the list).

## What we tolerate

The engine keeps the request rate below the empirical floor:

- `engine.pollInterval()` clamps `BookingPolicy.PollFloor` to
  ≥ 100 ms — see [I-6](invariants.md#pollfloor-is-floor).
- `RunBookingRace` enforces the same floor *between* `PrepareSlot`
  calls so `/3/details` fetches don't tail-gate.
- The default `User-Agent` matches a recent desktop Chrome string;
  Resy actively rejects unrealistic UAs.
- `Origin`, `Referer`, and `X-Origin` are pinned to
  `widgets.resy.com` to mirror the real frontend.
- `x-resy-auth-token` is sent in lowercase verbatim (Go's `Header.Set`
  canonicalisation would mangle it; the adapter bypasses canonical-
  isation explicitly — see
  [`client.go:applyHeaders`](../internal/resy/client.go)).

This is enough for the calendar / find / details flow on accounts
with normal usage history. It is *not* enough at peak Friday-evening
load on a high-demand venue.

## How the Signer recovers

R7 added the recovery half of the anti-bot story. The detection half
is unchanged (`classifyHTTP` still maps a 403 + marker body to
`providers.ErrAntiBotChallenge`); the new piece is a `Signer` seam
the adapter consults on every `/3/details` and `/3/book` call, plus a
sign-and-retry wrapper that turns a single anti-bot response into a
transparent transient.

### The seam

`internal/resy/sign` defines:

```go
type Signer interface {
    Sign(ctx context.Context, path string) (Headers, error)
    Reset(ctx context.Context) error
}
```

Plus two impls:

- **`sign.Noop`** — returns no headers, never errors. This is the
  default when no `WithSigner` option is passed and is what every
  existing test sees, so a codebase that has never opted in keeps
  working unchanged.
- **`sign.Subprocess`** — shells out to a configured binary (path
  via `RESY_SNIPE_SIGNER_BIN`) on every `Sign` call and on `Reset`.
  Output format is JSON on stdout: `{"headers": {"x-px-foo": "..."}}`.
  Sign-output is cached in-process; `Reset` discards the cache and
  re-shells via the binary's `reset --provider resy` subcommand.

### The retry envelope

`internal/resy/sign_retry.go:doSignedAndRetry` is the single path
`/3/details` and `/3/book` share:

1. Ask the Signer for headers; merge with caller-supplied `extra`
   (caller wins on key collision so `X-Resy-Idempotency-Key` is never
   overwritten).
2. Call `do()`. If the response classifies as
   `ErrAntiBotChallenge`, call `Signer.Reset(ctx)`, wait
   `signRetryFloor` (100ms, clock-driven), and retry once.
3. The second response is returned unchanged — even another
   `ErrAntiBotChallenge`. The engine sees the second failure as
   terminal, matching the pre-R7 contract.

The retry pacing uses `clock.Clock.After`, never `time.Sleep`, so
tests under `clock.NewFake` stay deterministic. The wait is
ctx-cancellable: a SIGTERM landing inside the wait drops the retry
and surfaces the original anti-bot response.

### Wiring

`cmd/resy-snipe/login.go:openSnipeBackend` constructs the production
client. When `RESY_SNIPE_SIGNER_BIN` is set, it builds a
`sign.Subprocess` pointing at that binary; otherwise it leaves the
default `sign.Noop` in place. Constructor failures are logged and
fall back to Noop — a misconfigured signer never breaks the snipe
path's bootstrap.

### Decision: subprocess wrapper, not Go port

Two factors drove this:

1. **The named upstream tool is something different**.
   [`mvanhorn/cli-printing-press`](https://github.com/mvanhorn/cli-printing-press)
   is a CLI generator (it prints token-efficient Go CLIs from
   APIs), not a Resy / PerimeterX signing toolkit. There is no
   logic to port — the integration target the original spec
   imagined doesn't exist at that URL.
2. **The seam is the deliverable**. With a generic subprocess
   wrapper plus a documented JSON wire format, the user can drop in
   any binary that produces signing headers (a Node.js script that
   solves the px challenge, a Python tool that mints `x-resy-rotated`,
   even a hand-rolled Go binary) without us picking a winner. The
   seam outlasts any specific upstream.

So the production default is `sign.Noop` (no behavior change from
pre-R7), and the `sign.Subprocess` wrapper is the production-ready
hook waiting for a binary to point at. When a real upstream lands,
the env var is the only knob.

### Runtime requirements

- **No mandatory dependency.** Without `RESY_SNIPE_SIGNER_BIN`, the
  binary works exactly as it did pre-R7.
- **With the env var:** the user provides the executable. Whatever
  that binary expects (Node, Python, a static binary) is the user's
  problem; we shell out to it via `exec.CommandContext` and never
  touch its internals. The 10s per-call timeout caps a hung signer.

### Future work

- TTL-based cache invalidation in `sign.Subprocess` (today the cache
  only invalidates on `Reset`; if upstream tokens have a known TTL
  we could invalidate proactively).
- Per-endpoint header shaping. `Sign(ctx, path)` already takes the
  path so a Signer can vary headers, but no current Signer uses it.
- A real upstream worth shipping. When the right tool surfaces, it's
  one wiring change in `openSnipeBackend` to make it the default.
