// Package main: `resy-snipe quest create …` sub-subcommand.
//
// Wired into main.go's `quest` switch by adding a "create" case alongside
// the existing list/get/cancel/plan branches. The exact addition:
//
//	case "create":
//	    return runQuestCreateCmd(context.Background(), args[2:], stdin, logOut, clk)
//
// Until that line lands the entry point below is reachable only via the
// cmd/resy-snipe test suite (which calls runQuestCreateCmd directly).
//
// Surface:
//
//	resy-snipe quest create <url|slug+city|name>
//	    [-date YYYY-MM-DD] [-party N] -time HH:MM..HH:MM
//	    [-priority earlier|later|none] [-tables type1,type2,...]
//	    -account <email> -user <user_id>
//	    [-yes] [-idempotency-key <string>]
//	    [-format=table|json]
//
// Behavior (ADR-0012 §plan-first-ux): the handler plans the quest, prints
// the Plan via PrintPlanTable (or JSON), then either honors -yes or
// reads a y/n confirmation from stdin. On confirm the handler calls
// Service.CreateQuest with CreateOpts.PlanHash pinned to the just-shown
// plan, so a stale venue config between preview and commit surfaces as
// ErrInvalidPlanHash rather than silently committing a different plan.
//
// Per Law 4 this file is CLI wiring + output formatting only. Plan
// recomputation, hash-verify, and the idempotency-key write-once logic
// all live inside internal/service.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// questCreateOpts is the parsed shape of `quest create` flags. It is a
// superset of questPlanOpts — the planning prelude reuses the same flag
// names and meanings; the extra knobs (-yes, -idempotency-key) gate the
// commit step that follows the plan-preview.
type questCreateOpts struct {
	plan           questPlanOpts
	yes            bool
	idempotencyKey string
}

// questCreateService is the slim subset of service.Service that
// runQuestCreateCmd needs. Narrowing the seam keeps the test double
// trivial (the Standard service exposes ~15 methods; we only call
// PlanQuest + CreateQuest here).
type questCreateService interface {
	PlanQuest(ctx context.Context, userID domain.UserID, goal domain.Goal) (domain.Plan, error)
	CreateQuest(ctx context.Context, userID domain.UserID, goal domain.Goal, opts service.CreateOpts) (domain.QuestID, error)
}

// openQuestCreateServiceFn is the test seam: production points it at
// newStandardService (wrapped to satisfy the narrowed interface);
// quest_create_test.go swaps in an in-memory fake so the CLI surface can
// be exercised without a real *resy.Client or sqlite store.
//
// The cleanup is invoked unconditionally on handler exit; tests return
// a no-op cleanup.
var openQuestCreateServiceFn = func(
	ctx context.Context,
	logger *slog.Logger,
	clk clock.Clock,
) (questCreateService, func() error, error) {
	svc, cleanup, err := newStandardService(ctx, logger, clk)
	if err != nil {
		return nil, nil, err
	}
	return svc, cleanup, nil
}

