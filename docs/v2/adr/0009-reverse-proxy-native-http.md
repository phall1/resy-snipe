# ADR 0009: HTTP server is reverse-proxy native, not internet-facing

**Status**: Accepted
**Date**: 2026-05-10
**Decision-makers**: phall
**Related**: [ADR-0003](0003-daemon-first-cli-as-client.md),
[ADR-0007](0007-self-hosted-only-no-saas.md),
[design/daemon.md](../design/daemon.md)

## Context

The daemon's HTTP server (and HTTP-MCP transport) will sit behind
*something* in any real deployment: Caddy, nginx, Traefik, Tailscale
Funnel, Cloudflare Tunnel. Operators have strong opinions about TLS
termination and authentication-at-the-edge. We will not win that
argument by shipping a "TLS-and-Let's-Encrypt-built-in" daemon — we
will lose it by being annoying to integrate.

## Decision

The daemon's HTTP layer is reverse-proxy native:

- **Bind to `127.0.0.1` by default.** Override via
  `--bind 0.0.0.0:port` only if the operator explicitly accepts that
  exposure.
- **No TLS in the daemon.** Operators terminate TLS at their proxy.
  The `--bind 0.0.0.0` path emits a startup warning recommending a
  TLS terminator.
- **Respect `X-Forwarded-Proto`, `X-Forwarded-Host`,
  `X-Forwarded-For`** when (and only when) the source IP is in a
  configurable trusted-proxy CIDR list (defaults to RFC1918 + loopback).
- **No assumption about being at `/`.** All routes assume a configurable
  base path (defaults to `/`); links generated for emails / MCP
  resource URIs use the canonical external URL the operator supplies.
- **No automatic redirects to HTTPS.** The proxy does that.

## Consequences

**Positive**
- Drop-in behind any of Caddy/nginx/Traefik/Tailscale. We ship example
  configs for each.
- No certificate management in the daemon. No Let's Encrypt rate
  limits, no DNS-01 dance, no expiry calendaring. Operator's problem
  by design.
- Audit and rate-limit decisions can rely on real client IPs once the
  trusted-proxy list is set.

**Negative**
- "Bare daemon, no proxy" is not a supported production posture. The
  operator who runs `resy-snipe serve` and exposes port 8080 to the
  internet without a proxy will get clear-text traffic and a startup
  warning, but no protection.
- One more piece of operator config (the trusted-proxy list).

**Neutral**
- Loopback-only is the default. `localhost` deployments (operator and
  agents on the same box) need zero proxy config.

## Alternatives considered

1. **Built-in TLS with ACME.** *Rejected:* duplicates what Caddy does
   (better) for free. Adds an outbound CA dependency and a cert
   reload story to the daemon for zero gain when a proxy is right
   there.
2. **mTLS only, no header trust.** *Rejected:* great in theory,
   miserable in practice for a friends-and-family setup. Issuing
   client certs to your friend's iPhone Safari is not the UX we want.
3. **No HTTP at all; require Tailscale + raw TCP.** *Rejected:* MCP
   and CLI both want HTTP, MCP-over-HTTP is a thing. Tailscale-only
   would force the agent surface into stdio-only, which loses the
   "Claude in the cloud calls our daemon" use case.

## Notes

Example reverse-proxy configs ship in
[deploy/](../../../deploy/) (created in M2):

- `deploy/caddy/Caddyfile.example`
- `deploy/nginx/resy-snipe.conf.example`
- `deploy/traefik/dynamic.yml.example`

Each example shows TLS termination, the `X-Forwarded-*` headers, and
how to expose the MCP-HTTP path separately if the operator wants
different auth there.

Tailscale users have a simpler path: `tailscale serve` directly
proxies the daemon's `127.0.0.1:port`, no intermediate proxy needed.
That's documented in [design/daemon.md](../design/daemon.md).
