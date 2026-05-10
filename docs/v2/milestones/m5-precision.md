# Milestone M5 — Precision & polish

**Status**: Scoped (sub-issues materialize when M4 closes)
**Owner**: phall
**Beads epic**: `resy-snipe-0li`
**Depends on**: M2
**Blocks**: nothing

## Goal in one sentence

Win more races. The architectural milestones (M1–M4) make the system
correct and pleasant; M5 makes it actually fast and resilient at the
moment that matters: midnight drops on contested venues.

## What ships

- **NTP-aware clock wrapper.** New `internal/clock` mode that pulls
  NTP offset on boot (and on a slow background tick) and applies it to
  `clock.Real.Now()`. Engine fires within ±50ms of the true drop
  moment regardless of laptop clock skew.
- **Signer auto-detect.** Daemon at boot looks for a signer at
  `signers/obscura-resy.sh` next to the binary, falls back to
  `RESY_SNIPE_SIGNER_BIN` env, falls back to noop with a WARN-level
  boot log. Today this is opt-in only; M5 makes the right thing happen
  by default.
- **Residential-proxy seam.** `internal/resy.Client` accepts an
  optional `*url.URL` for an outbound HTTP proxy. When the daemon runs
  on a VPS (datacenter IP, more likely to get PerimeterX'd), operator
  routes through their preferred residential proxy (Bright Data /
  Smartproxy / their own /29 of residential IPs / whatever).
- **Connection prewarm.** Engine maintains warm TLS connections to
  `api.resy.com` ahead of a known drop moment. Reduces the cold-start
  latency from ~80ms to single-digit ms.
- **Anti-bot pre-arming.** Sign before the drop moment, not at it, so
  the first `/3/details` call after the drop has a fresh cookie pack
  ready.

## What does NOT ship in M5

- Proxy management (selecting between providers, rotating IPs) —
  operator's problem, not ours.
- Distributed snipe across multiple boxes — out of scope (one daemon
  per box per ADR-0010).

## Acceptance criteria

- An integration test that injects a 400ms clock skew and asserts the
  NTP-corrected fire moment is within 50ms of true UTC.
- Boot banner reports signer status: "auto-detected" / "from env" /
  "noop (no signer)".
- A connection-prewarm metric (`resy_snipe_prewarm_connections`) is
  exposed and observable in `/metrics`.
- An end-to-end "midnight drop simulation" test against an httptest
  fixture: simulate a venue release, measure time-from-drop to first
  `/4/find` request — assert <100ms.

## Cross-references

- ADRs: 0006, 0009 (proxy seam crosses transport boundaries)
- v1 docs: `anti-bot.md`, `signers.md`
