// Package main: `resy-snipe quest list|get|cancel` sub-subcommands.
//
// Wiring intent: this file is wired into main.go's subcommand dispatch
// by adding the following branch near the existing `user` / `venue
// resolve` branches (the harness performs the edit at wave end; M1-18
// wires the `plan` case, M1-19 wires the `create` case):
//
//	if len(args) > 1 && args[0] == "quest" {
//	    switch args[1] {
//	    case "list":
//	        return runQuestListCmd(context.Background(), args[2:], stdin, logOut, clk)
//	    case "get":
//	        return runQuestGetCmd(context.Background(), args[2:], stdin, logOut, clk)
//	    case "cancel":
//	        return runQuestCancelCmd(context.Background(), args[2:], stdin, logOut, clk)
//	    // case "plan":   wired by M1-18
//	    // case "create": wired by M1-19
//	    }
//	}
//
// Until that branch lands the entry points below are reachable only
// via the cmd/resy-snipe test suite (which calls each runQuest*Cmd
// directly).
//
// Per Law 4 the cmd/ package stays wiring-only: every handler parses
// flags, calls into a fully-wired service.Service, and renders the
// result. All business logic — tenancy folding, plan recomputation,
// event ordering — lives in internal/service.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"text/tabwriter"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
	"resy-snipe/internal/store"
)

// questFormat selects the output rendering for the quest verbs. The
// same vocabulary is shared by `list`, `get`, and `cancel` even though
// `cancel` only ever produces a one-line confirmation — keeping the
// flag uniform across the trio makes the CLI surface predictable.
type questFormat string

const (
	questFormatTable questFormat = "table"
	questFormatJSON  questFormat = "json"
)

// defaultQuestListLimit caps the number of rows `quest list` returns
// when -limit is not supplied. 50 mirrors the design-doc "most-recent
// N" sketch and keeps a fresh terminal scroll buffer happy.
const defaultQuestListLimit = 50

// newQuestServiceFn is the test seam for the quest-* handlers. In
// production it points at newStandardService (which opens the real
// sqlite DB + provider stack); resolve-style tests swap in a fake
// service.Service so the handler logic can be exercised without
// touching the network or the filesystem.
//
// The returned cleanup is invoked unconditionally on handler exit (via
// defer). It MUST tolerate being called even when construction failed
// — production callers return the zero cleanup (nil) on the error
// path, which the helper below short-circuits.
var newQuestServiceFn = func(
	ctx context.Context,
	logger *slog.Logger,
	clk clock.Clock,
) (service.Service, func() error, error) {
	svc, cleanup, err := newStandardService(ctx, logger, clk)
	if err != nil {
		return nil, nil, err
	}
	return svc, cleanup, nil
}

// runQuestListCmd implements `resy-snipe quest list [flags]`.
//
// Filters:
//   - -status   comma-separated status names (see parseStatusList)
//   - -account  filter to one Resy account (email or acct_… id)
//   - -since    RFC3339 created-at lower bound (inclusive)
//   - -until    RFC3339 created-at upper bound (exclusive)
//   - -limit    max rows (default defaultQuestListLimit)
//   - -user     target user id; defaults to the sole operator via
//     resolveDefaultUser
//   - -format   table (default) or json
//
// Empty result prints "(no quests)" to give the user a clear signal
// they did not just hit a paginated empty page.
func runQuestListCmd(
	ctx context.Context,
	args []string,
	_ io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	fs := flag.NewFlagSet("quest list", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe quest list [flags]")
		fs.PrintDefaults()
	}
	statusRaw := fs.String("status", "", "comma-separated statuses (submitted|scheduled|awaiting|finding|booking|booked|failed|canceled|expired; aliases: pending=submitted, firing={awaiting,finding,booking})")
	accountRaw := fs.String("account", "", "filter to one account (email or acct_… id)")
	sinceRaw := fs.String("since", "", "created-at lower bound (RFC3339)")
	untilRaw := fs.String("until", "", "created-at upper bound (RFC3339)")
	limit := fs.Int("limit", defaultQuestListLimit, "max rows to return")
	userRaw := fs.String("user", "", "tenant user id (defaults to the sole operator)")
	formatRaw := fs.String("format", string(questFormatTable), "output format: table or json")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("quest list: %w", err)
	}

	statuses, err := parseStatusList(*statusRaw)
	if err != nil {
		return fmt.Errorf("quest list: %w", err)
	}
	since, err := parseOptionalTime(*sinceRaw)
	if err != nil {
		return fmt.Errorf("quest list: -since: %w", err)
	}
	until, err := parseOptionalTime(*untilRaw)
	if err != nil {
		return fmt.Errorf("quest list: -until: %w", err)
	}
	format, err := parseQuestFormat(*formatRaw)
	if err != nil {
		return fmt.Errorf("quest list: %w", err)
	}
	if *limit <= 0 {
		return fmt.Errorf("quest list: -limit must be > 0 (got %d)", *limit)
	}

	logger := newCLILogger(out, slog.LevelInfo)
	svc, cleanup, err := newQuestServiceFn(ctx, logger, clk)
	if err != nil {
		return fmt.Errorf("quest list bootstrap: %w", err)
	}
	defer questCleanup(cleanup)

	userID, err := resolveQuestUser(ctx, *userRaw, clk)
	if err != nil {
		return fmt.Errorf("quest list: %w", err)
	}

	filter := service.ListFilter{
		Status: statuses,
		Since:  since,
		Until:  until,
		Limit:  *limit,
	}
	if strings.TrimSpace(*accountRaw) != "" {
		acct := domain.AccountID(strings.TrimSpace(*accountRaw))
		filter.AccountID = &acct
	}

	rows, err := svc.ListQuests(ctx, userID, filter)
	if err != nil {
		return fmt.Errorf("quest list: %w", err)
	}
	return renderQuestList(out, rows, format)
}

