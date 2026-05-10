# Getting started

Five-minute walkthrough from `git clone` to a scheduled v2 quest. v2
inverted the contract: you state a Goal (URL + date + party + time
prefs), and the system resolves the venue, plans the drop, and
schedules the engine. You no longer compute snipe-time by hand. See
[docs/v2/README.md](v2/README.md) for the full re-architecture story;
[docs/v2/design/overview.md](v2/design/overview.md) is the map.

If you're new to v2 from v1, skim **§What changed from v1** at the
bottom first.

## Prerequisites

- Go 1.25+
- `just` (`brew install just`) — runs the project's recipes
- A Resy account
- macOS or Linux (signer subprocess uses `/bin/sh`)

Same as v1.

## 1. Build

```bash
just build              # → bin/resy-snipe
```

Self-contained binary; no runtime deps unless you opt into a Signer.

## 2. Log in (one-time)

```bash
bin/resy-snipe login
```

Prompts for email + password. The session is persisted in SQLite
under `$XDG_DATA_HOME/resy-snipe/db.sqlite` (default
`~/.local/share/resy-snipe/db.sqlite` on Linux,
`~/Library/Application Support/resy-snipe/db.sqlite` on macOS).

The JWT's `exp` claim is parsed and stored; expired sessions surface
as `run 'resy-snipe login' first` — never a silent re-login mid-snipe.

If Resy demands MFA, you'll be prompted for the code. The MFA handler
is stubbed; it'll surface an actionable error if hit. (Phase 2.)

Unchanged from v1.

## 3. Seed the operator user (one-time)

```bash
bin/resy-snipe user seed --email you@example.com
```

Creates the single homelab operator row in the new `users` table and
backfills any pre-existing Resy account rows so the v2 Service layer
has a `user_id` to scope every call by. Required before any `quest`
or `venue` subcommand — v2 is multi-user-from-day-one
([ADR-0005](v2/adr/0005-multi-user-data-model-from-day-one.md)) even
when the deployment has exactly one user. Single-user mode is just
one row in `users`.

v1 had no multi-user model; everything was implicitly the running
user. Seeding is the v2 equivalent.

To invite a second user later, see §8.

## 4. Resolve a venue

```bash
bin/resy-snipe venue resolve "https://resy.com/cities/washington-dc/venues/astoria-dc"
```

Expected output:

```
Resolved venue:
  Name:     Astoria DC
  Provider: resy
  Ref:      49716
  TZ:       America/New_York
```

`venue resolve` accepts three input shapes:

| Shape | Example |
|---|---|
| Resy URL | `https://resy.com/cities/washington-dc/venues/astoria-dc?date=…` |
| `slug:city` | `astoria-dc:washington-dc` (or `astoria-dc+washington-dc`) |
| Freeform name | `"Astoria DC"` (server-side search; ambiguous names print a picker) |

Pass `-format json` for the machine-readable shape.

The result is cached in SQLite (`venues_cache`) for 24h by default —
override with the `RESY_SNIPE_RESOLVER_TTL` env var (any duration
`time.ParseDuration` accepts). `-no-cache` bypasses the cache for one
call. See [docs/v2/design/resolver.md](v2/design/resolver.md) for the
URL parser, name-search fallback, and stale-on-failure semantics.

## 5. Plan a quest (preview, no commit)

```bash
bin/resy-snipe quest plan \
  "https://resy.com/cities/washington-dc/venues/astoria-dc" \
  -date 2026-06-09 \
  -party 2 \
  -time 18:30..21:00 \
  -priority earlier \
  -account you@example.com \
  -user usr_op01
```

Expected output (shortened):

```
PLAN
  venue:        Astoria DC (resy:49716)
  date:         2026-06-09 (Tue)
  party:        2
  time prefs:   18:30, 19:00, 19:30, 20:00, 20:30
  drop moment:  2026-05-12T00:00 EDT (2 days from now)
  strategy:     Explicit (release time observed for this venue)
  signing:      probably not required
  fire windows: 1
  hash:         a8f2…
  notes:        —
```

