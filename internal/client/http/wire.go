package httpclient

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// wire.go is the client's copy of the daemon's HTTP wire codec. The
// server's equivalent lives in internal/transport/http/wire.go and uses
// unexported types; duplication is intentional so the client does not
// import the server package (which would pull the daemon's full
// dependency graph — sqlite, store, engine — into a CLI binary that
// only needs to speak HTTP). The two codecs must stay byte-compatible:
// any change to a JSON tag, discriminator value, or RFC3339 convention
// MUST land on both sides in the same commit.

// venueQueryWire is the discriminated wire shape for domain.VenueQuery.
// Kind ∈ {"url","slug","name"}; other fields are populated per variant.
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

// timeWindowWire mirrors domain.TimeWindow. Start/End use HH:MM(:SS);
// priority is the lowercase tag (PriorityNone elides to "none").
type timeWindowWire struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Priority string `json:"priority,omitempty"`
}

// constraintsWire mirrors domain.Constraints. Empty TableTypes is
// omitted to keep the payload compact.
type constraintsWire struct {
	TableTypes []string `json:"table_types,omitempty"`
	AnyTable   bool     `json:"any_table,omitempty"`
}

// goalWire is the wire shape of domain.Goal. Date is YYYY-MM-DD;
// AccountID is the v2 ULID id (not an email).
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
	var d domain.Date
	if w.Date != "" {
		dd, err := domain.ParseDate(w.Date)
		if err != nil {
			return domain.Goal{}, fmt.Errorf("decode goal: date: %w", err)
		}
		d = dd
	}
	start, err := decodeWallTime(w.TimePrefs.Start)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("decode goal: time_prefs.start: %w", err)
	}
	end, err := decodeWallTime(w.TimePrefs.End)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("decode goal: time_prefs.end: %w", err)
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

func decodeVenueQuery(w venueQueryWire) (domain.VenueQuery, error) {
	// An empty discriminator is permitted when decoding a Plan that
	// has a zero VenueQuery (the server always emits a Kind, but the
	// server's "nil" sentinel maps to a nil interface). We surface
	// the nil through the return value; callers that need a populated
	// query will check for it themselves.
	switch w.Kind {
	case "url":
		return domain.VenueQueryURL{URL: w.URL}, nil
	case "slug":
		return domain.VenueQuerySlug{Slug: w.Slug, City: w.City}, nil
	case "name":
		return domain.VenueQueryName{Name: w.Name}, nil
	case "", "nil":
		return nil, nil //nolint:nilnil // a missing VenueQuery is a legal zero value, not an error
	default:
		return nil, fmt.Errorf("decode venue_query: unknown kind %q", w.Kind)
	}
}