// runQuestGetCmd implements `resy-snipe quest get <questID> [flags]`.
//
// Wrong-tenant access surfaces as ErrNotFound: the design says
// tenancy elides existence, so the user sees "quest not found"
// regardless of whether they typed a bogus id or someone else's id.
//
// Events come from the M1-11 quest_events stream; M1-10 always returns
// an empty list, so the renderer prints "(no events recorded yet)"
// when state.Events is empty.
func runQuestGetCmd(
	ctx context.Context,
	args []string,
	_ io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	fs := flag.NewFlagSet("quest get", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe quest get <questID> [-user <id>] [-format=table|json]")
		fs.PrintDefaults()
	}
	userRaw := fs.String("user", "", "tenant user id (defaults to the sole operator)")
	formatRaw := fs.String("format", string(questFormatTable), "output format: table or json")
	flagsFirst, positionals := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagsFirst); err != nil {
		return fmt.Errorf("quest get: %w", err)
	}
	rest := positionals
	rest = append(rest, fs.Args()...)
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("quest get: missing <questID> positional argument")
	}
	if len(rest) > 1 {
		fs.Usage()
		return fmt.Errorf("quest get: unexpected extra arguments: %v", rest[1:])
	}
	questID := domain.QuestID(strings.TrimSpace(rest[0]))
	if questID == "" {
		return errors.New("quest get: questID is required")
	}
	format, err := parseQuestFormat(*formatRaw)
	if err != nil {
		return fmt.Errorf("quest get: %w", err)
	}

	logger := newCLILogger(out, slog.LevelInfo)
	svc, cleanup, err := newQuestServiceFn(ctx, logger, clk)
	if err != nil {
		return fmt.Errorf("quest get bootstrap: %w", err)
	}
	defer questCleanup(cleanup)

	userID, err := resolveQuestUser(ctx, *userRaw, clk)
	if err != nil {
		return fmt.Errorf("quest get: %w", err)
	}

	state, err := svc.GetQuest(ctx, userID, questID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			// Tenancy elides existence (docs/v2/design/service-layer.md
			// §identity-scoping): a wrong-tenant lookup is
			// indistinguishable from a missing row.
			fprintf(out, "quest not found: %s\n", questID)
			return fmt.Errorf("quest get: %w", err)
		}
		return fmt.Errorf("quest get: %w", err)
	}
	return renderQuestState(out, state, format)
}