Pure dry-run — no quest is persisted, no engine is scheduled, no
notifier fires. The `hash` is a content-hash of the canonicalized
Plan (RFC 8785 JCS-style); it's the value you pin on `quest create`
to defend against TOCTOU between preview and commit
([ADR-0012](v2/adr/0012-plan-first-ux.md)).

URL query params (`?date=`, `?seats=`) pre-fill the corresponding
flags; explicit flags always win. `-priority` is `earlier|later|none`.
`-tables` accepts a comma-separated table-type filter (default: any).
`-format json` returns the full canonical Plan including the full
hash. See [docs/v2/design/planner.md](v2/design/planner.md) for the
strategy-selection rules and canonicalization.

## 6. Create a quest (commit)

```bash
bin/resy-snipe quest create \
  "https://resy.com/cities/washington-dc/venues/astoria-dc" \
  -date 2026-06-09 -party 2 -time 18:30..21:00 \
  -account you@example.com -user usr_op01
```

This:

1. Recomputes the Plan server-side.
2. Prints it for your inspection.
3. Prompts `create? [y/N]`.
4. On `y`, persists the Quest, schedules the engine wake-up, writes
   an audit row, and prints the assigned `qst_…` id.

Pass `-yes` to skip the prompt (scripts, CI). The plan hash is
verified between preview and commit — if the venue config drifted
between `quest plan` and `quest create` (e.g. release window changed),
the CLI surfaces `the plan changed — re-run` rather than committing
to a stale plan.

For idempotent retries from agents or shell loops, pass
`-idempotency-key=<24h-unique-string>`. Same key + same Goal = same
Quest returned; same key + different Goal = `idempotency conflict`.
The Service retains keys for 24h.

`quest create` is the only subcommand that writes a Quest. See
[ADR-0012](v2/adr/0012-plan-first-ux.md) for why plan-and-confirm is
split from commit.

## 7. List, inspect, cancel

```bash
bin/resy-snipe quest list
bin/resy-snipe quest get  qst_8xK3aZ
bin/resy-snipe quest cancel qst_8xK3aZ -reason="changed mind"
```

`quest list` filters: `-status` (comma-separated:
`submitted|scheduled|awaiting|finding|booking|booked|failed|canceled|expired`;
aliases `pending=submitted`, `firing={awaiting,finding,booking}`),
`-account`, `-since` / `-until` (RFC3339), `-limit`.

`quest get` prints the full quest state including the event log; pair
with `-format json` for machine consumption.

`quest cancel` prompts unless `-yes`. `-reason` is a free-form
annotation stored on the audit row.

All three subcommands accept `-user <usr_…>`; on a single-user install
the operator is the default and the flag can be omitted. The Service
enforces tenancy — user A never sees user B's quests
([ADR-0005](v2/adr/0005-multi-user-data-model-from-day-one.md),
[ADR-0010](v2/adr/0010-one-daemon-many-users.md)).

## 8. Multi-user (optional)

```bash
bin/resy-snipe user list
bin/resy-snipe user invite teammate@example.com
bin/resy-snipe user accept-invite <token>
```

`user invite` returns a one-shot token; the invitee runs
`user accept-invite <token>` on a (separate) install to claim it. The
operator who ran `user seed` is the only principal authorized to
invite. There is no self-registration endpoint
([ADR-0011](v2/adr/0011-operator-issued-invites-no-self-registration.md)).

For the data-model details see
[docs/v2/design/multi-user.md](v2/design/multi-user.md).

## 9. Legacy snipe path (still works, not recommended)

The v1 one-shot snipe binary entry point continues to function for
operators who want a single-process snipe without the Service layer:

```bash
bin/resy-snipe \
  -user you@example.com \
  -venue-id 49716 \
  -res-times 18:30,19:00 \
  -snipe-time 00:00 \
  -date 2026-06-09 -party-size 2
```

Differences from v1:

- `-venue-id` is **required** — the v1 built-in venue catalog (Dead
  Rabbit, Rubirosa, Carbone, …) was removed in M1-22. Use
  `venue resolve` to look up the numeric id, or read the
  `venue_id=…` query parameter from `/4/find` in the browser's
  network tab.
