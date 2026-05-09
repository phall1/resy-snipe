package sign

import "context"

// Headers is the per-call header bag a Signer contributes. Keys are
// applied verbatim — the Signer is responsible for any case-sensitive
// shaping the upstream API requires (Resy expects lowercase
// "x-resy-*" tokens). Empty values are skipped at the merge site so a
// Signer can return a zero-length value to mean "no value to send"
// without forcing callers to scrub.
//
// A nil or zero-length Headers means the Signer has nothing to add
// for this call; that is a normal idle state, not an error.
type Headers map[string]string

// Signer is the seam the Resy adapter consults before each /3/details
// and /3/book request to populate any signing headers the upstream
// anti-bot pipeline expects (x-px-*, x-resy-rotated-token, etc).
//
// Implementations must be safe for concurrent use — the engine's
// race-and-cancel layer fan-outs N ConfirmSlot calls, each of which
// will call Sign concurrently.
//
// On providers.ErrAntiBotChallenge, the adapter calls Reset(ctx)
// exactly once and retries the failed request. A Reset failure is
// non-fatal: the adapter logs and lets the original ErrAntiBot-
// Challenge propagate to the engine.
type Signer interface {
	// Sign returns the headers to merge into the next outbound request
	// for the given endpoint path (e.g. "/3/details", "/3/book"). The
	// path lets future Signer impls vary headers per endpoint without
	// an enum.
	//
	// An error is returned only when the Signer is configured but
	// cannot produce headers (subprocess timeout, cache miss with no
	// fetch path). Callers treat a Sign error as best-effort — they
	// proceed with no extra headers and let the request fail naturally
	// if signing was actually required.
	Sign(ctx context.Context, path string) (Headers, error)

	// Reset discards any cached signing state so the next Sign call
	// re-derives it. The adapter calls this after the upstream returns
	// providers.ErrAntiBotChallenge.
	//
	// Reset blocks until the discard / refetch is complete (or ctx is
	// canceled). Implementations must honor ctx cancellation so a
	// SIGTERM-driven shutdown never stalls inside Reset.
	Reset(ctx context.Context) error
}

// Noop is the zero-value Signer: it returns no headers and never
// errors. Tests and any cmd/ wiring that has not opted into a real
// signer use this so the adapter's sign-and-retry path is a true
// no-op and existing behavior is preserved.
//
// Noop is the default the adapter falls back to when no Signer is
// supplied via WithSigner, so nil-Signer never reaches the call site.
type Noop struct{}

// Sign returns an empty Headers and no error. Returning a non-nil
// (zero-length) map keeps the (value, error) contract obvious — the
// nilnil linter rule is on for a reason — and the merge site already
// no-ops on a zero-length overlay.
func (Noop) Sign(_ context.Context, _ string) (Headers, error) { return Headers{}, nil }

// Reset is a no-op; Noop has no cached state.
func (Noop) Reset(_ context.Context) error { return nil }

// compile-time interface check.
var _ Signer = Noop{}
