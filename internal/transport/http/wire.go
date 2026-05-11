package http

import (
	"encoding/json"
	"fmt"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// wire.go owns the daemon's HTTP wire (de)serialization of domain
// types. The codec is a parallel surface to the disk codec in
// internal/service/goal_codec.go (which serializes a Goal for storage
// in the quests.goal_json column). Keeping a separate codec at the
// transport boundary lets the wire shape evolve without disturbing
// the stored JSON and keeps the sealed-union discriminator names a
// public contract specifically for HTTP clients.

// venueQueryWire is the discriminated wire shape for domain.VenueQuery.
// The Kind field is one of "url", "slug", "name"; the other fields
// are populated based on the variant.
type venueQueryWire struct {
	Kind string `json:"kind"`
	URL  string `json:"url,omitempty"`
	Slug string `json:"slug,omitempty"`
	City string `json:"city,omitempty"`
	Name string `json:"name,omitempty"`
}

func encodeVenueQuery(q domain.VenueQuery) venueQueryWire {
	switch v := q.(type) {
	case domain.VenueQueryURL:
		return venueQueryWire{Kind: "url", URL: v.URL}
	case domain.VenueQuerySlug:
		return venueQueryWire{Kind: "slug", Slug: v.Slug, City: v.City}
	case domain.VenueQueryName:
		return venueQueryWire{Kind: "name", Name: v.Name}
	default:
		return venueQueryWire{Kind: "nil"}
	}
}

func decodeVenueQuery(w venueQueryWire) (domain.VenueQuery, error) {
	switch w.Kind {
	case "url":
		return domain.VenueQueryURL{URL: w.URL}, nil
	case "slug":
		return domain.VenueQuerySlug{Slug: w.Slug, City: w.City}, nil
	case "name":
		return domain.VenueQueryName{Name: w.Name}, nil
	case "":
		return nil, fmt.Errorf("%w: venue_query.kind is required", service.ErrInvalidArgument)
	default:
		return nil, fmt.Errorf("%w: unknown venue_query.kind %q", service.ErrInvalidArgument, w.Kind)
	}
}

// timeWindowWire mirrors domain.TimeWindow on the wire. Times use the
// HH:MM(:SS) form ParseWallTime accepts; priority is the lowercase tag
// (PriorityNone elides to "none").
type timeWindowWire struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Priority string `json:"priority,omitempty"`
}

// constraintsWire mirrors domain.Constraints. Empty TableTypes is
// omitted to keep the wire body compact when callers don't care about
// table-type filtering.
type constraintsWire struct {
	TableTypes []string `json:"table_types,omitempty"`
	AnyTable   bool     `json:"any_table,omitempty"`
}

// goalWire is the wire shape of domain.Goal. The discriminator on
// VenueQuery is enforced; Date is YYYY-MM-DD; AccountID is the v2 ULID
// id (not an email).
type goalWire struct {
	VenueQuery  venueQueryWire  `json:"venue_query"`
	Date        string          `json:"date"`
	Party       int             `json:"party"`
	TimePrefs   timeWindowWire  `json:"time_prefs"`
	AccountID   string          `json:"account_id"`
	Constraints constraintsWire `json:"constraints"`
}

func encodeGoal(g domain.Goal) goalWire {
	return goalWire{
		VenueQuery: encodeVenueQuery(g.VenueQuery),
		Date:       g.Date.String(),
		Party:      g.Party,
		TimePrefs: timeWindowWire{
			Start:    g.TimePrefs.Start.String(),
			End:      g.TimePrefs.End.String(),
			Priority: g.TimePrefs.Priority.String(),
		},
		AccountID: string(g.AccountID),
		Constraints: constraintsWire{
			TableTypes: append([]string(nil), g.Constraints.TableTypes...),
			AnyTable:   g.Constraints.AnyTable,
		},
	}
}

func decodeGoal(w goalWire) (domain.Goal, error) {
	vq, err := decodeVenueQuery(w.VenueQuery)
	if err != nil {
		return domain.Goal{}, err
	}
	d, err := domain.ParseDate(w.Date)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("%w: date: %s", service.ErrInvalidArgument, err.Error())
	}
	start, err := domain.ParseWallTime(w.TimePrefs.Start)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("%w: time_prefs.start: %s", service.ErrInvalidArgument, err.Error())
	}
	end, err := domain.ParseWallTime(w.TimePrefs.End)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("%w: time_prefs.end: %s", service.ErrInvalidArgument, err.Error())
	}
	return domain.Goal{
		VenueQuery: vq,
		Date:       d,
		Party:      w.Party,
		TimePrefs: domain.TimeWindow{
			Start:    start,
			End:      end,
			Priority: parsePriorityWire(w.TimePrefs.Priority),
		},
		AccountID: domain.AccountID(w.AccountID),
		Constraints: domain.Constraints{
			TableTypes: w.Constraints.TableTypes,
			AnyTable:   w.Constraints.AnyTable,
		},
	}, nil
}