- Everything else (release strategies, retry-window, dry-run on
  missing `-user`) is unchanged.

If you're starting fresh, prefer `quest plan` → `quest create`. The
legacy path doesn't write to the v2 quests table and won't show up in
`quest list`.

## 10. Anti-bot signing (optional, peak-load only)

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
bin/resy-snipe quest create <url> ...
```

The boot log will confirm:
`anti-bot signer: subprocess bin=…/obscura-resy.sh`.

For the deeper "what is a signer / why does this exist" explanation,
see [signers.md](signers.md). For tuning + adding your own signer,
see [`signers/README.md`](../signers/README.md). Unchanged from v1.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `no valid session — run 'resy-snipe login' first` | No session for that `-user`, or it's expired. | `bin/resy-snipe login` |
| `provider: anti-bot challenge` | PerimeterX flagged the call. | Wait, retry off-peak, or wire `RESY_SNIPE_SIGNER_BIN` (see §10). |
| `provider: rate limited` | Resy returned 429. | Lower `-retry-window`, increase `-snipe-time` granularity, or wait. |
| `provider: book token expired` | `/3/details` token aged out before `/3/book` fired. | Engine retries automatically; if persistent, the venue's anti-bot is hot — try off-peak. |
| `dry-run: pass -user` (legacy path) | You forgot `-user` on the v1 snipe entry. | Add `-user you@example.com`. |
| `quest not found` | Wrong `-user` for that `qst_…`, or the id is wrong. | `quest list -user <usr_…>` to find it. |
| `the plan changed — re-run` | Venue config drifted between `quest plan` and `quest create` (e.g. release window observation updated). | Re-run `quest plan`, inspect the new hash, then `quest create`. |
| `idempotency conflict` | Same `-idempotency-key` used with a different Goal within 24h. | Use a fresh key, or repeat the exact original Goal to get the original Quest back. |
| `ambiguous query "<name>" (N candidates)` | `venue resolve` by freeform name matched multiple venues. | Re-run with the printed `slug:city` form or the URL. |
| `venue not found` | Resolver got zero hits. | Double-check the URL/spelling; cache may be stale — pass `-no-cache`. |

## What changed from v1

- v1 had a **hardcoded venue catalog** (Dead Rabbit, Rubirosa, Carbone,
  Don Angie, …) compiled into the binary. v2 resolves dynamically —
  the catalog is gone, replaced by
  [`internal/resolver`](v2/design/resolver.md). Use `venue resolve`.
- v1 made you **compute `-snipe-time` from release-window math**
  (30 days ahead at midnight, 14 days ahead at noon, …). v2's
  [Planner](v2/design/planner.md) does that — you state the Goal
  (URL + date + party + prefs), the Planner picks the strategy and
  computes the drop moment.
- v1 was **single-user implicit** — there was no `users` table. v2 is
  **multi-user from day one**; the single-operator install is just
  one row, seeded once via `user seed`
  ([ADR-0005](v2/adr/0005-multi-user-data-model-from-day-one.md)).
- v1's `bin/resy-snipe -venue-id …` invocation **still works** for
  one-shot snipes but isn't the recommended path. The v1 venue
  catalog is removed; `-venue-id` is now required.
- `quest create` is a **plan-first commit** — you (or an agent) see
  the canonical Plan and its hash before anything is persisted. See
  [ADR-0012](v2/adr/0012-plan-first-ux.md).

The engine, providers seam, store, clock, signer, and notifier
interfaces are unchanged. v2 added layers above the engine (Goal →
Resolver → Planner → Service); the engine state machine and booking
race ([docs/state-machine.md](state-machine.md),
[docs/anti-bot.md](anti-bot.md)) are byte-for-byte v1.

Full re-architecture rationale: [docs/v2/README.md](v2/README.md).

---

That's it. For deeper reading: [docs/README.md](README.md) is the
index into the rest of the architecture/invariants/laws docs;
[docs/v2/](v2/) is the v2-specific design corpus.
