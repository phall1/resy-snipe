// Package main: `resy-snipe quest plan …` subcommand.
//
// Wired into main.go's subcommand dispatch by adding the following
// branch alongside the existing `venue resolve` / `user` / `login`
// branches (the harness performs the edit at wave end; the exact line
// is):
//
//	if len(args) > 1 && args[0] == "quest" && args[1] == "plan" {
//	    return runQuestPlanCmd(context.Background(), args[2:], stdin, logOut, clk)
//	}
//
// Until that line lands the entry point below is reachable only via
// the cmd/resy-snipe test suite (which calls runQuestPlanCmd directly).
//
// Surface:
//
//	resy-snipe quest plan <url|slug+city|name>
//	    [-date YYYY-MM-DD] [-party N] -time HH:MM..HH:MM
//	    [-priority earlier|later|none] [-tables type1,type2,...]
//	    -account <email> -user <user_id>
//	    [-format=table|json]
//
// Behavior: parse + validate flags, build a domain.Goal, call
// service.Service.PlanQuest, and pretty-print the resulting Plan. No
// side effects — the Plan is the user-approvable artifact described in
// ADR-0012. The user can copy the hash and feed it back into a
// follow-up `quest create -plan-hash=<hash>` to commit it.
//
// Per Law 4 this file is CLI wiring + output formatting only. Goal
// validation, venue resolution, calendar snapshot, and strategy
// selection all live inside internal/service + internal/planner.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/planner"
	"resy-snipe/internal/resolver"
	"resy-snipe/internal/service"
)

// questPlanFormat selects the output rendering for the printed Plan.
type questPlanFormat string

const (
	questPlanFormatTable questPlanFormat = "table"
	questPlanFormatJSON  questPlanFormat = "json"
)

// questPlanOpts is the parsed shape of `quest plan` flags. The
// positional venue argument flows in via venueQuery as a free-form
// string and is resolved against buildVenueQuery in resolve.go (the
// helper is exported within the package and is the single source of
// truth for "URL/slug+city/name → VenueQuery").
type questPlanOpts struct {
	venueQuery string
	date       string // YYYY-MM-DD; empty means "fall back to URL hint".
	party      int    // 0 means "fall back to URL hint".
	timeWindow string // "HH:MM..HH:MM"; required.
	priority   string // earlier|later|none.
	tables     string // comma-separated; empty → AnyTable=true.
	account    string // Resy account email.
	user       string // operator UserID (until auth lands).
	format     questPlanFormat
}

// questPlanService is the slim subset of service.Service that
// runQuestPlanCmd needs. Narrowing the seam keeps the test double
// trivial (the Standard service exposes ~15 methods; we only call
// PlanQuest here).
type questPlanService interface {
	PlanQuest(ctx context.Context, userID domain.UserID, goal domain.Goal) (domain.Plan, error)
}

// openQuestServiceFn is the test seam mirroring openResolveBackendFn:
// production points it at newStandardService (wrapped to satisfy the
// narrowed interface); quest_plan_test.go swaps in an in-memory fake
// so the CLI surface can be exercised without bootstrapping a SQLite
// store or a real *resy.Client.
//
// The cleanup closes whatever resources the production wiring opened
// (sql.DB handle, etc). Tests return a no-op cleanup.
var openQuestServiceFn = func(
	ctx context.Context,
	logger *slog.Logger,
	clk clock.Clock,
) (questPlanService, func() error, error) {
	svc, cleanup, err := newStandardService(ctx, logger, clk)
	if err != nil {
		return nil, nil, err
	}
	return svc, cleanup, nil
}