func decodeWallTime(s string) (domain.WallTime, error) {
	if s == "" {
		return domain.WallTime{}, nil
	}
	return domain.ParseWallTime(s)
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

// venueWire mirrors domain.Venue. TZ is the IANA name; the empty
// string means an unresolved venue surface.
type venueWire struct {
	Provider string `json:"provider"`
	Ref      string `json:"ref"`
	Name     string `json:"name,omitempty"`
	TZ       string `json:"tz"`
}

func decodeVenue(w venueWire) (domain.Venue, error) {
	var tz *time.Location
	if w.TZ != "" {
		loc, err := time.LoadLocation(w.TZ)
		if err != nil {
			return domain.Venue{}, fmt.Errorf("decode venue: tz %q: %w", w.TZ, err)
		}
		tz = loc
	}
	return domain.Venue{
		Provider: domain.ProviderID(w.Provider),
		Ref:      w.Ref,
		Name:     w.Name,
		TZ:       tz,
	}, nil
}

// strategyWire is the discriminated wire shape of
// domain.ReleaseStrategy. Tag ∈ {"explicit","discovered","continuous"}.
type strategyWire struct {
	Tag        string `json:"tag"`
	At         string `json:"at,omitempty"`
	ProbeFrom  string `json:"probe_from,omitempty"`
	ProbeUntil string `json:"probe_until,omitempty"`
	Until      string `json:"until,omitempty"`
}

func decodeStrategy(w strategyWire) (domain.ReleaseStrategy, error) {
	switch w.Tag {
	case "explicit":
		at, err := parseRFC3339(w.At)
		if err != nil {
			return nil, fmt.Errorf("decode strategy.explicit.at: %w", err)
		}
		return domain.ExplicitRelease{At: at}, nil
	case "discovered":
		from, err := parseRFC3339(w.ProbeFrom)
		if err != nil {
			return nil, fmt.Errorf("decode strategy.discovered.probe_from: %w", err)
		}
		until, err := parseRFC3339(w.ProbeUntil)
		if err != nil {
			return nil, fmt.Errorf("decode strategy.discovered.probe_until: %w", err)
		}
		return domain.DiscoveredRelease{ProbeFrom: from, ProbeUntil: until}, nil
	case "continuous":
		until, err := parseRFC3339(w.Until)
		if err != nil {
			return nil, fmt.Errorf("decode strategy.continuous.until: %w", err)
		}
		return domain.ContinuousRelease{Until: until}, nil
	case "", "nil":
		return nil, nil //nolint:nilnil // a missing ReleaseStrategy is a legal zero value, not an error
	default:
		return nil, fmt.Errorf("decode strategy: unknown tag %q", w.Tag)
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

func decodePlan(w planWire) (domain.Plan, error) {
	goal, err := decodeGoal(w.Goal)
	if err != nil {
		return domain.Plan{}, fmt.Errorf("decode plan.goal: %w", err)
	}
	venue, err := decodeVenue(w.Venue)
	if err != nil {
		return domain.Plan{}, fmt.Errorf("decode plan.venue: %w", err)
	}
	drop, err := parseRFC3339(w.DropMoment)
	if err != nil {
		return domain.Plan{}, fmt.Errorf("decode plan.drop_moment: %w", err)
	}
	strategy, err := decodeStrategy(w.Strategy)
	if err != nil {
		return domain.Plan{}, fmt.Errorf("decode plan.strategy: %w", err)
	}
	fs := make([]time.Time, 0, len(w.FireSchedule))
	for i, s := range w.FireSchedule {
		t, err := parseRFC3339(s)
		if err != nil {
			return domain.Plan{}, fmt.Errorf("decode plan.fire_schedule[%d]: %w", i, err)
		}
		fs = append(fs, t)
	}
	computed, err := parseRFC3339(w.ComputedAt)
	if err != nil {
		return domain.Plan{}, fmt.Errorf("decode plan.computed_at: %w", err)
	}
	return domain.Plan{
		Goal:            goal,
		Venue:           venue,
		DropMoment:      drop,
		Strategy:        strategy,
		FireSchedule:    fs,
		SigningRequired: w.SigningRequired,
		Hash:            w.Hash,
		Notes:           append([]string(nil), w.Notes...),
		PlanHashVersion: w.PlanHashVersion,
		ComputedAt:      computed,
	}, nil
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

func decodeQuestSummary(w questSummaryWire) (service.QuestSummary, error) {
	status, ok := statusFromString(w.Status)
	if !ok {
		return service.QuestSummary{}, fmt.Errorf("decode quest_summary: unknown status %q", w.Status)
	}
	created, err := parseRFC3339(w.CreatedAt)
	if err != nil {
		return service.QuestSummary{}, fmt.Errorf("decode quest_summary.created_at: %w", err)
	}
	updated, err := parseRFC3339(w.UpdatedAt)
	if err != nil {
		return service.QuestSummary{}, fmt.Errorf("decode quest_summary.updated_at: %w", err)
	}
	out := service.QuestSummary{
		ID:        domain.QuestID(w.ID),
		UserID:    domain.UserID(w.UserID),
		AccountID: domain.AccountID(w.AccountID),
		Status:    status,
		CreatedAt: created,
		UpdatedAt: updated,
		PlanHash:  w.PlanHash,
	}
	if w.CompletedAt != nil {
		t, err := parseRFC3339(*w.CompletedAt)
		if err != nil {
			return service.QuestSummary{}, fmt.Errorf("decode quest_summary.completed_at: %w", err)
		}
		out.CompletedAt = &t
	}
	return out, nil
}

// eventWire is the wire shape of domain.Event. Attrs is the flattened
// map[string]string the server produces from []slog.Attr — the client
// reconstructs slog.Attrs as string values (we lose the original Value
// kind, which is acceptable for callback consumers that only render
// the attributes).
type eventWire struct {
	Type  string            `json:"type"`
	At    string            `json:"at"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

func decodeEvent(w eventWire) (domain.Event, error) {
	at, err := parseRFC3339(w.At)
	if err != nil {
		return domain.Event{}, fmt.Errorf("decode event.at: %w", err)
	}
	attrs := make([]slog.Attr, 0, len(w.Attrs))
	for k, v := range w.Attrs {
		attrs = append(attrs, slog.String(k, v))
	}
	return domain.Event{
		Type:  domain.EventType(w.Type),
		At:    at,
		Attrs: attrs,
	}, nil
}

// questStateWire is the wire shape of service.QuestState.
type questStateWire struct {
	Summary questSummaryWire `json:"summary"`
	Goal    goalWire         `json:"goal"`
	Plan    planWire         `json:"plan"`
	Events  []eventWire      `json:"events"`
}

func decodeQuestState(w questStateWire) (service.QuestState, error) {
	summary, err := decodeQuestSummary(w.Summary)
	if err != nil {
		return service.QuestState{}, err
	}
	goal, err := decodeGoal(w.Goal)
	if err != nil {
		return service.QuestState{}, fmt.Errorf("decode quest_state.goal: %w", err)
	}
	plan, err := decodePlan(w.Plan)
	if err != nil {
		return service.QuestState{}, fmt.Errorf("decode quest_state.plan: %w", err)
	}
	events := make([]domain.Event, 0, len(w.Events))
	for i, e := range w.Events {
		ev, err := decodeEvent(e)
		if err != nil {
			return service.QuestState{}, fmt.Errorf("decode quest_state.events[%d]: %w", i, err)
		}
		events = append(events, ev)
	}
	return service.QuestState{
		Summary: summary,
		Goal:    goal,
		Plan:    plan,
		Events:  events,
	}, nil
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

func decodeAccount(w accountWire) (service.Account, error) {
	created, err := parseRFC3339(w.CreatedAt)
	if err != nil {
		return service.Account{}, fmt.Errorf("decode account.created_at: %w", err)
	}
	out := service.Account{
		ID:          domain.AccountID(w.ID),
		UserID:      domain.UserID(w.UserID),
		Email:       w.Email,
		DisplayName: w.DisplayName,
		CreatedAt:   created,
	}
	if w.DisabledAt != nil {
		t, err := parseRFC3339(*w.DisabledAt)
		if err != nil {
			return service.Account{}, fmt.Errorf("decode account.disabled_at: %w", err)
		}
		out.DisabledAt = &t
	}
	return out, nil
}

// tokenResponseWire mirrors the server's tokenResponse listing shape.
// Plaintext bearer is intentionally absent — only the issue endpoint
// returns it.
type tokenResponseWire struct {
	ID        string  `json:"id"`
	Scope     string  `json:"scope"`
	Label     string  `json:"label"`
	CreatedAt string  `json:"created_at"`
	LastSeen  *string `json:"last_seen,omitempty"`
	RevokedAt *string `json:"revoked_at,omitempty"`
}

func decodeToken(w tokenResponseWire) (service.Token, error) {
	created, err := parseRFC3339(w.CreatedAt)
	if err != nil {
		return service.Token{}, fmt.Errorf("decode token.created_at: %w", err)
	}
	out := service.Token{
		ID:        w.ID,
		Scope:     w.Scope,
		Label:     w.Label,
		CreatedAt: created,
	}
	if w.LastSeen != nil {
		t, err := parseRFC3339(*w.LastSeen)
		if err != nil {
			return service.Token{}, fmt.Errorf("decode token.last_seen: %w", err)
		}
		out.LastSeen = &t
	}
	if w.RevokedAt != nil {
		t, err := parseRFC3339(*w.RevokedAt)
		if err != nil {
			return service.Token{}, fmt.Errorf("decode token.revoked_at: %w", err)
		}
		out.RevokedAt = &t
	}
	return out, nil
}

// parseRFC3339 is the symmetric decoder for the server's rfc3339
// encoder. An empty string produces a zero time so optional wire
// fields round-trip.
func parseRFC3339(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

func statusFromString(s string) (domain.Status, bool) {
	for _, st := range domain.AllStatuses() {
		if st.String() == s {
			return st, true
		}
	}
	return 0, false
}

// marshalJSON is a thin wrapper so call sites can compose request
// bodies without sprinkling json.Marshal at every verb.
func marshalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}
