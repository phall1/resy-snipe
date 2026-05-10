# docs/

The durable reference for resy-snipe. Code is the source of truth for
*what* the system does; this tree is the source of truth for *why* it
does it that way and *what stays true* across changes.

## Reading order

**If you just want to use the binary**: jump to
**[getting-started.md](getting-started.md)** — five-minute walkthrough
from clone to live snipe, with troubleshooting.

If you're new to the codebase, read in this order:

1. **[architecture.md](architecture.md)** — packages, dependency graph,
   the seams that matter. ~5 min.
2. **[state-machine.md](state-machine.md)** — the lifecycle every
   snipe walks and the transition table. ~5 min.
3. **[release-strategies.md](release-strategies.md)** — the three ways
   a snipe can wait for inventory. ~5 min.
4. **[invariants.md](invariants.md)** — properties the system promises
   to preserve. Read before refactoring anything load-bearing. ~10 min.
5. **[laws.md](laws.md)** — conventions the linter and reviewers
   enforce. Read once; refer back. ~5 min.

For specific topics:
- **[anti-bot.md](anti-bot.md)** — Resy's defense surface and our gaps.
- **[signers.md](signers.md)** — what a "signer" is, why it lives outside the Go binary, when you need one, and how to debug.
- **[opentable-mapping.md](opentable-mapping.md)** — design exercise
  validating the Provider interface against a second concrete provider.
- **[state.md](state.md)** — what's wired and what's not, right now.
- **[work-items.md](work-items.md)** — open epics + how to find work.

## v2 re-architecture (in progress)

The current binary is a scheduled HTTP client that requires the user to
be the planner (look up venue ids, compute snipe times, pick release
strategies). v2 inverts that: the user states a Goal, the system
resolves + plans + executes. The eventual deployment is a friends-and-
family homelab daemon with both human (CLI/TUI) and agent (MCP)
front-ends sharing one Service-layer interface.

- **[v2/README.md](v2/README.md)** — v2 design tree index.
- **[v2/design/overview.md](v2/design/overview.md)** — the whole
  architecture in one diagram + 5 paragraphs.
- **[v2/adr/](v2/adr/)** — twelve load-bearing decisions, one ADR each.
- **[v2/milestones/](v2/milestones/)** — M1 (goal-driven CLI +
  multi-user schema), M2 (daemon), M3 (MCP), M4 (TUI + notifications),
  M5 (precision + polish).

This `docs/` tree remains authoritative for the v1 code paths shipping
today. v2 docs describe the target; when v2 lands, the v1 docs that no
longer apply move into `v2/` (or are deleted) and the cross-references
flip.

## Conventions for this tree

- **Stable, not chronological**. Don't add dated entries. If a doc
  goes stale, edit it; if a section is no longer load-bearing, delete it.
- **Cross-link generously**. Every doc should reference the files it
  describes (`internal/engine/run.go:118`-style line refs are fine —
  they age slowly and the linker on GitHub follows them).
- **One claim per sentence**. Bullet lists beat paragraphs when the
  goal is a future reader running `grep` for a specific rule.
- **No code dumps**. Reference functions; don't paste their bodies.
  When a doc and the code disagree, code wins — but that means the
  doc was wrong and needs an edit, not a copy-paste refresh.

## When to add a new doc

Add a new file when an explanation is too big to live inline in the
package's `doc.go` and applies across more than one package. Single-
package architecture lives in `internal/<pkg>/doc.go` so `go doc`
finds it.