// runQuestCancelCmd implements `resy-snipe quest cancel <questID> [flags]`.
//
// By default the handler prompts before sending the cancel (typing 'y'
// or 'Y' accepts; everything else aborts with a non-zero exit). -yes
// skips the prompt for scripted use. The handler treats "user said no"
// as a non-error path that prints a clear message and returns nil so
// the binary exits 0 — refusing to confirm is a successful exit.
//
// Canceling a terminal quest is a no-op at the Service layer (per
// docs/v2/design/service-layer.md §error-model): the CLI still prints
// the "canceled" confirmation so the user gets an unambiguous signal
// the call landed.
func runQuestCancelCmd(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	fs := flag.NewFlagSet("quest cancel", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe quest cancel <questID> [-reason <str>] [-user <id>] [-yes]")
		fs.PrintDefaults()
	}
	reason := fs.String("reason", "", "short free-form annotation stored on the audit row")
	userRaw := fs.String("user", "", "tenant user id (defaults to the sole operator)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	flagsFirst, positionals := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagsFirst); err != nil {
		return fmt.Errorf("quest cancel: %w", err)
	}
	rest := positionals
	rest = append(rest, fs.Args()...)
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("quest cancel: missing <questID> positional argument")
	}
	if len(rest) > 1 {
		fs.Usage()
		return fmt.Errorf("quest cancel: unexpected extra arguments: %v", rest[1:])
	}
	questID := domain.QuestID(strings.TrimSpace(rest[0]))
	if questID == "" {
		return errors.New("quest cancel: questID is required")
	}

	if !*yes {
		reader := bufio.NewReader(stdin)
		fprintf(out, "Cancel quest %s? [y/N]: ", questID)
		raw, perr := reader.ReadString('\n')
		if perr != nil && (perr != io.EOF || raw == "") {
			return fmt.Errorf("quest cancel: prompt: %w", perr)
		}
		ans := strings.ToLower(strings.TrimSpace(raw))
		if ans != "y" && ans != "yes" {
			fprintln(out, "aborted (no changes made)")
			return nil
		}
	}

	logger := newCLILogger(out, slog.LevelInfo)
	svc, cleanup, err := newQuestServiceFn(ctx, logger, clk)
	if err != nil {
		return fmt.Errorf("quest cancel bootstrap: %w", err)
	}
	defer questCleanup(cleanup)

	userID, err := resolveQuestUser(ctx, *userRaw, clk)
	if err != nil {
		return fmt.Errorf("quest cancel: %w", err)
	}

	if err := svc.CancelQuest(ctx, userID, questID, service.CancelOpts{Reason: *reason}); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			fprintf(out, "quest not found: %s\n", questID)
			return fmt.Errorf("quest cancel: %w", err)
		}
		return fmt.Errorf("quest cancel: %w", err)
	}
	fprintf(out, "canceled %s\n", questID)
	return nil
}

// questCleanup invokes the per-service cleanup defensively: a nil
// cleanup (the error-path return from newQuestServiceFn) is a no-op,
// and errors from a real cleanup land at debug level — the user-facing
// command has already succeeded, and a missed Close on a sqlite handle
// is recoverable on the next invocation.
func questCleanup(cleanup func() error) {
	if cleanup == nil {
		return
	}
	_ = cleanup()
}

// resolveQuestUser picks the UserID the quest verbs operate against.
// When `raw` is non-empty it is the user id verbatim (we trust the
// caller — the Service layer will fold a bogus id into ErrNotFound on
// every read). When `raw` is empty we fall back to the sole operator
// via the resolveDefaultUserFn seam; multi-tenant DBs require an
// explicit -user.
func resolveQuestUser(ctx context.Context, raw string, clk clock.Clock) (domain.UserID, error) {
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		return domain.UserID(trimmed), nil
	}
	return resolveDefaultUserFn(ctx, clk)
}

// resolveDefaultUserFn is the test seam for the no-user-flag path.
// Production points it at resolveDefaultUserViaStore (which opens the
// sqlite DB and calls store.ListUsers); the quest_ops tests swap in a
// canned implementation so the handler logic can be exercised
// without seeding the operator on every test run.
var resolveDefaultUserFn = resolveDefaultUserViaStore

// resolveDefaultUserViaStore is the production implementation: it
// opens the operator's sqlite DB, calls store.ListUsers (the
// cross-tenant admin function — see internal/store/users.go for the
// exemption rationale), and returns the sole user. Zero users → a
// "seed first" hint; two-plus users → an explicit "specify -user"
// error so the caller knows to disambiguate.
func resolveDefaultUserViaStore(ctx context.Context, clk clock.Clock) (domain.UserID, error) {
	db, cleanup, err := openSeedDB(ctx, clk)
	if err != nil {
		return "", fmt.Errorf("resolve default user: %w", err)
	}
	defer func() { _ = cleanup() }()
	rows, err := store.ListUsers(ctx, db)
	if err != nil {
		return "", fmt.Errorf("list users: %w", err)
	}
	switch len(rows) {
	case 0:
		return "", errors.New("no users in DB — run `resy-snipe user seed --email <email>` to mint the operator")
	case 1:
		return rows[0].ID, nil
	default:
		return "", fmt.Errorf("multiple users in DB (%d) — specify -user explicitly", len(rows))
	}
}

