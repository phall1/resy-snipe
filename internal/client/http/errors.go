package httpclient

import (
	"encoding/json"
	"errors"
	"fmt"

	"resy-snipe/internal/providers"
	"resy-snipe/internal/service"
)

// Error is the rich error the client returns for any non-2xx response.
// It carries the wire envelope verbatim (Code, Message, Details) and
// wraps the matching service sentinel so callers can branch with
// errors.Is — e.g. errors.Is(err, service.ErrNotFound) — and also
// extract structured context via a type assertion:
//
//	var herr *httpclient.Error
//	if errors.As(err, &herr) {
//	    qid, _ := herr.Details["quest_id"].(string)
//	}
//
// Status is the HTTP status code; Code is the §4.3 string code; the
// underlying sentinel for errors.Is is the wrapped sentinel.
type Error struct {
	Status   int
	Code     string
	Message  string
	Details  map[string]any
	sentinel error
}

// Error implements the error interface; the wire message is the human-
// facing part, and the code+status are appended so logs distinguish
// "validation_failed" from "not_found" at a glance.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return fmt.Sprintf("http %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("http %d %s: %s", e.Status, e.Code, e.Message)
}

// Unwrap exposes the wrapped service sentinel for errors.Is.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.sentinel
}

// codeToSentinel is the inverse of the server's MapError. New codes
// MUST be added here in lockstep with the server's mapping table. An
// unknown code surfaces as a bare Error with no wrapped sentinel —
// errors.Is against any service sentinel will return false, which is
// the safe default (the caller sees an opaque "code=foo" error).
//
// Note: providers.ErrRateLimited and providers.ErrAuthExpired escape
// the service package, so we wrap them here too — a CLI that wants to
// branch on "rate limited" without depending on the provider package
// can also use errors.Is(err, service.ErrUpstreamUnavailable) since
// the server maps "provider_unavailable" → ErrUpstreamUnavailable.
func codeToSentinel(code string) error {
	switch code {
	case "unauthenticated":
		return service.ErrUnauthorized
	case "token_expired":
		return service.ErrTokenExpired
	case "token_revoked":
		return service.ErrTokenRevoked
	case "forbidden":
		return service.ErrForbidden
	case "not_found":
		return service.ErrNotFound
	case "venue_not_found":
		return service.ErrVenueNotFound
	case "conflict":
		return service.ErrConflict
	case "idempotency_conflict":
		return service.ErrIdempotencyConflict
	case "venue_ambiguous":
		return service.ErrVenueAmbiguous
	case "invite_consumed":
		return service.ErrInviteConsumed
	case "invite_expired":
		return service.ErrInviteExpired
	case "validation_failed":
		return service.ErrInvalidArgument
	case "plan_hash_mismatch":
		return service.ErrInvalidPlanHash
	case "rate_limited":
		return providers.ErrRateLimited
	case "provider_session_expired":
		return providers.ErrAuthExpired
	case "provider_unavailable":
		return service.ErrUpstreamUnavailable
	}
	return nil
}

// envelopeWire mirrors the server's Envelope. Kept in this package to
// avoid pulling the server's transport package in as a dependency.
type envelopeWire struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// parseErrorEnvelope decodes a non-2xx response body into an *Error.
// A body that fails JSON parsing surfaces as a generic Error with no
// wrapped sentinel — the body could be HTML from a reverse proxy or
// truncated mid-stream, neither of which should crash the caller.
func parseErrorEnvelope(status int, body []byte) *Error {
	out := &Error{Status: status}
	if len(body) == 0 {
		out.Message = fmt.Sprintf("http %d with empty body", status)
		return out
	}
	var env envelopeWire
	if err := json.Unmarshal(body, &env); err != nil {
		out.Message = fmt.Sprintf("http %d: undecodable body: %s", status, err.Error())
		return out
	}
	out.Code = env.Code
	out.Message = env.Message
	out.Details = env.Details
	out.sentinel = codeToSentinel(env.Code)
	return out
}

// asError narrows err to *Error for callers that want the structured
// view. Returns nil when err does not wrap one (e.g. transport errors).
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
