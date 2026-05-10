package resy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// searchPerPage caps how many candidates the adapter asks Resy for in
// one search call. The design (docs/v2/design/resolver.md) caps the
// resolver's returned candidate list at 25; we ask for the same here
// so the adapter never trims data the resolver would otherwise want
// to surface in a disambiguation picker.
const searchPerPage = 25

// SearchVenuesByName implements providers.Provider.SearchVenuesByName
// against Resy's POST /3/venuesearch/search endpoint. It performs a
// freeform name search and returns candidate venues in Resy's
// relevance order (the API is backed by Elasticsearch and orders hits
// by score; the adapter preserves that order verbatim).
//
// Returned domain.Venue values are intentionally PARTIAL:
//   - Provider, Ref (Resy venue id stringified), and Name are filled
//     from the hit.
//   - TZ is *nil*. The search response carries city codes ("ny",
//     "washington-dc", ...) but not IANA timezones; the resolver is
//     expected to call ResolveVenue(slug, city) once the human (or the
//     caller) picks a candidate, which yields a fully-validated
//     domain.Venue.
//
// This means the returned venues will FAIL domain.Venue.Validate() —
// callers that need a Validate-passing venue must round-trip through
// ResolveVenue. The resolver layer (M1-05) is the intended caller and
// handles this contract.
//
// /3/venuesearch/search is a metadata read, not a polling endpoint,
// so — like /3/venue — it bypasses the sign+retry envelope. The
// classifier handles transport failures uniformly via the existing
// taxonomy.
//
// Error mapping:
//   - empty / whitespace query → input-validation error (caller's bug)
//   - HTTP 4xx/5xx / anti-bot → wrapped sentinel via classify()
//   - 2xx with malformed JSON → wrapped providers.ErrParseFailure
//   - 2xx with empty hits → ([]domain.Venue{}, nil); "no match" is a
//     signal the resolver classifies, not a transport error.
func (c *Client) SearchVenuesByName(ctx context.Context, query string) ([]domain.Venue, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, errors.New("resy.SearchVenuesByName: query is required")
	}

	reqBody := SearchRequest{
		Query:   q,
		PerPage: searchPerPage,
		Types:   []string{"venue"},
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("resy.SearchVenuesByName: marshal request: %w", err)
	}

	// Resy expects application/json for this POST. The default header
	// set by applyHeaders is x-www-form-urlencoded; override per-call.
	body, resp, err := c.doWithExtraHeaders(
		ctx, http.MethodPost, "/3/venuesearch/search",
		nil, bytes.NewReader(bodyBytes), "",
		map[string]string{"Content-Type": "application/json"},
	)
	if err != nil {
		return nil, fmt.Errorf("resy.SearchVenuesByName: %w", err)
	}
	if cls := classify(resp.StatusCode, body); cls != nil {
		return nil, fmt.Errorf("resy.SearchVenuesByName: %w", cls)
	}

	var raw SearchResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("resy.SearchVenuesByName: parse response: %w",
			fmt.Errorf("%w: %s", providers.ErrParseFailure, err.Error()))
	}

	// Empty hits → empty slice + nil error. The resolver decides what
	// "no matches" means in its calling context; the adapter does not
	// editorialize.
	venues := make([]domain.Venue, 0, len(raw.Search.Hits))
	for _, h := range raw.Search.Hits {
		if h.ID.Resy == 0 || h.URLSlug == "" {
			// Defensive: a hit without a venue id or slug cannot be
			// resolved further. Skip rather than fail the whole search;
			// future Resy schema drift on one entry should not blank
			// out the rest of a relevance-ordered list.
			continue
		}
		venues = append(venues, domain.Venue{
			Provider: providerID,
			Ref:      strconv.FormatInt(h.ID.Resy, 10),
			Name:     h.Name,
			// TZ intentionally nil — see godoc.
		})
	}
	return venues, nil
}
