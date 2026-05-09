# Signers

A "signer" is a binary that produces the headers Resy's PerimeterX
anti-bot pipeline expects on `/3/details`, `/3/book`, `/4/find`, and
`/4/venue/calendar` requests. resy-snipe shells out to it before each
of those calls.

This doc explains the *what* and *why*. For the *how* and a working
recipe, see [`signers/README.md`](../signers/README.md).

## Why this exists outside the Go binary

Resy fronts its API with PerimeterX. PerimeterX runs JavaScript in
your browser that fingerprints the machine, computes a token, and
sets cookies (`_px3`, `_pxhd`, `_pxvid`, …). Resy's API rejects calls
that don't include those cookies on hot endpoints.

A Go HTTP client doesn't run JavaScript — so it has nothing to send.
The signer's job is to obtain those cookies *somehow* and hand them
back to resy-snipe as headers.

There are three real strategies:

1. **Headless browser** — actually run PX's JS in a real V8/JS engine
   under a fake DOM. The cookies it sets are real. This is what the
   recommended Obscura signer does.
2. **JS-runtime + reverse-engineered VM** — extract PX's JS, run it
   under a small JS engine (QuickJS, V8 standalone) with a faked
   `window`/`navigator`. Faster than (1), more brittle.
3. **Pay a SaaS solver** — CapSolver, AntiCaptcha, etc. POST a target
   URL, get cookies. Easiest, costs money.

All three are heavy or external. The signer lives outside the Go
binary because:

- (1) drags in a multi-MB browser engine
- (2) breaks every time PX rotates their JS — you don't want to ship
  resy-snipe releases on PX's schedule
- (3) is a network call to a third party
- Different users will pick different options. The seam stays
  constant; the implementation is yours

## The seam

resy-snipe exposes one interface: `internal/resy/sign.Signer`. Its
production implementation, `Subprocess`, just shells out to whatever
binary `RESY_SNIPE_SIGNER_BIN` points at and parses its stdout as
JSON.

Wire format:

```
$ $RESY_SNIPE_SIGNER_BIN sign --provider resy /3/details
{"headers": {"Cookie": "_px3=...; _pxhd=..."}}    ← exit 0

$ $RESY_SNIPE_SIGNER_BIN reset --provider resy
                                                  ← exit 0, empty stdout
```

That's the whole contract. Any binary that obeys it plugs in.

## When the adapter calls Sign and Reset

For each anti-bot-watched HTTP call (`PrepareSlot`, `Book`, `Find`,
`Calendar`), the adapter:

1. Calls `Sign(ctx, path)` and merges the returned headers into the
   outbound request.
2. Sends the request. If the response is `ErrAntiBotChallenge` (HTTP
   403 + an anti-bot marker body), the adapter:
   - Calls `Reset(ctx)` so the signer discards any cached cookie set.
   - Waits the engine's PollFloor (≥100 ms) — this is invariant
     [I-6](invariants.md#pollfloor-is-floor).
   - Calls `Sign(ctx, path)` again and retries the request **once**.
3. If the second response is also `ErrAntiBotChallenge`, the adapter
   surfaces it unchanged. The engine sees a terminal failure. The
   contract is "best effort, no infinite loop."

The retry is skipped entirely when the active Signer is `sign.Noop`
(an explicit type assertion in `internal/resy/sign_retry.go`),
because retrying with no signer would just send a byte-for-byte
identical request and double rate-limit pressure.

## Implementations shipped

`internal/resy/sign`:

- **`Noop`** — returns no headers, never errors. The default. Suitable
  for off-peak snipes against accounts with normal browsing history.
- **`Subprocess`** — wraps a configured binary, parses its JSON output,
  caches in-process across calls, refetches on `Reset`. The production
  path.

`signers/`:

- **`obscura-resy.sh`** — recommended. Wraps the Obscura headless
  browser; ~70 MB binary, ~1-2 s first call, 30 MB resident. See
  [`signers/README.md`](../signers/README.md).

## Knowing which signer is active

The CLI logs the signer state on every snipe boot:

```
level=INFO msg="anti-bot signer: subprocess" bin=/path/to/signer
```

or:

```
level=INFO msg="anti-bot signer: noop (set RESY_SNIPE_SIGNER_BIN to enable per-call signing)"
```

If you set `RESY_SNIPE_SIGNER_BIN` and the binary fails to construct
(e.g. the file doesn't exist or isn't executable), you'll see a WARN
log and the adapter falls back to `Noop` — a bad signer config never
breaks bootstrap.

## When you actually need a signer

The honest answer: **most snipes don't need one.** PerimeterX is most
aggressive on:

- Highest-demand venues (Carbone, Don Angie, top steakhouse drops)
- Midnight ET ± a few minutes
- Accounts without recent browsing history
- IPs flagged as VPN / data-center / Tor

For mid-tier venues at off-peak times on accounts you actually use to
browse Resy in a browser, the default `Noop` signer is fine. Only
reach for Obscura when you start seeing `provider: anti-bot challenge`
errors that don't go away by waiting a few minutes.

## When you need to debug a signer

The signer is a regular subprocess, so:

```bash
# Reproduce what resy-snipe sees:
$RESY_SNIPE_SIGNER_BIN sign --provider resy /3/details

# Inspect what Obscura is actually returning:
obscura fetch https://widgets.resy.com/ --stealth --wait-until networkidle0 --eval "document.cookie"

# Watch the wire — see what headers resy-snipe is sending:
bin/resy-snipe -log-level debug -user you@example.com ...
```

If `document.cookie` from Obscura is empty, PX didn't run the
challenge — try `--stealth`, try a different `OBSCURA_TARGET_URL`,
check Obscura's release notes for stealth-mode fixes.