// renderQuestList writes a table or JSON view of `rows` to out.
//
// Table columns: ID, Account, Venue, Date, Status, CreatedAt,
// CompletedAt. Venue and Date come from the persisted Goal (which the
// service does not project into QuestSummary in M1-10); M1-10 returns
// a slim summary so the table renders "-" for those columns until M1-11
// lands the projection (or until we widen QuestSummary upstream).
//
// Empty input prints "(no quests)" and returns nil so a caller can
// distinguish "the call succeeded" from "the call errored out".
func renderQuestList(out io.Writer, rows []service.QuestSummary, format questFormat) error {
	if len(rows) == 0 {
		fprintln(out, "(no quests)")
		return nil
	}
	switch format {
	case questFormatJSON:
		payload := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			payload = append(payload, questSummaryJSON(r))
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			return fmt.Errorf("render json: %w", err)
		}
		return nil
	case questFormatTable:
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fprintf(tw, "ID\tACCOUNT\tVENUE\tDATE\tSTATUS\tCREATED\tCOMPLETED\n")
		for _, r := range rows {
			completed := "-"
			if r.CompletedAt != nil {
				completed = r.CompletedAt.UTC().Format(time.RFC3339)
			}
			fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				string(r.ID),
				accountDisplay(r.AccountID),
				"-", // venue: not projected into QuestSummary in M1-10
				"-", // date:  ditto
				r.Status.String(),
				r.CreatedAt.UTC().Format(time.RFC3339),
				completed,
			)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("render table: flush: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("render: unknown format %q", format)
	}
}