// runQuestPlanCmd is the entry point for `quest plan`. See the file
// header for the wiring line main.go must add to dispatch into it.
//
// The supplied out is where ALL user-visible output lands (the Plan
// render AND error messages). Returning a non-nil error from the
// entry point is how main signals a non-zero exit code.
func runQuestPlanCmd(
	ctx context.Context,
	args []string,
	_ io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	opts, err := parseQuestPlanFlags(args, out)
	if err != nil {
		return err
	}

	// Build the VenueQuery + parse URL hints in one pass so a URL with
	// ?date=/?seats= can transparently fill in unspecified flags.
	query, hints, err := buildVenueQueryWithHints(opts.venueQuery)
	if err != nil {
		return fmt.Errorf("quest plan: %w", err)
	}

	goal, err := buildGoalFromOpts(opts, query, hints)
	if err != nil {
		// All flag-shaped errors land here; surface them before any
		// service bootstrap so we don't open the DB on a malformed call.
		return fmt.Errorf("quest plan: %w", err)
	}

	logger := newCLILogger(out, slog.LevelInfo)
	svc, cleanup, err := openQuestServiceFn(ctx, logger, clk)
	if err != nil {
		return fmt.Errorf("quest plan bootstrap: %w", err)
	}
	defer func() {
		if cleanup != nil {
			_ = cleanup()
		}
	}()

	plan, planErr := svc.PlanQuest(ctx, domain.UserID(opts.user), goal)
	if planErr != nil {
		fprintf(out, "Plan failed: %v\n", planErr)
		return planErr
	}

	switch opts.format {
	case questPlanFormatJSON:
		return renderPlanJSON(out, plan)
	default:
		return renderPlanTable(out, plan, clk.Now())
	}
}

// parseQuestPlanFlags pulls every flag off args and returns the
// positional venue query as opts.venueQuery. ContinueOnError means a
// malformed flag surfaces as the returned error rather than calling
// os.Exit; usage is written to out so test harnesses can assert on it.
func parseQuestPlanFlags(args []string, out io.Writer) (questPlanOpts, error) {
	fs := flag.NewFlagSet("quest plan", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out,
			"Usage: resy-snipe quest plan <url|slug+city|name> "+
				"[-date YYYY-MM-DD] [-party N] -time HH:MM..HH:MM "+
				"[-priority earlier|later|none] [-tables type1,type2,...] "+
				"-account <email> -user <user_id> [-format=table|json]")
		fs.PrintDefaults()
	}

	date := fs.String("date", "",
		"target reservation date (YYYY-MM-DD); falls back to a ?date= URL hint")
	party := fs.Int("party", 0,
		"party size; falls back to a ?seats= URL hint")
	timeWindow := fs.String("time", "",
		"time window HH:MM..HH:MM (required)")
	priority := fs.String("priority", "earlier",
		"time priority: earlier|later|none")
	tables := fs.String("tables", "",
		"comma-separated table-type preference list (empty = any table)")
	account := fs.String("account", "",
		"Resy account email to plan against (required)")
	user := fs.String("user", "",
		"operator user_id (required until auth context lands)")
	format := fs.String("format", string(questPlanFormatTable),
		"output format: table or json")

	if err := fs.Parse(args); err != nil {
		return questPlanOpts{}, fmt.Errorf("parse flags: %w", err)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return questPlanOpts{}, errors.New("missing venue argument")
	}
	// Join trailing positional args with a space so `quest plan carbone nyc`
	// works without quoting — buildVenueQuery handles the parse.
	venueQuery := strings.TrimSpace(strings.Join(rest, " "))
	if venueQuery == "" {
		return questPlanOpts{}, errors.New("empty venue argument")
	}

	f := questPlanFormat(strings.ToLower(strings.TrimSpace(*format)))
	switch f {
	case questPlanFormatTable, questPlanFormatJSON:
	default:
		return questPlanOpts{}, fmt.Errorf("invalid -format %q (want table|json)", *format)
	}

	return questPlanOpts{
		venueQuery: venueQuery,
		date:       strings.TrimSpace(*date),
		party:      *party,
		timeWindow: strings.TrimSpace(*timeWindow),
		priority:   strings.ToLower(strings.TrimSpace(*priority)),
		tables:     strings.TrimSpace(*tables),
		account:    strings.TrimSpace(*account),
		user:       strings.TrimSpace(*user),
		format:     f,
	}, nil
}

