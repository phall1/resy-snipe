package service

import "errors"

// ErrNotImplemented is returned by Stub method implementations until
// the real Service lands. Callers should never see it in production.
var ErrNotImplemented = errors.New("service: not implemented")

// Sentinel errors returned by Service methods. Transports own the
// mapping from each sentinel to their wire shape; the Service defines
// the sentinel set. Every error returned from a Service method either
// is one of these or wraps one — callers branch with errors.Is.
var (
	// ErrNotFound is returned when a resource does not exist or is not
	// owned by the caller. The two cases are folded into one to avoid
	// leaking cross-tenant existence.
	ErrNotFound = errors.New("service: not found")

	// ErrUnauthorized is returned when the caller's token is missing,
	// invalid, or lacks the required role for the action.
	ErrUnauthorized = errors.New("service: unauthorized")

	// ErrConflict is returned when an idempotency key is reused with a
	// different action target.
	ErrConflict = errors.New("service: conflict")

	// ErrInvalidPlanHash is returned by CreateQuest when the supplied
	// plan hash does not match the freshly recomputed Plan.
	ErrInvalidPlanHash = errors.New("service: plan hash mismatch")

	// ErrVenueNotFound is returned when the resolver cannot identify a
	// venue from the supplied query.
	ErrVenueNotFound = errors.New("service: venue not found")

	// ErrVenueAmbiguous is returned when a venue query matches more
	// than one candidate and the caller must disambiguate.
	ErrVenueAmbiguous = errors.New("service: venue ambiguous")

	// ErrUpstreamUnavailable is returned when a downstream provider
	// (Resy, notifier, signer) is unreachable or returns a transient
	// failure that the Service cannot mask.
	ErrUpstreamUnavailable = errors.New("service: upstream unavailable")

	// ErrInvalidArgument is returned when input validation fails before
	// any side effect is attempted.
	ErrInvalidArgument = errors.New("service: invalid argument")

	// ErrTokenExpired is returned when the caller presents a bearer
	// token whose expiry has passed.
	ErrTokenExpired = errors.New("service: token expired")

	// ErrTokenRevoked is returned when the caller presents a bearer
	// token that has been explicitly revoked.
	ErrTokenRevoked = errors.New("service: token revoked")

	// ErrInviteExpired is returned by AcceptInvite when the invite
	// token has passed its expiry.
	ErrInviteExpired = errors.New("service: invite expired")

	// ErrInviteConsumed is returned by AcceptInvite when the invite
	// token has already been redeemed.
	ErrInviteConsumed = errors.New("service: invite consumed")

	// ErrIdempotencyConflict is returned when a stored idempotency
	// response is incompatible with the current request payload.
	ErrIdempotencyConflict = errors.New("service: idempotency conflict")
)

// IsNotFound reports whether err is or wraps ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsUnauthorized reports whether err is or wraps ErrUnauthorized.
func IsUnauthorized(err error) bool { return errors.Is(err, ErrUnauthorized) }

// IsConflict reports whether err is or wraps ErrConflict.
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }

// IsInvalidArgument reports whether err is or wraps ErrInvalidArgument.
func IsInvalidArgument(err error) bool { return errors.Is(err, ErrInvalidArgument) }
