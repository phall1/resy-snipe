package domain

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// VenueQuery is a sealed union describing the three ways a user can
// point at a venue: a Resy URL pasted from a browser, a (slug, city)
// pair learned from a prior session, or a freeform name handed to the
// fuzzy resolver. The Resolver consumes a VenueQuery and produces a
// resolved Venue (see ADR-0002).
//
// The sealed-interface idiom mirrors ReleaseStrategy: callers cannot
// introduce new variants without editing this package, so every type
// switch in the resolver stays exhaustive.
type VenueQuery interface {
	isVenueQuery()
	// String returns a stable single-line form for log and CLI output.
	String() string
}

// VenueQueryURL captures the canonical Resy URL the user pasted. The
// resolver re-parses the URL itself; the planner stores the original
// string so the plan hash is reproducible.
type VenueQueryURL struct {
	URL string
}

// String returns "url=<URL>".
func (v VenueQueryURL) String() string { return "url=" + v.URL }

func (VenueQueryURL) isVenueQuery() {}

// VenueQuerySlug is the (slug, city) pair the Resolver caches once it
// has parsed a URL or talked to /3/venue. It is the most stable form;
// MCP agents typically pass this after a prior planning round-trip.
type VenueQuerySlug struct {
	Slug string
	City string
}

// String returns "slug=<slug> city=<city>".
func (v VenueQuerySlug) String() string { return "slug=" + v.Slug + " city=" + v.City }

func (VenueQuerySlug) isVenueQuery() {}

// VenueQueryName is the freeform "Carbone NYC" form the user might
// type into a chat surface. The Resolver runs a fuzzy lookup and may
// surface multiple candidates for disambiguation.
type VenueQueryName struct {
	Name string
}

// String returns "name=<Name>".
func (v VenueQueryName) String() string { return "name=" + v.Name }

func (VenueQueryName) isVenueQuery() {}

// TimePriority orders the expansion of a TimeWindow during slot
// planning. PriorityNone leaves the order undefined and lets the
// planner pick a default (center-out).
type TimePriority int

const (
	// PriorityNone defers to the planner default.
	PriorityNone TimePriority = iota
	// PriorityEarlier expands earliest times first.
	PriorityEarlier
	// PriorityLater expands latest times first.
	PriorityLater
)

// String returns the lowercase tag used in plan canonicalization.
func (p TimePriority) String() string {
	switch p {
	case PriorityEarlier:
		return "earlier"
	case PriorityLater:
		return "later"
	default:
		return "none"
	}
}

// TimeWindow is the contiguous range of times the user will accept for
// a reservation, together with an ordering hint. The Planner expands
// this into the concrete SlotPreference slice the engine walks.
type TimeWindow struct {
	Start    WallTime     `json:"start"`
	End      WallTime     `json:"end"`
	Priority TimePriority `json:"priority"`
}

// IsZero reports whether w is the zero value.
func (w TimeWindow) IsZero() bool { return w == TimeWindow{} }

// Constraints is intentionally minimal in v1. The planner consumes
// these alongside TimeWindow when expanding slot preferences.
type Constraints struct {
	// TableTypes is the ordered table-type preference list. Empty means
	// any table type is acceptable.
	TableTypes []string `json:"table_types,omitempty"`
	// AnyTable, when true, forces the planner to ignore TableTypes and
	// emit a single "any" preference. It exists so a user can opt out
	// of table-type filtering even after seeding a preferred list.
	AnyTable bool `json:"any_table,omitempty"`
}

// Goal is what the user wants. Per ADR-0001 it is the persisted
// surface that survives replans; the Planner derives a fresh Intent
// from the same Goal whenever inputs change.
type Goal struct {
	VenueQuery  VenueQuery  `json:"venue_query"`
	Date        Date        `json:"date"`
	Party       int         `json:"party"`
	TimePrefs   TimeWindow  `json:"time_prefs"`
	AccountID   AccountID   `json:"account_id"`
	Constraints Constraints `json:"constraints"`
}