// buildVenueQueryWithHints is buildVenueQuery + the URL hints the
// resolver extracts. For non-URL inputs the hints slot is nil so the
// caller falls back to the explicit flags. Keeping this distinct from
// resolve.go's buildVenueQuery avoids forcing resolve to carry the
// extra return value (it does not use hints).
func buildVenueQueryWithHints(raw string) (domain.VenueQuery, *resolver.URLHints, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil, errors.New("query is empty")
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		q, hints, err := resolver.ParseResyURL(trimmed)
		if err != nil {
			return nil, nil, err
		}
		return q, hints, nil
	}
	// Non-URL paths share resolve.go's classifier verbatim.
	q, err := buildVenueQuery(trimmed)
	if err != nil {
		return nil, nil, err
	}
	return q, nil, nil
}

// buildGoalFromOpts assembles a domain.Goal from the parsed flags,
// applying URL hints (?date=, ?seats=) when the corresponding flag was
// not explicitly supplied. Every flag-shaped error is surfaced here so
// the caller can refuse the request before opening a DB connection.
func buildGoalFromOpts(opts questPlanOpts, query domain.VenueQuery, hints *resolver.URLHints) (domain.Goal, error) {
	if opts.account == "" {
		return domain.Goal{}, errors.New("-account is required")
	}
	if opts.user == "" {
		return domain.Goal{}, errors.New("-user is required")
	}
	if opts.timeWindow == "" {
		return domain.Goal{}, errors.New("-time HH:MM..HH:MM is required")
	}

	// Date: flag wins; otherwise consume the URL hint; otherwise error.
	var date domain.Date
	switch {
	case opts.date != "":
		d, err := domain.ParseDate(opts.date)
		if err != nil {
			return domain.Goal{}, fmt.Errorf("-date: %w", err)
		}
		date = d
	case hints != nil && hints.Date != nil:
		date = *hints.Date
	default:
		return domain.Goal{}, errors.New("-date is required when the URL has no ?date= hint")
	}

	// Party: flag wins (non-zero); otherwise consume the URL hint;
	// otherwise error. A flag value of 0 is treated as "unset" so the
	// hint can supply it.
	var party int
	switch {
	case opts.party > 0:
		party = opts.party
	case hints != nil && hints.Party != nil:
		party = *hints.Party
	default:
		return domain.Goal{}, errors.New("-party is required when the URL has no ?seats= hint")
	}

	window, err := parseTimeWindowFlag(opts.timeWindow, opts.priority)
	if err != nil {
		return domain.Goal{}, err
	}

	constraints := buildConstraintsFromFlag(opts.tables)

	return domain.Goal{
		VenueQuery:  query,
		Date:        date,
		Party:       party,
		TimePrefs:   window,
		AccountID:   domain.AccountID(opts.account),
		Constraints: constraints,
	}, nil
}

// parseTimeWindowFlag accepts "HH:MM..HH:MM" (or "HH:MM:SS..HH:MM:SS")
// and a priority tag. The ".." separator is the canonical CLI form
// matching the design doc's example surface.
func parseTimeWindowFlag(raw, priority string) (domain.TimeWindow, error) {
	parts := strings.Split(raw, "..")
	if len(parts) != 2 {
		return domain.TimeWindow{}, fmt.Errorf("-time %q: expected HH:MM..HH:MM", raw)
	}
	start, err := domain.ParseWallTime(strings.TrimSpace(parts[0]))
	if err != nil {
		return domain.TimeWindow{}, fmt.Errorf("-time start: %w", err)
	}
	end, err := domain.ParseWallTime(strings.TrimSpace(parts[1]))
	if err != nil {
		return domain.TimeWindow{}, fmt.Errorf("-time end: %w", err)
	}
	prio, err := parsePriorityFlag(priority)
	if err != nil {
		return domain.TimeWindow{}, err
	}
	return domain.TimeWindow{Start: start, End: end, Priority: prio}, nil
}

