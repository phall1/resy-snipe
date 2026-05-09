package main

import (
	"context"
	"errors"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resy"
)

// providerAdapter bridges *resy.Client to the engine's
// providers.Provider interface. The Resy client predates the interface
// and exposes a more concrete signature for Login (returning *Session
// directly so the login subcommand can read User() / ExpiresAt() without
// a type assertion). Rather than weaken the resy package's typing for
// every caller, this adapter — owned by the wiring layer — narrows the
// signatures down to providers.Provider where the engine needs them.
//
// The adapter embeds *resy.Client so the SlotPreparer methods
// (PrepareSlot / ConfirmSlot, which already match providers types) pass
// through unmodified. RunBookingRace's runtime check
// `e.provider.(SlotPreparer)` therefore continues to succeed.
type providerAdapter struct {
	*resy.Client
}

// Compile-time guarantee that the adapter satisfies the engine's
// expectations. The SlotPreparer assertion is duplicated here so the
// build breaks if Resy's PrepareSlot/ConfirmSlot signatures ever drift
// out of step with the engine's expectations.
var (
	_ providers.Provider = (*providerAdapter)(nil)
	_ slotPreparer       = (*providerAdapter)(nil)
)

// slotPreparer mirrors engine.SlotPreparer locally so we can compile-
// time check satisfaction here without importing engine into a file
// that engine_test would otherwise have to mock around. The shape is
// identical by construction; if the engine's interface drifts, the
// runtime type-assertion in RunBookingRace would catch it before any
// real booking happens, but compile-time is better.
type slotPreparer interface {
	PrepareSlot(ctx context.Context, slot providers.Slot, sess providers.Session) (providers.Slot, error)
	ConfirmSlot(ctx context.Context, slot providers.Slot, sess providers.Session, idempotencyKey string) (providers.Confirmation, error)
}

// providerID is the canonical identifier for the Resy provider.
const providerID domain.ProviderID = "resy"

// ID satisfies providers.Provider.
func (*providerAdapter) ID() domain.ProviderID { return providerID }

// Login wraps the embedded client's Login so the return type matches
// providers.Provider. The concrete *resy.Session pointer is returned as
// a providers.Session (which it already satisfies) — callers that need
// the typed session retain it through the SnipeID-attached state, but
// engine code never inspects it directly.
func (a *providerAdapter) Login(ctx context.Context, creds providers.Credentials) (providers.Session, error) {
	sess, err := a.Client.Login(ctx, creds)
	if err != nil {
		// Returning a typed-nil providers.Session would be a
		// confusing error trap; explicitly return nil.
		return nil, err
	}
	return sess, nil
}

// errSearchVenuesUnsupported is returned by SearchVenues until the
// directory feature ships. The engine never calls this on the snipe
// path; it exists only so the adapter satisfies the interface.
var errSearchVenuesUnsupported = errors.New(
	"resy: SearchVenues is not implemented on Phase 1 — venue ids are supplied via -venue-id")

// SearchVenues is a placeholder. Phase 1 takes venue ids on the
// command line; the directory experience that would consume this
// method does not exist yet.
func (*providerAdapter) SearchVenues(_ context.Context, _ providers.Query) ([]domain.Venue, error) {
	return nil, errSearchVenuesUnsupported
}