// Sentinel validation errors for Goal. The Service layer relies on
// errors.Is to branch; never replace with a bare fmt.Errorf.
var (
	ErrGoalDateInPast         = errors.New("goal: date is in the past")
	ErrGoalDateZero           = errors.New("goal: date is required")
	ErrGoalPartyNonPositive   = errors.New("goal: party must be positive")
	ErrGoalTimeWindowEmpty    = errors.New("goal: time window is required")
	ErrGoalTimeWindowInverted = errors.New("goal: time window start is after end")
	ErrGoalVenueQueryMissing  = errors.New("goal: venue query is required")
	ErrGoalVenueQueryHost     = errors.New("goal: venue URL host is not a Resy domain")
	ErrGoalAccountMissing     = errors.New("goal: account id is required")
)

// resyHosts is the closed allow-list of hosts a VenueQueryURL may
// carry. The list is small on purpose; an unknown host is a strong
// signal the user pasted the wrong thing.
var resyHosts = map[string]struct{}{
	"resy.com":     {},
	"www.resy.com": {},
}

// Validate enforces the invariants the planner expects. The caller
// supplies "now" so domain remains time-source-agnostic (per laws.md
// rule 7); production code passes clock.Clock.Now(), tests pass a
// fixed instant.
func (g Goal) Validate(now time.Time) error {
	if g.VenueQuery == nil {
		return ErrGoalVenueQueryMissing
	}
	if err := validateVenueQuery(g.VenueQuery); err != nil {
		return err
	}
	if g.AccountID == "" {
		return ErrGoalAccountMissing
	}
	if g.Date.IsZero() {
		return ErrGoalDateZero
	}
	// Compare in UTC at start-of-day: a Date is zoneless by definition,
	// so we treat "today or later" using UTC as a sentinel; the planner
	// re-checks against the venue's TZ once a Venue is resolved.
	if g.Date.In(time.UTC).Before(todayStart(now)) {
		return ErrGoalDateInPast
	}
	if g.Party <= 0 {
		return ErrGoalPartyNonPositive
	}
	if g.TimePrefs.IsZero() {
		return ErrGoalTimeWindowEmpty
	}
	if wallTimeAfter(g.TimePrefs.Start, g.TimePrefs.End) {
		return ErrGoalTimeWindowInverted
	}
	return nil
}

// String returns a multi-line, indented form suitable for CLI dry-run
// output. The shape mirrors how the CLI prints an Intent today: the
// goal type on the first line, fields indented with two spaces.
func (g Goal) String() string {
	var b strings.Builder
	b.WriteString("Goal{\n")
	if g.VenueQuery != nil {
		fmt.Fprintf(&b, "  venue:   %s\n", g.VenueQuery.String())
	} else {
		b.WriteString("  venue:   <nil>\n")
	}
	fmt.Fprintf(&b, "  date:    %s\n", g.Date)
	fmt.Fprintf(&b, "  party:   %d\n", g.Party)
	fmt.Fprintf(&b, "  window:  %s..%s priority=%s\n",
		g.TimePrefs.Start, g.TimePrefs.End, g.TimePrefs.Priority)
	fmt.Fprintf(&b, "  account: %s\n", g.AccountID)
	if len(g.Constraints.TableTypes) > 0 {
		fmt.Fprintf(&b, "  tables:  %s\n", strings.Join(g.Constraints.TableTypes, ","))
	}
	if g.Constraints.AnyTable {
		b.WriteString("  tables:  <any>\n")
	}
	b.WriteString("}")
	return b.String()
}

func validateVenueQuery(q VenueQuery) error {
	switch v := q.(type) {
	case VenueQueryURL:
		if strings.TrimSpace(v.URL) == "" {
			return ErrGoalVenueQueryMissing
		}
		u, err := url.Parse(v.URL)
		if err != nil {
			return fmt.Errorf("goal: invalid venue url: %w", err)
		}
		host := strings.ToLower(u.Hostname())
		if _, ok := resyHosts[host]; !ok {
			return ErrGoalVenueQueryHost
		}
		return nil
	case VenueQuerySlug:
		if strings.TrimSpace(v.Slug) == "" || strings.TrimSpace(v.City) == "" {
			return ErrGoalVenueQueryMissing
		}
		return nil
	case VenueQueryName:
		if strings.TrimSpace(v.Name) == "" {
			return ErrGoalVenueQueryMissing
		}
		return nil
	default:
		return ErrGoalVenueQueryMissing
	}
}

func todayStart(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

func wallTimeAfter(a, b WallTime) bool {
	if a.Hour != b.Hour {
		return a.Hour > b.Hour
	}
	if a.Minute != b.Minute {
		return a.Minute > b.Minute
	}
	return a.Second > b.Second
}