// parsePriorityFlag maps the CLI tag to domain.TimePriority. The
// default ("earlier") matches the design's surfaced default.
func parsePriorityFlag(s string) (domain.TimePriority, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "earlier":
		return domain.PriorityEarlier, nil
	case "later":
		return domain.PriorityLater, nil
	case "none":
		return domain.PriorityNone, nil
	default:
		return 0, fmt.Errorf("-priority %q: want earlier|later|none", s)
	}
}

// buildConstraintsFromFlag turns the comma-separated -tables flag
// into a domain.Constraints. Empty input means AnyTable=true.
func buildConstraintsFromFlag(raw string) domain.Constraints {
	if raw == "" {
		return domain.Constraints{AnyTable: true}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return domain.Constraints{AnyTable: true}
	}
	return domain.Constraints{TableTypes: out}
}

// renderPlanTable writes the human-readable Plan summary. The layout
// matches docs/v2/milestones/m1-goal-driven.md §what-ships
// (PLAN block with two-space-indented label:value rows). The hash is
// shortened for human consumption; the full hash is in the JSON form.
//
// now is supplied by the caller (clock.Clock.Now()) so days-from-now
// arithmetic stays deterministic in tests.
func renderPlanTable(out io.Writer, p domain.Plan, now time.Time) error {
	fprintln(out, "PLAN")
	fprintf(out, "  venue:        %s (%s:%s)\n",
		nonEmpty(p.Venue.Name, "(unknown)"), p.Venue.Provider, p.Venue.Ref)
	fprintf(out, "  date:         %s (%s)\n",
		p.Goal.Date, p.Goal.Date.In(planTZ(p.Venue)).Format("Mon"))
	fprintf(out, "  party:        %d\n", p.Goal.Party)
	fprintf(out, "  time prefs:   %s\n", formatTimePrefs(p.Goal))
	fprintf(out, "  drop moment:  %s\n", formatDropMoment(p.DropMoment, planTZ(p.Venue), now))
	fprintf(out, "  strategy:     %s\n", formatStrategy(p.Strategy))
	fprintf(out, "  signing:      %s\n", formatSigning(p.SigningRequired))
	fprintf(out, "  fire windows: %d\n", len(p.FireSchedule))
	fprintf(out, "  hash:         %s\n", shortHash(p.Hash))
	if len(p.Notes) == 0 {
		fprintln(out, "  notes:        —")
		return nil
	}
	for i, n := range p.Notes {
		label := "  notes:"
		if i > 0 {
			label = "       "
		}
		fprintf(out, "%s        - %s\n", label, n)
	}
	return nil
}

// renderPlanJSON writes the Plan as indented JSON. Plan has its own
// canonical JSON tags so encoding/json round-trips the full structure
// (including the FULL hash, unlike the table form's shortened view).
func renderPlanJSON(out io.Writer, p domain.Plan) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		return fmt.Errorf("render json: %w", err)
	}
	return nil
}

// PrintPlanTable is the package-exported wrapper around the unexported
// renderer so adjacent commands (eg. M1-19 `quest create`'s confirm
// prompt) can reuse the same layout. now is required so the
// days-from-now arithmetic stays deterministic.
func PrintPlanTable(out io.Writer, p domain.Plan, now time.Time) error {
	return renderPlanTable(out, p, now)
}

// formatTimePrefs renders the expanded slot times the planner would
// walk, in the same 30-minute grid the engine consumes (planner's
// ExpandSlots). Showing the expansion (not just the bounds) lets the
// user confirm the candidate set before committing the quest.
//
// A planner-internal error (an inverted window slipped past
// Goal.Validate) degrades to the raw bounds — the Plan is still
// printable even if expansion is unhappy.
func formatTimePrefs(g domain.Goal) string {
	slots, err := planner.ExpandSlots(g.TimePrefs, domain.Constraints{AnyTable: true})
	if err != nil || len(slots) == 0 {
		return fmt.Sprintf("%s..%s (%s)",
			shortWallTime(g.TimePrefs.Start),
			shortWallTime(g.TimePrefs.End),
			g.TimePrefs.Priority)
	}
	parts := make([]string, 0, len(slots))
	for _, s := range slots {
		parts = append(parts, shortWallTime(s.Time))
	}
	return strings.Join(parts, ", ")
}