// renderQuestState writes a `quest get` view of state to out.
//
// The table form mimics resolve's columnar layout: a summary block,
// a plan summary block, and an event-log block (or the empty-events
// hint when len(state.Events) == 0).
func renderQuestState(out io.Writer, state service.QuestState, format questFormat) error {
	switch format {
	case questFormatJSON:
		payload := map[string]any{
			"summary": questSummaryJSON(state.Summary),
			"goal": map[string]any{
				"date":       state.Goal.Date.String(),
				"party":      state.Goal.Party,
				"account_id": string(state.Goal.AccountID),
				"venue":      goalVenueDisplay(state.Goal),
				"time_start": state.Goal.TimePrefs.Start.String(),
				"time_end":   state.Goal.TimePrefs.End.String(),
			},
			"plan": map[string]any{
				"hash":             state.Plan.Hash,
				"drop_moment":      state.Plan.DropMoment.UTC().Format(time.RFC3339),
				"signing_required": state.Plan.SigningRequired,
				"fire_count":       len(state.Plan.FireSchedule),
			},
			"events": eventsJSON(state.Events),
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			return fmt.Errorf("render json: %w", err)
		}
		return nil
	case questFormatTable:
		s := state.Summary
		fprintf(out, "Quest %s\n", s.ID)
		fprintf(out, "  Status:    %s\n", s.Status.String())
		fprintf(out, "  Account:   %s\n", accountDisplay(s.AccountID))
		fprintf(out, "  User:      %s\n", string(s.UserID))
		fprintf(out, "  Plan hash: %s\n", s.PlanHash)
		fprintf(out, "  Created:   %s\n", s.CreatedAt.UTC().Format(time.RFC3339))
		fprintf(out, "  Updated:   %s\n", s.UpdatedAt.UTC().Format(time.RFC3339))
		if s.CompletedAt != nil {
			fprintf(out, "  Completed: %s\n", s.CompletedAt.UTC().Format(time.RFC3339))
		}
		fprintf(out, "Goal:\n")
		fprintf(out, "  Venue: %s\n", goalVenueDisplay(state.Goal))
		fprintf(out, "  Date:  %s\n", state.Goal.Date.String())
		fprintf(out, "  Party: %d\n", state.Goal.Party)
		fprintf(out, "  Time:  %s..%s\n",
			state.Goal.TimePrefs.Start.String(),
			state.Goal.TimePrefs.End.String())
		fprintf(out, "Plan:\n")
		fprintf(out, "  Hash:        %s\n", state.Plan.Hash)
		if !state.Plan.DropMoment.IsZero() {
			fprintf(out, "  Drop moment: %s\n", state.Plan.DropMoment.UTC().Format(time.RFC3339))
		}
		fprintf(out, "  Signing:     %t\n", state.Plan.SigningRequired)
		fprintf(out, "  Fire slots:  %d\n", len(state.Plan.FireSchedule))
		fprintf(out, "Events:\n")
		if len(state.Events) == 0 {
			fprintln(out, "  (no events recorded yet)")
			return nil
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fprintf(tw, "  AT\tTYPE\n")
		for _, ev := range state.Events {
			fprintf(tw, "  %s\t%s\n",
				ev.At.UTC().Format(time.RFC3339),
				string(ev.Type),
			)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("render events: flush: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("render: unknown format %q", format)
	}
}

// questSummaryJSON projects a QuestSummary into a stable JSON shape.
// CompletedAt is nullable; we render the zero value as nil so the JSON
// consumer can branch cleanly on presence.
func questSummaryJSON(r service.QuestSummary) map[string]any {
	out := map[string]any{
		"id":         string(r.ID),
		"user_id":    string(r.UserID),
		"account_id": string(r.AccountID),
		"status":     r.Status.String(),
		"plan_hash":  r.PlanHash,
		"created_at": r.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": r.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if r.CompletedAt != nil {
		out["completed_at"] = r.CompletedAt.UTC().Format(time.RFC3339)
	} else {
		out["completed_at"] = nil
	}
	return out
}

// eventsJSON projects an event slice into the JSON wire shape. Attrs
// are dropped at this seam — M1-10 returns an empty slice anyway, and
// the M1-11 quest_events stream will widen this helper when it lands.
func eventsJSON(evs []domain.Event) []map[string]any {
	out := make([]map[string]any, 0, len(evs))
	for _, ev := range evs {
		out = append(out, map[string]any{
			"at":   ev.At.UTC().Format(time.RFC3339),
			"type": string(ev.Type),
		})
	}
	return out
}

// accountDisplay renders an AccountID for human-facing output. Empty
// ids show as "-" so the table column never collapses; non-empty ids
// pass through verbatim.
func accountDisplay(id domain.AccountID) string {
	if id == "" {
		return "-"
	}
	return string(id)
}

// goalVenueDisplay renders a Goal's VenueQuery as a single human-line.
// Falls back to "-" for the zero VenueQuery so the table column is
// always populated.
func goalVenueDisplay(g domain.Goal) string {
	if g.VenueQuery == nil {
		return "-"
	}
	return g.VenueQuery.String()
}

// parseStatusList parses the comma-separated -status flag into a
// []domain.Status. Empty input returns a nil slice (no filter). The
// recognized tokens are the lowercase Status.String() values plus a
// handful of user-facing aliases pulled from
// docs/v2/milestones/m1-goal-driven.md §what-ships:
//
//	pending     → Submitted
//	firing      → Awaiting, Finding, Booking   (in-flight)
//
// An unknown token surfaces as an error so a typo doesn't silently
// produce an empty result set.
func parseStatusList(raw string) ([]domain.Status, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	var out []domain.Status
	seen := map[domain.Status]struct{}{}
	add := func(s domain.Status) {
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for tok := range strings.SplitSeq(trimmed, ",") {
		t := strings.ToLower(strings.TrimSpace(tok))
		if t == "" {
			continue
		}
		switch t {
		case "submitted", "pending":
			add(domain.StatusSubmitted)
		case "scheduled":
			add(domain.StatusScheduled)
		case "discovering":
			add(domain.StatusDiscovering)
		case "awaiting":
			add(domain.StatusAwaiting)
		case "finding":
			add(domain.StatusFinding)
		case "booking":
			add(domain.StatusBooking)
		case "firing":
			add(domain.StatusAwaiting)
			add(domain.StatusFinding)
			add(domain.StatusBooking)
		case "booked":
			add(domain.StatusBooked)
		case "failed":
			add(domain.StatusFailed)
		case "canceled":
			add(domain.StatusCanceled)
		case "expired":
			add(domain.StatusExpired)
		default:
			return nil, fmt.Errorf("unknown status %q (want one of: submitted|scheduled|discovering|awaiting|finding|booking|booked|failed|canceled|expired, or aliases pending|firing)", tok)
		}
	}
	return out, nil
}

// parseOptionalTime decodes an RFC3339 timestamp, returning a nil
// pointer when the input is empty. Used for both -since and -until on
// `quest list`: both are optional and a nil pointer signals "no bound".
func parseOptionalTime(raw string) (*time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil //nolint:nilnil // optional time: nil pointer is the documented "no bound" signal.
	}
	t, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid RFC3339 timestamp %q: %w", raw, err)
	}
	return &t, nil
}

// parseQuestFormat normalizes the -format flag's value. The accepted
// vocabulary is shared across quest verbs so the same helper backs
// list / get / cancel (cancel ignores the result today but the flag is
// scaffolded for symmetry).
func parseQuestFormat(raw string) (questFormat, error) {
	f := questFormat(strings.ToLower(strings.TrimSpace(raw)))
	switch f {
	case questFormatTable, questFormatJSON:
		return f, nil
	default:
		return "", fmt.Errorf("invalid -format %q (want table|json)", raw)
	}
}
