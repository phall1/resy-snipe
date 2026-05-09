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

## What we don't yet handle

`ErrAntiBotChallenge` is currently terminal — the engine surfaces it
and the snipe fails. There is no logic to:

1. Fetch a fresh PerimeterX `_px*` cookie set.
2. Solve the JavaScript challenge / CAPTCHA puzzle.
3. Re-mint the `x-resy-auth-token` rotations the frontend issues
   alongside session tokens.

This is the Phase-2 gap the **R7 / printing-press integration**
addresses (tracked in [work-items.md](work-items.md)).

### printing-press

[`mvanhorn/cli-printing-press`](https://github.com/mvanhorn/cli-printing-press)
is a published reverse-engineered toolkit for the Resy signing
surface. The integration shape we want:

- **New package**: `internal/resy/sign` with a `Signer` interface
  (`Sign(ctx, req) (headers, error)`) and one production impl that
  shells out to printing-press as a subprocess.
- **Signer is the seam**, like `Provider`. Tests substitute a fake;
  the production wiring lives in `cmd/`.
- **Adapter consults Signer** before `/3/details` and `/3/book` to
  populate any per-call signed headers.
- **On `ErrAntiBotChallenge`**, the adapter calls `Signer.Reset(ctx)`
  to mint a fresh token set, then retries with bounded backoff. The
  engine sees the retry as a transparent transient.

This keeps the engine ignorant of signing — anti-bot is an adapter
concern, not a state-machine concern. The sentinel taxonomy already
has the slot for "we hit the wall"; what it lacks is the recovery
path.

### Open question

Subprocess vs. Go port of printing-press. Subprocess is faster to
ship but adds a runtime dependency. A native port would be cleaner
but is a non-trivial reverse-engineering job in its own right. The
R7 bead's first deliverable is the read-the-code-and-decide step,
not the integration itself.