// runQuestCreateCmd is the entry point for `quest create`. See the file
// header for the wiring line main.go must add to dispatch into it.
//
// stdin is read for the y/N confirmation when -yes is not supplied; the
// caller MUST pass a real reader (os.Stdin in production, a
// strings.Reader in tests). out is where ALL user-visible output lands.
// Returning a non-nil error signals a non-zero exit code.
func runQuestCreateCmd(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	opts, err := parseQuestCreateFlags(args, out)
	if err != nil {
		return err
	}

	// Resolve the positional venue argument + URL hints, then assemble
	// the Goal. Every flag-shaped error lands here so we refuse the
	// request before touching the service (matches quest plan's order).
	query, hints, err := buildVenueQueryWithHints(opts.plan.venueQuery)
	if err != nil {
		return fmt.Errorf("quest create: %w", err)
	}
	goal, err := buildGoalFromOpts(opts.plan, query, hints)
	if err != nil {
		return fmt.Errorf("quest create: %w", err)
	}

	logger := newCLILogger(out, slog.LevelInfo)
	svc, cleanup, err := openQuestCreateServiceFn(ctx, logger, clk)
	if err != nil {
		return fmt.Errorf("quest create bootstrap: %w", err)
	}
	defer func() {
		if cleanup != nil {
			_ = cleanup()
		}
	}()

	userID := domain.UserID(opts.plan.user)

	// Plan first. The hash from this Plan is the pin we send back into
	// CreateQuest below — ADR-0012 §plan-first-ux.
	plan, planErr := svc.PlanQuest(ctx, userID, goal)
	if planErr != nil {
		fprintf(out, "Plan failed: %v\n", planErr)
		return planErr
	}

	// Render the plan for the user to inspect. The table form mirrors
	// quest plan's output exactly (PrintPlanTable is the package-exported
	// wrapper); JSON mode is for scripted callers piping through jq.
	switch opts.plan.format {
	case questPlanFormatJSON:
		if rerr := renderPlanJSON(out, plan); rerr != nil {
			return rerr
		}
	default:
		if rerr := PrintPlanTable(out, plan, clk.Now()); rerr != nil {
			return rerr
		}
	}

	// Confirmation gate. -yes skips; otherwise we read one line from
	// stdin. A "no" answer is a user-initiated cancel — not an error —
	// so the binary exits 0 with a clear message.
	if !opts.yes {
		reader := bufio.NewReader(stdin)
		fprintf(out, "Create this quest? [y/N]: ")
		raw, perr := reader.ReadString('\n')
		if perr != nil && (perr != io.EOF || raw == "") {
			return fmt.Errorf("quest create: prompt: %w", perr)
		}
		ans := strings.ToLower(strings.TrimSpace(raw))
		if ans != "y" && ans != "yes" {
			fprintln(out, "canceled, no quest created")
			return nil
		}
	}

	createOpts := service.CreateOpts{
		PlanHash: &plan.Hash,
	}
	if opts.idempotencyKey != "" {
		k := opts.idempotencyKey
		createOpts.IdempotencyKey = &k
	}

	questID, err := svc.CreateQuest(ctx, userID, goal, createOpts)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidPlanHash):
			fprintln(out, "the plan changed between preview and create — re-run `quest create`")
		case errors.Is(err, service.ErrConflict),
			errors.Is(err, service.ErrIdempotencyConflict):
			fprintf(out, "idempotency conflict: %v\n", err)
		default:
			fprintf(out, "Create failed: %v\n", err)
		}
		return fmt.Errorf("quest create: %w", err)
	}

	// Final confirmation line. DropMoment is rendered in the venue's
	// local zone (or UTC fallback) so it lines up with the plan-preview
	// block above; the venue is the same Plan we just printed.
	fprintf(out, "created quest %s · scheduled at %s\n",
		string(questID),
		formatDropMoment(plan.DropMoment, planTZ(plan.Venue), clk.Now()),
	)
	return nil
}

// parseQuestCreateFlags reuses the questPlanOpts flag shape verbatim and
// layers on the create-only knobs (-yes, -idempotency-key). The
// positional venue argument flows in via opts.plan.venueQuery. Malformed
// flag inputs surface as the returned error rather than os.Exit so the
// test harness can assert on them.
func parseQuestCreateFlags(args []string, out io.Writer) (questCreateOpts, error) {
	fs := flag.NewFlagSet("quest create", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out,
			"Usage: resy-snipe quest create <url|slug+city|name> "+
				"[-date YYYY-MM-DD] [-party N] -time HH:MM..HH:MM "+
				"[-priority earlier|later|none] [-tables type1,type2,...] "+
				"-account <email> -user <user_id> "+
				"[-yes] [-idempotency-key <string>] [-format=table|json]")
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
	yes := fs.Bool("yes", false,
		"skip the interactive confirmation prompt (required when stdin is not a tty)")
	idem := fs.String("idempotency-key", "",
		"optional idempotency key passed through to the Service")

	if err := fs.Parse(args); err != nil {
		return questCreateOpts{}, fmt.Errorf("parse flags: %w", err)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return questCreateOpts{}, errors.New("missing venue argument")
	}
	venueQuery := strings.TrimSpace(strings.Join(rest, " "))
	if venueQuery == "" {
		return questCreateOpts{}, errors.New("empty venue argument")
	}

	f := questPlanFormat(strings.ToLower(strings.TrimSpace(*format)))
	switch f {
	case questPlanFormatTable, questPlanFormatJSON:
	default:
		return questCreateOpts{}, fmt.Errorf("invalid -format %q (want table|json)", *format)
	}

	return questCreateOpts{
		plan: questPlanOpts{
			venueQuery: venueQuery,
			date:       strings.TrimSpace(*date),
			party:      *party,
			timeWindow: strings.TrimSpace(*timeWindow),
			priority:   strings.ToLower(strings.TrimSpace(*priority)),
			tables:     strings.TrimSpace(*tables),
			account:    strings.TrimSpace(*account),
			user:       strings.TrimSpace(*user),
			format:     f,
		},
		yes:            *yes,
		idempotencyKey: strings.TrimSpace(*idem),
	}, nil
}

// Compile-time check: *service.Standard satisfies questCreateService.
// Kept here so a future change to the Service surface that drops either
// PlanQuest or CreateQuest is caught at this file's compile rather than
// at the indirect call site.
var _ questCreateService = (*service.Standard)(nil)