// formatDropMoment renders the planner's DropMoment in the venue's
// local zone plus a "(N days from now)" hint. Falling back to UTC
// when the venue has no TZ keeps the renderer total — a degenerate
// Venue (no TZ) shouldn't crash the printer.
func formatDropMoment(t time.Time, tz *time.Location, now time.Time) string {
	if t.IsZero() {
		return "(unset)"
	}
	if tz == nil {
		tz = time.UTC
	}
	local := t.In(tz)
	delta := local.Sub(now).Truncate(24 * time.Hour)
	days := int(delta.Hours() / 24)
	rel := "today"
	switch {
	case days > 0:
		rel = fmt.Sprintf("%d days from now", days)
	case days < 0:
		rel = fmt.Sprintf("%d days ago", -days)
	}
	return fmt.Sprintf("%s (%s)", local.Format("2006-01-02T15:04 MST"), rel)
}

// formatStrategy collapses the typed ReleaseStrategy variants into a
// short label suitable for the table form. The JSON renderer carries
// the full structure verbatim.
func formatStrategy(r domain.ReleaseStrategy) string {
	switch v := r.(type) {
	case domain.ExplicitRelease:
		return "Explicit (release time known for this venue)"
	case domain.DiscoveredRelease:
		return fmt.Sprintf("Discovered (probe %s..%s)",
			v.ProbeFrom.UTC().Format("2006-01-02T15:04Z"),
			v.ProbeUntil.UTC().Format("2006-01-02T15:04Z"))
	case domain.ContinuousRelease:
		return fmt.Sprintf("Continuous (until %s)",
			v.Until.UTC().Format("2006-01-02T15:04Z"))
	case nil:
		return "(no strategy)"
	default:
		return fmt.Sprintf("(unknown: %T)", r)
	}
}

// formatSigning renders the planner's SigningRequired flag in plain
// English. The plain-bool form ("true"/"false") is too easy to misread
// in a vertically-aligned label/value table.
func formatSigning(required bool) string {
	if required {
		return "required (high-demand window)"
	}
	return "probably not required"
}

// shortHash trims the canonical "sha256:<hex>" plan hash to the first
// 8 hex chars after the prefix. Matches the design's "a8f2…" example.
// The JSON renderer carries the full hash.
func shortHash(h string) string {
	if h == "" {
		return "(unset)"
	}
	// Cut off the "sha256:" prefix for the short form so the displayed
	// value matches the design doc's "a8f2…" shape (no algorithm tag).
	body := strings.TrimPrefix(h, "sha256:")
	if len(body) <= 8 {
		return body
	}
	return body[:8] + "…"
}

// shortWallTime renders WallTime as HH:MM (dropping seconds). The
// planner's slot expansion writes :00 seconds by default; the design's
// example output shows minute-precision throughout.
func shortWallTime(w domain.WallTime) string {
	return fmt.Sprintf("%02d:%02d", w.Hour, w.Minute)
}

// planTZ returns the venue's TZ or UTC as a safe fallback. The
// renderer must be total even on a degenerate Venue.
func planTZ(v domain.Venue) *time.Location {
	if v.TZ != nil {
		return v.TZ
	}
	return time.UTC
}

// nonEmpty returns s if non-empty, otherwise fallback. Trivial helper
// kept local so the renderer stays self-contained.
func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// Compile-time check: *service.Standard satisfies questPlanService.
// Kept here so a future change to the Service surface that drops
// PlanQuest is caught at this file's compile rather than at the
// indirect call site.
var _ questPlanService = (*service.Standard)(nil)
