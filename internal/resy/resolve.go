package resy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// ResolveVenue implements providers.Provider.ResolveVenue against
// Resy's GET /3/venue endpoint. The endpoint takes a (url_slug,
// location) pair — the location is Resy's city slug ("washington-dc",
// "ny", "la") — and returns the full venue config blob. We project
// only the fields the engine cares about: the integer venue id, the
// display name, and the IANA-loadable timezone string.
//
// /3/venue is a metadata read, not a polling endpoint, so it does not
// route through the sign+retry envelope: the PerimeterX exposure that
// motivated signing for /4/find, /3/details, and /3/book does not
// apply here, and the resolver's caller (the URL/slug → Venue path)
// runs at most once per planning round-trip.
//
// Error mapping:
//   - HTTP 404 → wrapped providers.ErrVenueNotFound
//   - any other classify(...) result → its sentinel
//   - 2xx with malformed JSON → wrapped providers.ErrParseFailure
//   - 2xx with empty body → wrapped providers.ErrVenueNotFound
//     (Resy occasionally returns 200 + empty for unknown slug+city)
//   - missing time_zone in the response → wrapped ErrParseFailure
//   - unknown timezone → wrapped ErrParseFailure
func (c *Client) ResolveVenue(ctx context.Context, slug, city string) (domain.Venue, error) {
	slug = strings.TrimSpace(slug)
	city = strings.TrimSpace(city)
	if slug == "" {
		return domain.Venue{}, errors.New("resy.ResolveVenue: slug is required")
	}
	if city == "" {
		return domain.Venue{}, errors.New("resy.ResolveVenue: city is required")
	}

	q := url.Values{
		"url_slug": {slug},
		"location": {city},
	}
	body, resp, err := c.do(ctx, http.MethodGet, "/3/venue", q, nil, "")
	if err != nil {
		return domain.Venue{}, fmt.Errorf("resy.ResolveVenue: %w", err)
	}
	if cls := classify(resp.StatusCode, body); cls != nil {
		// classify treats 200 + matching patterns (inventory/MFA) as
		// signals; neither applies to /3/venue. Pass-through is correct
		// for 404 → ErrVenueNotFound and any other taxonomied status.
		// One special case: a 2xx that classify ignored will fall
		// through (cls == nil) and we parse the body below.
		return domain.Venue{}, fmt.Errorf("resy.ResolveVenue: %w", cls)
	}
	if len(body) == 0 || isJSONNull(body) {
		return domain.Venue{}, fmt.Errorf("resy.ResolveVenue: empty body: %w", providers.ErrVenueNotFound)
	}

	var raw VenueResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return domain.Venue{}, fmt.Errorf("resy.ResolveVenue: parse response: %w",
			fmt.Errorf("%w: %s", providers.ErrParseFailure, err.Error()))
	}
	if raw.ID.Resy == 0 {
		return domain.Venue{}, fmt.Errorf("resy.ResolveVenue: response missing id.resy: %w",
			providers.ErrParseFailure)
	}
	if raw.Locale.TimeZone == "" {
		return domain.Venue{}, fmt.Errorf("resy.ResolveVenue: response missing locale.time_zone: %w",
			providers.ErrParseFailure)
	}
	tz, err := time.LoadLocation(raw.Locale.TimeZone)
	if err != nil {
		return domain.Venue{}, fmt.Errorf("resy.ResolveVenue: load tz %q: %w",
			raw.Locale.TimeZone, fmt.Errorf("%w: %s", providers.ErrParseFailure, err.Error()))
	}

	v := domain.Venue{
		Provider: providerID,
		Ref:      strconv.FormatInt(raw.ID.Resy, 10),
		Name:     raw.Name,
		TZ:       tz,
	}
	if err := v.Validate(); err != nil {
		return domain.Venue{}, fmt.Errorf("resy.ResolveVenue: invalid venue: %w", err)
	}
	return v, nil
}

// isJSONNull is true for the JSON literal `null` (with optional
// whitespace) — the wire form Resy occasionally returns when a venue
// lookup misses but the platform does not produce a 404. Treating it
// as ErrVenueNotFound keeps callers on a single not-found code path.
func isJSONNull(b []byte) bool {
	return strings.TrimSpace(string(b)) == "null"
}
