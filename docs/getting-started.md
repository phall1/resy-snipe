# Getting started

Five-minute walkthrough from `git clone` to a live snipe. Skip
straight to the **Quick test** section if you just want to confirm
the binary works.

## Prerequisites

- Go 1.25+
- `just` (`brew install just`) — runs the project's recipes
- A Resy account
- macOS or Linux (signer subprocess uses `/bin/sh`)

## 1. Build

```bash
just build              # → bin/resy-snipe
```

That's the whole build step. The binary is self-contained — no
runtime deps unless you opt into a Signer.

## 2. Log in (one-time)

```bash
bin/resy-snipe login
```

Prompts for email + password. The session is persisted in SQLite
under `$XDG_DATA_HOME/resy-snipe/db.sqlite` (default
`~/.local/share/resy-snipe/db.sqlite` on Linux,
`~/Library/Application Support/resy-snipe/db.sqlite` on macOS).

The JWT's `exp` claim is parsed and stored; expired sessions surface
as `run 'resy-snipe login' first` — they never silently re-login
mid-snipe.

If Resy demands MFA, you'll be prompted for the code. The MFA
handler is currently stubbed — it'll print the actionable error if
the API requires it. (Phase 2.)

## 3. Smoke-test (no network)

```bash
bin/resy-snipe -snipe-time 00:00 -venue-id 38660 -res-times 18:30
```

No `-user` ⟹ **dry-run preview**. The binary parses the flags,
builds an `Intent`, logs it, and exits. This is your "did I get the
flags right?" check — it never touches the network.

Expected output (single info line):

```
time=… level=INFO msg="intent assembled" venue_ref=resy:38660 date=2026-05-16 ...
time=… level=INFO msg="dry-run: pass -user <email> to enable the live snipe path"
```

## 4. Live snipe (touches Resy)

```bash
bin/resy-snipe \
  -user you@example.com \
  -venue-id 38660 \
  -res-times 18:30,19:00 \
  -snipe-time 00:00
```

This will:

1. Open the SQLite store, look up the persisted session for `-user`.
2. Build the engine + provider adapter.
3. Wait until `-snipe-time` (e.g. midnight tonight).
4. Call `/4/find`; if a slot matches, race PrepareSlot+Book.
5. Print live transitions to stdout.
6. Exit `0` on `Booked`, non-zero otherwise.

Press Ctrl-C to abort cleanly — in-flight `/3/book` calls finish
(the engine detaches them from cancel to avoid orphaned
reservations), everything else unwinds in <5s.

## 5. Anti-bot signing (optional, peak-load only)

For most snipes you don't need this. Only worry about it if Resy
returns `provider: anti-bot challenge` errors — typically only on
the most-watched midnight drops at the highest-demand venues.

If you hit it, the recommended path is the bundled
[`signers/obscura-resy.sh`](../signers/obscura-resy.sh) wrapper around
[Obscura](https://github.com/h4ckf0r0day/obscura), an open-source Rust
headless browser that runs PerimeterX's JavaScript and harvests the
cookies it sets:

```bash
# 1. Install Obscura (Apple Silicon shown; see signers/README.md for
#    Linux + Intel)
curl -LO https://github.com/h4ckf0r0day/obscura/releases/latest/download/obscura-aarch64-macos.tar.gz
tar xzf obscura-aarch64-macos.tar.gz
sudo mv obscura /usr/local/bin/

# 2. Verify the script works standalone
./signers/obscura-resy.sh sign

# 3. Wire into resy-snipe
export RESY_SNIPE_SIGNER_BIN=$PWD/signers/obscura-resy.sh
bin/resy-snipe -user … -snipe-time 00:00 …
```

The boot log will confirm:
`anti-bot signer: subprocess bin=…/obscura-resy.sh`.

For the deeper "what is a signer / why does this exist" explanation,
see [signers.md](signers.md). For tuning + adding your own signer,
see [`signers/README.md`](../signers/README.md).

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `no valid session — run 'resy-snipe login' first` | No session for that `-user`, or it's expired. | `bin/resy-snipe login` |
| `provider: anti-bot challenge` | PerimeterX flagged the call. | Wait, retry off-peak, or wire `RESY_SNIPE_SIGNER_BIN` (see §5). |
| `provider: rate limited` | Resy returned 429. | Lower `-retry-window`, increase `-snipe-time` granularity, or wait. |
| `provider: book token expired` | `/3/details` token aged out before `/3/book` fired. | Engine retries automatically; if persistent, the venue's anti-bot is hot — try off-peak. |
| `dry-run: pass -user` | You forgot `-user`. | Add `-user you@example.com`. |

## Common flag combos

```bash
# Discovered release: don't know the drop time, poll calendar for 30m
bin/resy-snipe -user you@x -venue-id 38660 -res-times 19:00 -retry-window 30m

# Continuous: chase cancellations all afternoon
bin/resy-snipe -user you@x -venue-id 38660 -res-times 19:00,19:30 \
  -release-strategy continuous -retry-window 4h

# Verbose logs
bin/resy-snipe -user you@x -venue-id 38660 -res-times 19:00 \
  -snipe-time 00:00 -log-level debug
```

## Built-in venue ids

The interactive mode (`-interactive`) shows a built-in shortlist; you
can also use any numeric venue id:

| Venue | id |
|---|---|
| Dead Rabbit | 38660 |
| Rubirosa | 466 |
| Red Pearl | 69820 |
| Rafs | 65679 |
| Carbone | 6194 |
| Don Angie | 1505 |
| San Sabino | 78799 |
| Gertrudes | 71935 |
| Au Cheval | 5769 |
| HOWOO | 86696 |

To find a venue id: navigate to the venue on resy.com, open dev
tools, and look at the `/4/find` query in the network tab — `venue_id=…`
is the parameter.

---

That's it. For deeper reading: [docs/README.md](README.md) is the
index into the rest of the architecture/invariants/laws docs.
