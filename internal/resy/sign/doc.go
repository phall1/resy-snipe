// Package sign exposes the seam the Resy adapter consults before each
// /3/details and /3/book call to populate PerimeterX-aware signing
// headers. It is the recovery half of the anti-bot story whose
// detection half lives in errors.go (classifyHTTP →
// providers.ErrAntiBotChallenge).
//
// The package defines a Signer interface plus two implementations:
//
//   - Noop, which returns no headers and never errors. This is what
//     wires up by default and is what every existing test sees, so a
//     codebase that has never heard of signing keeps working
//     unchanged.
//   - Subprocess, which shells out to an external signing binary
//     (configured via RESY_SNIPE_SIGNER_BIN). Its expected wire format
//     is JSON on stdout: {"headers": {"x-px-something": "..."}}. The
//     subprocess is invoked with `sign --provider resy` to produce
//     headers; on ErrAntiBotChallenge the adapter calls
//     Signer.Reset(ctx) which invokes `sign reset --provider resy` so
//     the upstream tool can mint a fresh cookie/token set.
//
// The seam is the deliverable. The Subprocess wrapper is a stub for
// the hypothetical upstream — at the time of writing,
// `mvanhorn/cli-printing-press` is a CLI generator, not a Resy
// signing toolkit, so Subprocess is not the production default. When
// a real upstream lands, the wiring in cmd/ swaps Noop → Subprocess
// without touching the adapter or the engine.
package sign
