// Package http is the daemon's HTTP transport layer. It mounts one
// route per Service-layer verb (M2-11) plus the operational endpoints
// (/healthz, /readyz, /metrics — M2-14); the handlers are intentionally
// thin adapters that translate JSON ↔ Service calls and map sentinel
// errors to the {code, message, details} envelope from
// docs/v2/design/daemon.md §4.3.
//
// The transport never owns business logic. Every decision that
// matters (validation, idempotency, tenancy) lives in
// internal/service; this package's job is wire-format and routing.
//
// Per ADR-0009, the daemon does not terminate TLS — a reverse proxy
// (Caddy, nginx, tailscale serve) sits in front. The trusted-proxy
// CIDR list (M2-13) governs whether X-Forwarded-* headers are
// honored.
package http
