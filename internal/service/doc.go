// Package service is the transport-agnostic API of the system. Both
// the HTTP daemon and the MCP server consume the Service interface
// in process; neither transport package depends on the other. The
// Service knows about domain types and the layers below it
// (Resolver, Planner, Engine, Store, Secrets, Notifier); it does not
// know about HTTP, JSON-RPC, MCP, status codes, or SSE. Adding a
// verb to the system is one new method on Service plus the wiring in
// each transport. See docs/v2/design/service-layer.md for the
// contract.
package service
