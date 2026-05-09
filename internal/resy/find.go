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

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// Find implements providers.Provider.Find against Resy's GET /4/find.
// It returns one providers.Slot per available timeslot at the venue
// for the requested date and party size, in the order Resy returns
// them.
//
// Resy's /4/find response carries a "results.venues" array; each venue
// has a "slots" array. The engine asks Find for one venue at a time
// (the FindRequest carries a single VenueRef), so we expect at most
// one venue back; we still iterate to be defensive against shape
// changes. Each slot's config.token is what /3/details requires as
// config_id; we capture it as ResySlotPayload.ConfigID.
//
// Times in the slot.date.start field are wall-clock strings in
// "YYYY-MM-DD HH:MM:SS" venue-local form — we extract the time
// portion as a domain.WallTime.
func (c *Client) Find(ctx context.Context, req providers.FindRequest) ([]providers.Slot, error) {
	if req.Venue.Provider != providerID {
		return nil, fmt.Errorf("resy.Find: provider mismatch %q", req.Venue.Provider)
	}
	if req.Venue.Ref == "" {
		return nil, errors.New("resy.Find: empty venue ref")
	}
	if req.Date.IsZero() {
		return nil, errors.New("resy.Find: date is required")
	}
	if req.PartySize <= 0 {
		return nil, errors.New("resy.Find: party_size > 0 required")
	}

	q := url.Values{
		"lat":        {"0"},
		"long":       {"0"},
		"day":        {req.Date.String()},
		"party_size": {strconv.Itoa(req.PartySize)},
		"venue_id":   {req.Venue.Ref},
	}

	authToken := ""
	if req.Session != nil {
		if rs, ok := req.Session.(*Session); ok {
			authToken = rs.jwt
		}
	}

	// /4/find is the highest-volume polling endpoint and the one
	// PerimeterX watches most aggressively (per docs/anti-bot.md), so
	// it routes through the sign+retry envelope alongside /3/details
	// and /3/book. GET is idempotent — a retry after Reset cannot
	// create duplicate state.
	body, resp, err := c.doSignedAndRetry(ctx, http.MethodGet, "/4/find", q, nil, authToken, nil)
	if err != nil {
		return nil, fmt.Errorf("resy.Find: %w", err)
	}
	if cls := classify(resp.StatusCode, body); cls != nil {
		return nil, fmt.Errorf("resy.Find: %w", cls)
	}

	var raw FindResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("resy.Find: parse response: %w", err)
	}

	var slots []providers.Slot
	for _, v := range raw.Results.Venues {
		for _, s := range v.Slots {
			wall, err := parseSlotWallTime(s.Date.Start)
			if err != nil {
				return nil, fmt.Errorf("resy.Find: parse start %q: %w", s.Date.Start, err)
			}
			slots = append(slots, providers.Slot{
				Venue:     req.Venue,
				Date:      req.Date,
				Time:      wall,
				TableType: s.Config.Type,
				PartySize: req.PartySize,
				Payload: domain.ResySlotPayload{
					ConfigID:    s.Config.Token,
					TemplateID:  strconv.FormatInt(s.Config.ID, 10),
					SeatingType: s.Config.Type,
				},
			})
		}
	}
	if len(slots) == 0 {
		return nil, fmt.Errorf("resy.Find: %w", providers.ErrInventoryEmpty)
	}
	return slots, nil
}

// parseSlotWallTime accepts Resy's "YYYY-MM-DD HH:MM:SS" datetime
// strings and returns the time-of-day component as a WallTime.
func parseSlotWallTime(s string) (domain.WallTime, error) {
	parts := strings.Split(strings.TrimSpace(s), " ")
	if len(parts) != 2 {
		return domain.WallTime{}, fmt.Errorf("expected YYYY-MM-DD HH:MM:SS, got %q", s)
	}
	return domain.ParseWallTime(parts[1])
}
