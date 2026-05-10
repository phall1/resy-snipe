// Package daemon is the long-lived "resy-snipe serve" process —
// the system, per ADR-0003. It owns:
//
//   - the Service-layer composition (Resolver + Planner + Engine +
//     Store) bound to one *sql.DB;
//   - the sealed-secrets layer (internal/secrets);
//   - the signer subprocess (when configured);
//   - the audit log writer;
//   - the HTTP transport (mounted in M2-09);
//   - graceful shutdown and engine resume.
//
// The CLI and the (future) MCP server are HTTP clients of this
// daemon. The package's public surface is Config + Run; everything
// else is internal wiring. Subcommand-level glue lives in
// cmd/resy-snipe/serve.go (M2-08).
//
// See docs/v2/design/daemon.md for the boot sequence, the HTTP
// transport contract, and the deployment shapes.
package daemon
