# signers/

Drop-in `RESY_SNIPE_SIGNER_BIN` scripts for the anti-bot Signer seam.
See [docs/anti-bot.md](../docs/anti-bot.md) for the contract and
[docs/signers.md](../docs/signers.md) for the deeper "what is a signer"
explanation.

## obscura-resy.sh — recommended

Wraps [Obscura](https://github.com/h4ckf0r0day/obscura), an open-source
Rust headless browser with built-in stealth mode. Obscura runs Resy's
PerimeterX challenge JavaScript in V8, lets it set cookies, and the
script harvests those cookies as a `Cookie:` header for resy-snipe to
attach.

### Install

1. Grab the latest Obscura release for your platform:

   ```bash
   # macOS Apple Silicon
   curl -LO https://github.com/h4ckf0r0day/obscura/releases/latest/download/obscura-aarch64-macos.tar.gz
   tar xzf obscura-aarch64-macos.tar.gz
   sudo mv obscura /usr/local/bin/

   # macOS Intel
   curl -LO https://github.com/h4ckf0r0day/obscura/releases/latest/download/obscura-x86_64-macos.tar.gz
   tar xzf obscura-x86_64-macos.tar.gz
   sudo mv obscura /usr/local/bin/

   # Linux x86_64
   curl -LO https://github.com/h4ckf0r0day/obscura/releases/latest/download/obscura-x86_64-linux.tar.gz
   tar xzf obscura-x86_64-linux.tar.gz
   sudo mv obscura /usr/local/bin/
   ```

   Confirm: `obscura --version`.

2. Test the script standalone (proves Obscura can fetch + extract
   cookies):

   ```bash
   ./signers/obscura-resy.sh sign
   ```

   Expected output (cookie names will vary, the keys you want are
   `_px3` / `_pxhd` / `_pxvid`):

   ```json
   {"headers": {"Cookie": "_px3=...; _pxhd=...; _pxvid=..."}}
   ```

3. Wire into resy-snipe:

   ```bash
   export RESY_SNIPE_SIGNER_BIN=$PWD/signers/obscura-resy.sh
   bin/resy-snipe -user you@example.com -snipe-time 00:00 -venue-id 38660 -res-times 19:00
   ```

   The boot log should print
   `INFO anti-bot signer: subprocess bin=…/signers/obscura-resy.sh`.

### Tuning

Environment variables the script honors:

| Var | Default | Purpose |
|---|---|---|
| `OBSCURA_BIN` | `obscura` (PATH lookup) | Absolute path to the obscura binary if not on PATH |
| `OBSCURA_TARGET_URL` | `https://widgets.resy.com/` | Page to visit. Some PX deployments are stricter on the public homepage; tune if your account's snipes start failing |
| `OBSCURA_CACHE_PATH` | `/tmp/.obscura-resy-cookies` | Cross-invocation cookie cache; `reset` deletes it |
| `OBSCURA_TIMEOUT` | `20` | Obscura fetch timeout (seconds) |

### How it works

```
  resy-snipe                       obscura-resy.sh                 obscura
  ──────────                       ───────────────                 ───────
  PrepareSlot/Find/Book
        │
        │ Sign(ctx, "/3/details") ▶ check cache file ─── hit ──▶ emit cached JSON ─┐
        │                                                                          │
        │                          miss                                            │
        │                            ▼                                             │
        │                          obscura fetch widgets.resy.com ─────────────▶ runs PX JS
        │                                                                          │
        │                          PX cookies set in document.cookie               │
        │                          obscura returns cookie string ◀──────────────── │
        │                            │                                             │
        │                          write cache, emit JSON ◀──────────────────────  │
        │ {"headers": {"Cookie": "..."}}                                            │
        │                                                                          │
        ▼                                                                          │
  merge into outbound /3/details, /3/book, /4/find, /4/venue/calendar             │
        │                                                                          │
  on 403 anti-bot:                                                                  │
        │ Reset(ctx)        ────▶ rm cache file                                    │
        │ retry once                                                                │
```

### Caveats

- **Not a guarantee.** PerimeterX rotates its detection. Obscura's
  stealth team also rotates their countermeasures. Sometimes you'll
  win, sometimes you won't. If a high-demand snipe gets blocked
  repeatedly, try off-peak first; if that fails too, fork the script
  and iterate (run obscura interactively against the target URL, see
  what comes back, adjust).
- **First call is slow** (~1-2 s for fork+exec+page-load). Subsequent
  calls within the same `resy-snipe` run reuse the cached cookies via
  the in-process Signer cache — no extra round-trip.
- **User-Agent alignment.** Obscura's stealth mode presents a recent
  Chrome UA. The resy adapter's default UA in
  `internal/resy/client.go:DefaultUserAgent` is also Chrome desktop.
  These should be close enough that PX accepts the cookies. If you
  customise either, customise both to match.
- **Cookies != all the headers PX wants.** Some PX deployments also
  inject `x-px-original-token` or rotated `x-resy-auth-token` values
  into outbound XHR via JS. Today this script only harvests
  `document.cookie`. If you find that's not enough, the next step is
  using Obscura's CDP server (`obscura serve --stealth`) and a longer
  Node/Puppeteer-based extractor that captures `setRequestHeader` calls
  via `page.setRequestInterception`. That's a separate signer worth
  filing as a follow-up.

## Adding your own signer

The contract is small (see
[`internal/resy/sign/printing_press.go`](../internal/resy/sign/printing_press.go)):

```
$ your-signer sign --provider resy /3/details
{"headers": {"x-anything": "..."}}

$ your-signer reset --provider resy
# exit 0 with empty stdout
```

Drop the script in this directory, make it executable, point
`RESY_SNIPE_SIGNER_BIN` at it. The Subprocess Signer in
`internal/resy/sign/printing_press.go` does the rest.

Common alternatives if Obscura doesn't work for your case:

- **A SaaS solver** (CapSolver, AntiCaptcha, 2Captcha) — your script
  POSTs to their API and reformats the response. Easiest to build,
  costs per-call, opaque to PX but adds an external trust dependency.
- **Custom Puppeteer/Playwright** harness — more flexible than the
  bare `obscura fetch` path, can intercept XHR and extract dynamically
  injected headers. Heavier to maintain.
- **Static cookie injection** — paste cookies you grabbed from a
  signed-in browser DevTools session. Brittle but zero-dependency for
  one-off snipes.