func parsePriorityWire(s string) domain.TimePriority {
	switch s {
	case "earlier":
		return domain.PriorityEarlier
	case "later":
		return domain.PriorityLater
	default:
		return domain.PriorityNone
	}
}

// venueWire mirrors domain.Venue. TZ is the IANA name (e.g.
// "America/New_York"); the empty string means an unresolved venue
// surface that the client should not consume.
type venueWire struct {
	Provider string `json:"provider"`
	Ref      string `json:"ref"`
	Name     string `json:"name,omitempty"`
	TZ       string `json:"tz"`
}

func encodeVenue(v domain.Venue) venueWire {
	tz := ""
	if v.TZ != nil {
		tz = v.TZ.String()
	}
	return venueWire{
		Provider: string(v.Provider),
		Ref:      v.Ref,
		Name:     v.Name,
		TZ:       tz,
	}
}

// strategyWire is the discriminated wire shape of
// domain.ReleaseStrategy. Tag ∈ {"explicit","discovered","continuous"}.
type strategyWire struct {
	Tag        string `json:"tag"`
	At         string `json:"at,omitempty"`          // explicit
	ProbeFrom  string `json:"probe_from,omitempty"`  // discovered
	ProbeUntil string `json:"probe_until,omitempty"` // discovered
	Until      string `json:"until,omitempty"`       // continuous
}

func encodeStrategy(s domain.ReleaseStrategy) strategyWire {
	switch v := s.(type) {
	case domain.ExplicitRelease:
		return strategyWire{Tag: "explicit", At: rfc3339(v.At)}
	case domain.DiscoveredRelease:
		return strategyWire{Tag: "discovered", ProbeFrom: rfc3339(v.ProbeFrom), ProbeUntil: rfc3339(v.ProbeUntil)}
	case domain.ContinuousRelease:
		return strategyWire{Tag: "continuous", Until: rfc3339(v.Until)}
	default:
		return strategyWire{Tag: "nil"}
	}
}

// planWire is the wire shape of domain.Plan.
type planWire struct {
	Goal            goalWire     `json:"goal"`
	Venue           venueWire    `json:"venue"`
	DropMoment      string       `json:"drop_moment"`
	Strategy        strategyWire `json:"strategy"`
	FireSchedule    []string     `json:"fire_schedule,omitempty"`
	SigningRequired bool         `json:"signing_required"`
	Hash            string       `json:"hash,omitempty"`
	Notes           []string     `json:"notes,omitempty"`
	PlanHashVersion int          `json:"plan_hash_version"`
	ComputedAt      string       `json:"computed_at"`
}

func encodePlan(p domain.Plan) planWire {
	fs := make([]string, 0, len(p.FireSchedule))
	for _, t := range p.FireSchedule {
		fs = append(fs, rfc3339(t))
	}
	return planWire{
		Goal:            encodeGoal(p.Goal),
		Venue:           encodeVenue(p.Venue),
		DropMoment:      rfc3339(p.DropMoment),
		Strategy:        encodeStrategy(p.Strategy),
		FireSchedule:    fs,
		SigningRequired: p.SigningRequired,
		Hash:            p.Hash,
		Notes:           append([]string(nil), p.Notes...),
		PlanHashVersion: p.PlanHashVersion,
		ComputedAt:      rfc3339(p.ComputedAt),
	}
}

