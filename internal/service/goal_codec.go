package service

import (
	"encoding/json"
	"fmt"

	"resy-snipe/internal/domain"
)

// goal_codec.go owns the (de)serialization of a domain.Goal at the
// Service ↔ StoreBackend boundary. The Service hands a canonical JSON
// string to the StoreBackend so the persistence layer does not need
// to know about the sealed VenueQuery union; the store package owns
// only the row shape, not the codec.
//
// The shape mirrors store.MarshalGoal exactly so a row written by the
// adapter using the store-side codec deserializes here byte-for-byte
// — but the codec lives at this seam (per Law 5: define interfaces at
// the consumer) so internal/service does not need to import
// internal/store.

type goalJSON struct {
	VenueQuery  venueQueryJSON  `json:"venue_query"`
	Date        string          `json:"date"`
	Party       int             `json:"party"`
	TimePrefs   timeWindowJSON  `json:"time_prefs"`
	AccountID   string          `json:"account_id"`
	Constraints constraintsJSON `json:"constraints"`
}

type venueQueryJSON struct {
	Kind string `json:"kind"`
	URL  string `json:"url,omitempty"`
	Slug string `json:"slug,omitempty"`
	City string `json:"city,omitempty"`
	Name string `json:"name,omitempty"`
}

type timeWindowJSON struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Priority string `json:"priority"`
}

type constraintsJSON struct {
	TableTypes []string `json:"table_types,omitempty"`
	AnyTable   bool     `json:"any_table,omitempty"`
}

func marshalGoalJSON(g domain.Goal) (string, error) {
	out := goalJSON{
		VenueQuery: encodeVQ(g.VenueQuery),
		Date:       g.Date.String(),
		Party:      g.Party,
		TimePrefs: timeWindowJSON{
			Start:    g.TimePrefs.Start.String(),
			End:      g.TimePrefs.End.String(),
			Priority: g.TimePrefs.Priority.String(),
		},
		AccountID: string(g.AccountID),
		Constraints: constraintsJSON{
			TableTypes: append([]string(nil), g.Constraints.TableTypes...),
			AnyTable:   g.Constraints.AnyTable,
		},
	}
	buf, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("marshalGoalJSON: %w", err)
	}
	return string(buf), nil
}

func unmarshalGoalJSON(s string) (domain.Goal, error) {
	var raw goalJSON
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return domain.Goal{}, fmt.Errorf("unmarshalGoalJSON: %w", err)
	}
	vq, err := decodeVQ(raw.VenueQuery)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("unmarshalGoalJSON: %w", err)
	}
	d, err := domain.ParseDate(raw.Date)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("unmarshalGoalJSON date: %w", err)
	}
	start, err := domain.ParseWallTime(raw.TimePrefs.Start)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("unmarshalGoalJSON time_prefs.start: %w", err)
	}
	end, err := domain.ParseWallTime(raw.TimePrefs.End)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("unmarshalGoalJSON time_prefs.end: %w", err)
	}
	return domain.Goal{
		VenueQuery: vq,
		Date:       d,
		Party:      raw.Party,
		TimePrefs: domain.TimeWindow{
			Start:    start,
			End:      end,
			Priority: parsePriorityJSON(raw.TimePrefs.Priority),
		},
		AccountID: domain.AccountID(raw.AccountID),
		Constraints: domain.Constraints{
			TableTypes: raw.Constraints.TableTypes,
			AnyTable:   raw.Constraints.AnyTable,
		},
	}, nil
}

func encodeVQ(q domain.VenueQuery) venueQueryJSON {
	switch v := q.(type) {
	case domain.VenueQueryURL:
		return venueQueryJSON{Kind: "url", URL: v.URL}
	case domain.VenueQuerySlug:
		return venueQueryJSON{Kind: "slug", Slug: v.Slug, City: v.City}
	case domain.VenueQueryName:
		return venueQueryJSON{Kind: "name", Name: v.Name}
	default:
		return venueQueryJSON{Kind: "nil"}
	}
}

func decodeVQ(j venueQueryJSON) (domain.VenueQuery, error) {
	switch j.Kind {
	case "url":
		return domain.VenueQueryURL{URL: j.URL}, nil
	case "slug":
		return domain.VenueQuerySlug{Slug: j.Slug, City: j.City}, nil
	case "name":
		return domain.VenueQueryName{Name: j.Name}, nil
	case "", "nil":
		return nil, nil //nolint:nilnil // intentional optional-value sentinel
	default:
		return nil, fmt.Errorf("unknown venue_query.kind %q", j.Kind)
	}
}

func parsePriorityJSON(s string) domain.TimePriority {
	switch s {
	case "earlier":
		return domain.PriorityEarlier
	case "later":
		return domain.PriorityLater
	default:
		return domain.PriorityNone
	}
}
