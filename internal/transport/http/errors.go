package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"resy-snipe/internal/providers"
	"resy-snipe/internal/service"
)

// Envelope is the wire-shape every error response uses. Stable across
// the /v1 surface. `details` is per-code (callers populate it via
// WriteErrorDetails); empty by default.
type Envelope struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// ErrorMapping is the sentinel→(status, code) table. The order of
// errors.Is checks matters for sentinels that share an Is graph; we
// check most specific first. Adding a new sentinel means adding a
// row here AND a row in the §4.3 table in design/daemon.md.
type ErrorMapping struct {
	Status int
	Code   string
}

// MapError turns a Service / provider error into its wire shape.
// Unknown errors collapse to 500 internal — the daemon never leaks a
// raw error string to clients (the message in 500s is the generic
// "internal server error"; the structured log records the underlying
// error for the operator).
//
// The mapping reflects the §4.3 table:
//
//	service.ErrUnauthorized      → 401 unauthenticated
//	service.ErrTokenExpired      → 401 token_expired
//	service.ErrTokenRevoked      → 401 token_revoked
//	service.ErrNotFound          → 404 not_found
//	service.ErrConflict          → 409 conflict
//	service.ErrIdempotencyConflict → 409 idempotency_conflict
//	service.ErrInvalidArgument   → 422 validation_failed
//	service.ErrInvalidPlanHash   → 422 plan_hash_mismatch
//	service.ErrVenueNotFound     → 404 venue_not_found
//	service.ErrVenueAmbiguous    → 409 venue_ambiguous
//	service.ErrUpstreamUnavailable → 502 provider_unavailable
//	service.ErrInviteExpired     → 410 invite_expired
//	service.ErrInviteConsumed    → 409 invite_consumed
//	providers.ErrRateLimited     → 429 rate_limited
//	providers.ErrAuthExpired     → 502 provider_session_expired
//	(anything else)              → 500 internal
func MapError(err error) ErrorMapping {
	switch {
	case errors.Is(err, service.ErrTokenExpired):
		return ErrorMapping{http.StatusUnauthorized, "token_expired"}
	case errors.Is(err, service.ErrTokenRevoked):
		return ErrorMapping{http.StatusUnauthorized, "token_revoked"}
	case errors.Is(err, service.ErrUnauthorized):
		return ErrorMapping{http.StatusUnauthorized, "unauthenticated"}
	case errors.Is(err, service.ErrForbidden):
		return ErrorMapping{http.StatusForbidden, "forbidden"}
	case errors.Is(err, service.ErrVenueNotFound):
		return ErrorMapping{http.StatusNotFound, "venue_not_found"}
	case errors.Is(err, service.ErrNotFound):
		return ErrorMapping{http.StatusNotFound, "not_found"}
	case errors.Is(err, service.ErrVenueAmbiguous):
		return ErrorMapping{http.StatusConflict, "venue_ambiguous"}
	case errors.Is(err, service.ErrInviteConsumed):
		return ErrorMapping{http.StatusConflict, "invite_consumed"}
	case errors.Is(err, service.ErrIdempotencyConflict):
		return ErrorMapping{http.StatusConflict, "idempotency_conflict"}
	case errors.Is(err, service.ErrConflict):
		return ErrorMapping{http.StatusConflict, "conflict"}
	case errors.Is(err, service.ErrInviteExpired):
		return ErrorMapping{http.StatusGone, "invite_expired"}
	case errors.Is(err, service.ErrInvalidPlanHash):
		return ErrorMapping{http.StatusUnprocessableEntity, "plan_hash_mismatch"}
	case errors.Is(err, service.ErrInvalidArgument):
		return ErrorMapping{http.StatusUnprocessableEntity, "validation_failed"}
	case errors.Is(err, providers.ErrRateLimited):
		return ErrorMapping{http.StatusTooManyRequests, "rate_limited"}
	case errors.Is(err, providers.ErrAuthExpired):
		return ErrorMapping{http.StatusBadGateway, "provider_session_expired"}
	case errors.Is(err, service.ErrUpstreamUnavailable):
		return ErrorMapping{http.StatusBadGateway, "provider_unavailable"}
	}
	return ErrorMapping{http.StatusInternalServerError, "internal"}
}

// WriteError emits the JSON envelope for err and logs the
// underlying error so the operator can correlate.
//
// For 500s, the wire message is generic ("internal server error")
// to avoid leaking implementation detail; the underlying err is in
// the structured log. For mapped statuses, the wire message is
// err.Error() so the client gets actionable feedback.
func WriteError(w http.ResponseWriter, log *slog.Logger, err error) {
	WriteErrorDetails(w, log, err, nil)
}

// WriteErrorDetails is the with-details variant. Handlers that want
// to surface structured context (e.g. {"quest_id": "..."}) pass it
// in via details.
func WriteErrorDetails(w http.ResponseWriter, log *slog.Logger, err error, details map[string]any) {
	m := MapError(err)
	body := Envelope{Code: m.Code, Details: details}
	if m.Status == http.StatusInternalServerError {
		body.Message = "internal server error"
		if log != nil {
			log.Error("transport: 500 response",
				"err", err.Error(),
				"code", m.Code,
			)
		}
	} else {
		body.Message = err.Error()
		if log != nil {
			log.Info("transport: error response",
				"status", m.Status,
				"code", m.Code,
				"err", err.Error(),
			)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(m.Status)
	_ = json.NewEncoder(w).Encode(body) //nolint:errchkjson // body's `any` field hides safety from the linter; we already log on failure.
}

// WriteJSON writes a 2xx JSON body. status defaults to 200 if zero.
// Errors during encoding land in the operator log but the response
// has already been committed by that point.
func WriteJSON(w http.ResponseWriter, log *slog.Logger, status int, body any) {
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil && log != nil {
		log.Error("transport: encode response", "err", err.Error())
	}
}
