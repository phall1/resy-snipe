package resy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// Calendar implements providers.Provider.Calendar against Resy's
// GET /4/venue/calendar endpoint. Resy's "scheduled" array carries one
// entry per date in [start_date, end_date]; each entry's
// inventory.reservation field is "available" (or similar) when slots
// can be booked, or "sold-out" otherwise.
//
// The DiscoveredRelease loop relies on the FAR-EDGE date appearing
// in the scheduled array as the signal that a newly released date
// has dropped. The returned providers.Calendar preserves array order;
// Days[len-1] is therefore the freshest released date.
func (c *Client) Calendar(
	ctx context.Context,
	ref domain.VenueRef,
	r providers.DateRange,
) (providers.Calendar, error) {
	if ref.Provider != providerID {
		return providers.Calendar{}, fmt.Errorf("resy.Calendar: provider mismatch %q", ref.Provider)
	}
	if ref.Ref == "" {
		return providers.Calendar{}, errors.New("resy.Calendar: empty venue ref")
	}
	if r.Start.IsZero() || r.End.IsZero() {
		return providers.Calendar{}, errors.New("resy.Calendar: DateRange requires Start and End")
	}

	numSeats := r.PartySize
	if numSeats <= 0 {
		// Resy's calendar is party-coarse; a default of 2 yields the
		// most permissive view.
		numSeats = 2
	}

	q := url.Values{
		"venue_id":   {ref.Ref},
		"num_seats":  {strconv.Itoa(numSeats)},
		"start_date": {r.Start.String()},
		"end_date":   {r.End.String()},
	}

	// /4/venue/calendar is the DiscoveredRelease polling endpoint and
	// shares Find's anti-bot exposure under heavy load, so it routes
	// through the sign+retry envelope. GET is idempotent.
	body, resp, err := c.doSignedAndRetry(ctx, http.MethodGet, "/4/venue/calendar", q, nil, "", nil)
	if err != nil {
		return providers.Calendar{}, fmt.Errorf("resy.Calendar: %w", err)
	}
	if cls := classify(resp.StatusCode, body); cls != nil {
		return providers.Calendar{}, fmt.Errorf("resy.Calendar: %w", cls)
	}

	var raw CalendarResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return providers.Calendar{}, fmt.Errorf("resy.Calendar: parse response: %w", err)
	}

	out := providers.Calendar{
		Venue: ref,
		Days:  make([]providers.CalendarDay, 0, len(raw.Scheduled)),
	}
	for _, day := range raw.Scheduled {
		d, err := domain.ParseDate(day.Date)
		if err != nil {
			return providers.Calendar{}, fmt.Errorf("resy.Calendar: bad date %q: %w", day.Date, err)
		}
		out.Days = append(out.Days, providers.CalendarDay{
			Date:      d,
			Available: isAvailable(day.Inventory.Reservation),
		})
	}
	return out, nil
}

// isAvailable normalises Resy's inventory.reservation strings into a
// boolean. "available" is the canonical value; we accept any non-
// "sold-out" / "closed" value as available so a forward-compatible
// extension to the enum doesn't silently flip days off.
func isAvailable(s string) bool {
	switch s {
	case "sold-out", "sold_out", "soldout", "closed", "":
		return false
	default:
		return true
	}
}
