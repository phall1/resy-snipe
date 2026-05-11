package http

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// venuesBackend is the Service surface POST /v1/venues/resolve needs.
// Defined-at-the-consumer (Law 5) so the transport doesn't depend on
// the full service.Service interface in its tests.
type venuesBackend interface {
	ResolveVenue(ctx context.Context, userID domain.UserID, query domain.VenueQuery) (domain.Venue, error)
}

// MountVenueRoutes registers POST /v1/venues/resolve on s. The route
// requires an authenticated context (the daemon wraps the /v1 surface
// with AuthMiddleware before mounting); the handler reads the caller
// user via service.CallerUserID.
func MountVenueRoutes(s *Server, backend venuesBackend) {
	s.HandleFunc("POST /v1/venues/resolve", newResolveVenueHandler(backend, s.log))
}

// resolveVenueRequest is the POST body. Either form (URL, slug+city,
// name) is accepted — the venueQueryWire discriminator decides which
// branch the resolver runs.
type resolveVenueRequest struct {
	VenueQuery venueQueryWire `json:"venue_query"`
}

func newResolveVenueHandler(b venuesBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			WriteError(w, log, fmt.Errorf("resolve_venue: %w: %s", service.ErrInvalidArgument, err.Error()))
			return
		}
		var req resolveVenueRequest
		if err := decodeJSON(body, &req); err != nil {
			WriteError(w, log, err)
			return
		}
		vq, err := decodeVenueQuery(req.VenueQuery)
		if err != nil {
			WriteError(w, log, err)
			return
		}
		uid := service.CallerUserID(r.Context())
		if uid == "" {
			WriteError(w, log, fmt.Errorf("resolve_venue: %w", service.ErrUnauthorized))
			return
		}
		venue, err := b.ResolveVenue(r.Context(), uid, vq)
		if err != nil {
			WriteError(w, log, err)
			return
		}
		writeJSONOK(w, log, encodeVenue(venue))
	}
}

// writeJSONOK is a small alias to keep the success path one-liner.
func writeJSONOK(w http.ResponseWriter, log *slog.Logger, body any) {
	WriteJSON(w, log, http.StatusOK, body)
}