// questSummaryWire is the wire shape of service.QuestSummary.
type questSummaryWire struct {
	ID          string  `json:"id"`
	UserID      string  `json:"user_id"`
	AccountID   string  `json:"account_id"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	CompletedAt *string `json:"completed_at,omitempty"`
	PlanHash    string  `json:"plan_hash,omitempty"`
}

func encodeQuestSummary(q service.QuestSummary) questSummaryWire {
	out := questSummaryWire{
		ID:        string(q.ID),
		UserID:    string(q.UserID),
		AccountID: string(q.AccountID),
		Status:    q.Status.String(),
		CreatedAt: rfc3339(q.CreatedAt),
		UpdatedAt: rfc3339(q.UpdatedAt),
		PlanHash:  q.PlanHash,
	}
	if q.CompletedAt != nil {
		s := rfc3339(*q.CompletedAt)
		out.CompletedAt = &s
	}
	return out
}

// eventWire is the wire shape of domain.Event. Attrs (a []slog.Attr)
// is flattened to a map[string]string — every slog attribute
// serializes via its Value.String() form. The map is alphabetized so
// clients see a stable order.
type eventWire struct {
	Type  string            `json:"type"`
	At    string            `json:"at"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

func encodeEvent(e domain.Event) eventWire {
	attrs := make(map[string]string, len(e.Attrs))
	for _, a := range e.Attrs {
		attrs[a.Key] = a.Value.String()
	}
	if len(attrs) == 0 {
		attrs = nil
	}
	return eventWire{
		Type:  string(e.Type),
		At:    rfc3339(e.At),
		Attrs: attrs,
	}
}

// questStateWire is the wire shape of service.QuestState. Full view:
// summary + goal + plan + bounded event history.
type questStateWire struct {
	Summary questSummaryWire `json:"summary"`
	Goal    goalWire         `json:"goal"`
	Plan    planWire         `json:"plan"`
	Events  []eventWire      `json:"events"`
}

func encodeQuestState(q service.QuestState) questStateWire {
	evs := make([]eventWire, 0, len(q.Events))
	for _, e := range q.Events {
		evs = append(evs, encodeEvent(e))
	}
	return questStateWire{
		Summary: encodeQuestSummary(q.Summary),
		Goal:    encodeGoal(q.Goal),
		Plan:    encodePlan(q.Plan),
		Events:  evs,
	}
}

// accountWire is the wire shape of service.Account.
type accountWire struct {
	ID          string  `json:"id"`
	UserID      string  `json:"user_id"`
	Email       string  `json:"email"`
	DisplayName string  `json:"display_name,omitempty"`
	CreatedAt   string  `json:"created_at"`
	DisabledAt  *string `json:"disabled_at,omitempty"`
}

func encodeAccount(a service.Account) accountWire {
	out := accountWire{
		ID:          string(a.ID),
		UserID:      string(a.UserID),
		Email:       a.Email,
		DisplayName: a.DisplayName,
		CreatedAt:   rfc3339(a.CreatedAt),
	}
	if a.DisabledAt != nil {
		s := rfc3339(*a.DisabledAt)
		out.DisabledAt = &s
	}
	return out
}

// rfc3339 is the standard timestamp encoding for the wire surface. A
// zero time renders as the empty string so clients can branch on its
// emptiness without parsing a sentinel date.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseRFC3339 is the symmetric decoder. Empty strings produce a zero
// time.Time so optional wire fields round-trip.
func parseRFC3339(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s", service.ErrInvalidArgument, err.Error())
	}
	return t, nil
}

// decodeJSON is a thin wrapper that maps a json.Decode error into a
// service.ErrInvalidArgument-wrapped error so handlers return 422 for
// bad payloads (matching the §4.3 error contract).
func decodeJSON(body []byte, out any) error {
	if len(body) == 0 {
		return fmt.Errorf("%w: request body is empty", service.ErrInvalidArgument)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %s", service.ErrInvalidArgument, err.Error())
	}
	return nil
}

// ensureNonEmpty is a small helper for required path parameters.
func ensureNonEmpty(name, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s is required", service.ErrInvalidArgument, name)
	}
	return nil
}

// parseStatusList decodes a comma-separated status query string ("submitted,booking")
// into a []domain.Status. Empty input produces a nil slice (no filter).
// Unknown values surface as ErrInvalidArgument so a client typo doesn't
// silently match no quests.
func parseStatusList(s string) ([]domain.Status, error) {
	if s == "" {
		return nil, nil
	}
	out := []domain.Status{}
	for _, raw := range splitCSV(s) {
		st, ok := statusFromString(raw)
		if !ok {
			return nil, fmt.Errorf("%w: unknown status %q", service.ErrInvalidArgument, raw)
		}
		out = append(out, st)
	}
	return out, nil
}

func splitCSV(s string) []string {
	out := []string{}
	start := 0
	for i := range len(s) {
		if s[i] == ',' {
			if start < i {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func statusFromString(s string) (domain.Status, bool) {
	for _, st := range domain.AllStatuses() {
		if st.String() == s {
			return st, true
		}
	}
	return 0, false
}
