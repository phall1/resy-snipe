// Package main: `resy-snipe subscription create|list|get|cancel` sub-subcommands.
//
// Wired into main.go's subcommand dispatch by the `subscription` branch.
// Per Law 4 this file is CLI wiring + output formatting only.
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
	"text/tabwriter"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// newSubscriptionServiceFn is the test seam for the subscription handlers.
// In production it points at newQuestServiceFn (which opens the real
// sqlite DB + provider stack); tests swap in a fake service.Service.
var newSubscriptionServiceFn = newQuestServiceFn

// runSubscriptionCmd dispatches `resy-snipe subscription <verb> …`.
func runSubscriptionCmd(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	if len(args) == 0 {
		fprintf(out, "Usage: resy-snipe subscription <create|list|get|cancel> [args]\n")
		return errors.New("subscription: missing subcommand")
	}
	switch args[0] {
	case "create":
		return runSubscriptionCreateCmd(ctx, args[1:], stdin, out, clk)
	case "list":
		return runSubscriptionListCmd(ctx, args[1:], stdin, out, clk)
	case "get":
		return runSubscriptionGetCmd(ctx, args[1:], stdin, out, clk)
	case "cancel":
		return runSubscriptionCancelCmd(ctx, args[1:], stdin, out, clk)
	case "resume":
		return runSubscriptionResumeCmd(ctx, args[1:], stdin, out, clk)
	default:
		fprintf(out, "Usage: resy-snipe subscription <create|list|get|cancel|resume> [args]\n")
		return fmt.Errorf("subscription: unknown subcommand %q", args[0])
	}
}

// runSubscriptionCreateCmd implements `resy-snipe subscription create [flags]`.
func runSubscriptionCreateCmd(
	ctx context.Context,
	args []string,
	_ io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	fs := flag.NewFlagSet("subscription create", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe subscription create [-user <id>] -venue <name> -date YYYY-MM-DD -party N -time-start HH:MM -time-end HH:MM [-expires RFC3339]")
		fs.PrintDefaults()
	}
	userRaw := fs.String("user", "", "tenant user id (defaults to the sole operator)")
	venueRaw := fs.String("venue", "", "venue name (required)")
	dateRaw := fs.String("date", "", "target reservation date (YYYY-MM-DD) (required)")
	party := fs.Int("party", 0, "party size (required)")
	timeStart := fs.String("time-start", "", "start of time window HH:MM (required)")
	timeEnd := fs.String("time-end", "", "end of time window HH:MM (required)")
	expiresRaw := fs.String("expires", "", "optional expiration time (RFC3339)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("subscription create: %w", err)
	}

	if strings.TrimSpace(*venueRaw) == "" {
		return errors.New("subscription create: -venue is required")
	}
	if strings.TrimSpace(*dateRaw) == "" {
		return errors.New("subscription create: -date is required")
	}
	if *party <= 0 {
		return fmt.Errorf("subscription create: -party must be > 0 (got %d)", *party)
	}
	if strings.TrimSpace(*timeStart) == "" {
		return errors.New("subscription create: -time-start is required")
	}
	if strings.TrimSpace(*timeEnd) == "" {
		return errors.New("subscription create: -time-end is required")
	}

	date, err := domain.ParseDate(strings.TrimSpace(*dateRaw))
	if err != nil {
		return fmt.Errorf("subscription create: -date: %w", err)
	}
	start, err := domain.ParseWallTime(strings.TrimSpace(*timeStart))
	if err != nil {
		return fmt.Errorf("subscription create: -time-start: %w", err)
	}
	end, err := domain.ParseWallTime(strings.TrimSpace(*timeEnd))
	if err != nil {
		return fmt.Errorf("subscription create: -time-end: %w", err)
	}

	var expiresAt *time.Time
	if strings.TrimSpace(*expiresRaw) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*expiresRaw))
		if err != nil {
			return fmt.Errorf("subscription create: -expires: %w", err)
		}
		expiresAt = &t
	}

	goal := domain.Goal{
		VenueQuery:  domain.VenueQueryName{Name: strings.TrimSpace(*venueRaw)},
		Date:        date,
		Party:       *party,
		TimePrefs:   domain.TimeWindow{Start: start, End: end, Priority: domain.PriorityNone},
		Constraints: domain.Constraints{AnyTable: true},
	}

	logger := newCLILogger(out, slog.LevelInfo)
	svc, cleanup, err := newSubscriptionServiceFn(ctx, logger, clk)
	if err != nil {
		return fmt.Errorf("subscription create bootstrap: %w", err)
	}
	defer questCleanup(cleanup)

	userID, err := resolveQuestUser(ctx, *userRaw, clk)
	if err != nil {
		return fmt.Errorf("subscription create: %w", err)
	}

	subID, err := svc.CreateSubscription(ctx, userID, goal, nil, expiresAt)
	if err != nil {
		return fmt.Errorf("subscription create: %w", err)
	}
	fprintf(out, "created subscription %s\n", subID)
	return nil
}

// runSubscriptionListCmd implements `resy-snipe subscription list [flags]`.
func runSubscriptionListCmd(
	ctx context.Context,
	args []string,
	_ io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	fs := flag.NewFlagSet("subscription list", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe subscription list [flags]")
		fs.PrintDefaults()
	}
	statusRaw := fs.String("status", "", "comma-separated statuses (active|paused|fulfilled|expired|cancelled)")
	limit := fs.Int("limit", defaultQuestListLimit, "max rows to return")
	userRaw := fs.String("user", "", "tenant user id (defaults to the sole operator)")
	formatRaw := fs.String("format", string(questFormatTable), "output format: table or json")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("subscription list: %w", err)
	}

	statuses, err := parseSubscriptionStatusList(*statusRaw)
	if err != nil {
		return fmt.Errorf("subscription list: %w", err)
	}
	format, err := parseQuestFormat(*formatRaw)
	if err != nil {
		return fmt.Errorf("subscription list: %w", err)
	}
	if *limit <= 0 {
		return fmt.Errorf("subscription list: -limit must be > 0 (got %d)", *limit)
	}

	logger := newCLILogger(out, slog.LevelInfo)
	svc, cleanup, err := newSubscriptionServiceFn(ctx, logger, clk)
	if err != nil {
		return fmt.Errorf("subscription list bootstrap: %w", err)
	}
	defer questCleanup(cleanup)

	userID, err := resolveQuestUser(ctx, *userRaw, clk)
	if err != nil {
		return fmt.Errorf("subscription list: %w", err)
	}

	filter := service.SubscriptionFilter{
		Status: statuses,
		Limit:  *limit,
	}

	subs, err := svc.ListSubscriptions(ctx, userID, filter)
	if err != nil {
		return fmt.Errorf("subscription list: %w", err)
	}
	return renderSubscriptionList(out, subs, format)
}

// runSubscriptionGetCmd implements `resy-snipe subscription get <subID> [flags]`.
func runSubscriptionGetCmd(
	ctx context.Context,
	args []string,
	_ io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	fs := flag.NewFlagSet("subscription get", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe subscription get <subID> [-user <id>] [-format=table|json]")
		fs.PrintDefaults()
	}
	userRaw := fs.String("user", "", "tenant user id (defaults to the sole operator)")
	formatRaw := fs.String("format", string(questFormatTable), "output format: table or json")
	flagsFirst, positionals := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagsFirst); err != nil {
		return fmt.Errorf("subscription get: %w", err)
	}
	rest := positionals
	rest = append(rest, fs.Args()...)
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("subscription get: missing <subID> positional argument")
	}
	if len(rest) > 1 {
		fs.Usage()
		return fmt.Errorf("subscription get: unexpected extra arguments: %v", rest[1:])
	}
	subID := domain.SubscriptionID(strings.TrimSpace(rest[0]))
	if subID == "" {
		return errors.New("subscription get: subID is required")
	}
	format, err := parseQuestFormat(*formatRaw)
	if err != nil {
		return fmt.Errorf("subscription get: %w", err)
	}

	logger := newCLILogger(out, slog.LevelInfo)
	svc, cleanup, err := newSubscriptionServiceFn(ctx, logger, clk)
	if err != nil {
		return fmt.Errorf("subscription get bootstrap: %w", err)
	}
	defer questCleanup(cleanup)

	userID, err := resolveQuestUser(ctx, *userRaw, clk)
	if err != nil {
		return fmt.Errorf("subscription get: %w", err)
	}

	sub, err := svc.GetSubscription(ctx, userID, subID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			fprintf(out, "subscription not found: %s\n", subID)
			return fmt.Errorf("subscription get: %w", err)
		}
		return fmt.Errorf("subscription get: %w", err)
	}
	return renderSubscription(out, sub, format)
}

// runSubscriptionCancelCmd implements `resy-snipe subscription cancel <subID> [flags]`.
func runSubscriptionCancelCmd(
	ctx context.Context,
	args []string,
	_ io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	fs := flag.NewFlagSet("subscription cancel", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe subscription cancel <subID> [-user <id>]")
		fs.PrintDefaults()
	}
	userRaw := fs.String("user", "", "tenant user id (defaults to the sole operator)")
	flagsFirst, positionals := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagsFirst); err != nil {
		return fmt.Errorf("subscription cancel: %w", err)
	}
	rest := positionals
	rest = append(rest, fs.Args()...)
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("subscription cancel: missing <subID> positional argument")
	}
	if len(rest) > 1 {
		fs.Usage()
		return fmt.Errorf("subscription cancel: unexpected extra arguments: %v", rest[1:])
	}
	subID := domain.SubscriptionID(strings.TrimSpace(rest[0]))
	if subID == "" {
		return errors.New("subscription cancel: subID is required")
	}

	logger := newCLILogger(out, slog.LevelInfo)
	svc, cleanup, err := newSubscriptionServiceFn(ctx, logger, clk)
	if err != nil {
		return fmt.Errorf("subscription cancel bootstrap: %w", err)
	}
	defer questCleanup(cleanup)

	userID, err := resolveQuestUser(ctx, *userRaw, clk)
	if err != nil {
		return fmt.Errorf("subscription cancel: %w", err)
	}

	if err := svc.CancelSubscription(ctx, userID, subID); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			fprintf(out, "subscription not found: %s\n", subID)
			return fmt.Errorf("subscription cancel: %w", err)
		}
		return fmt.Errorf("subscription cancel: %w", err)
	}
	fprintf(out, "canceled %s\n", subID)
	return nil
}

// runSubscriptionResumeCmd implements `resy-snipe subscription resume <subID> [flags]`.
func runSubscriptionResumeCmd(
	ctx context.Context,
	args []string,
	_ io.Reader,
	out io.Writer,
	clk clock.Clock,
) error {
	fs := flag.NewFlagSet("subscription resume", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe subscription resume <subID> [-user <id>]")
		fs.PrintDefaults()
	}
	userRaw := fs.String("user", "", "tenant user id (defaults to the sole operator)")
	flagsFirst, positionals := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagsFirst); err != nil {
		return fmt.Errorf("subscription resume: %w", err)
	}
	rest := positionals
	rest = append(rest, fs.Args()...)
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("subscription resume: missing <subID> positional argument")
	}
	if len(rest) > 1 {
		fs.Usage()
		return fmt.Errorf("subscription resume: unexpected extra arguments: %v", rest[1:])
	}
	subID := domain.SubscriptionID(strings.TrimSpace(rest[0]))
	if subID == "" {
		return errors.New("subscription resume: subID is required")
	}

	logger := newCLILogger(out, slog.LevelInfo)
	svc, cleanup, err := newSubscriptionServiceFn(ctx, logger, clk)
	if err != nil {
		return fmt.Errorf("subscription resume bootstrap: %w", err)
	}
	defer questCleanup(cleanup)

	userID, err := resolveQuestUser(ctx, *userRaw, clk)
	if err != nil {
		return fmt.Errorf("subscription resume: %w", err)
	}

	if err := svc.ResumeSubscription(ctx, userID, subID); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			fprintf(out, "subscription not found: %s\n", subID)
			return fmt.Errorf("subscription resume: %w", err)
		}
		return fmt.Errorf("subscription resume: %w", err)
	}
	fprintf(out, "resumed %s\n", subID)
	return nil
}

// renderSubscriptionList renders subscriptions in table or JSON format.
func renderSubscriptionList(w io.Writer, subs []service.Subscription, format questFormat) error {
	if len(subs) == 0 {
		fprintln(w, "(no subscriptions)")
		return nil
	}
	switch format {
	case questFormatJSON:
		payload := make([]map[string]any, 0, len(subs))
		for _, s := range subs {
			payload = append(payload, subscriptionJSON(s))
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			return fmt.Errorf("render json: %w", err)
		}
		return nil
	case questFormatTable:
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fprintf(tw, "ID\tSTATUS\tVENUE\tDATE\tPARTY\tWINDOW\tNEXT POLL\n")
		for _, s := range subs {
			nextPoll := "-"
			if !s.NextPollAt.IsZero() {
				nextPoll = s.NextPollAt.UTC().Format(time.RFC3339)
			}
			fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s..%s\t%s\n",
				string(s.ID),
				s.Status.String(),
				goalVenueDisplay(s.Goal),
				s.Goal.Date.String(),
				s.Goal.Party,
				shortWallTime(s.Goal.TimePrefs.Start),
				shortWallTime(s.Goal.TimePrefs.End),
				nextPoll,
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

// renderSubscription renders a single subscription.
func renderSubscription(w io.Writer, sub service.Subscription, format questFormat) error {
	switch format {
	case questFormatJSON:
		payload := subscriptionJSON(sub)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			return fmt.Errorf("render json: %w", err)
		}
		return nil
	case questFormatTable:
		fprintf(w, "Subscription %s\n", sub.ID)
		fprintf(w, "  Status:     %s\n", sub.Status.String())
		fprintf(w, "  Venue:      %s\n", goalVenueDisplay(sub.Goal))
		fprintf(w, "  Date:       %s\n", sub.Goal.Date.String())
		fprintf(w, "  Party:      %d\n", sub.Goal.Party)
		fprintf(w, "  Window:     %s..%s\n",
			shortWallTime(sub.Goal.TimePrefs.Start),
			shortWallTime(sub.Goal.TimePrefs.End),
		)
		fprintf(w, "  Created:    %s\n", sub.CreatedAt.UTC().Format(time.RFC3339))
		if sub.ExpiresAt != nil {
			fprintf(w, "  Expires:    %s\n", sub.ExpiresAt.UTC().Format(time.RFC3339))
		}
		if sub.FulfilledBy != nil {
			fprintf(w, "  Fulfilled:  %s\n", string(*sub.FulfilledBy))
		}
		if !sub.NextPollAt.IsZero() {
			fprintf(w, "  Next poll:  %s\n", sub.NextPollAt.UTC().Format(time.RFC3339))
		}
		return nil
	default:
		return fmt.Errorf("render: unknown format %q", format)
	}
}

// subscriptionJSON projects a service.Subscription into a stable JSON shape.
func subscriptionJSON(s service.Subscription) map[string]any {
	out := map[string]any{
		"id":            string(s.ID),
		"user_id":       string(s.UserID),
		"status":        s.Status.String(),
		"venue":         goalVenueDisplay(s.Goal),
		"date":          s.Goal.Date.String(),
		"party":         s.Goal.Party,
		"time_start":    shortWallTime(s.Goal.TimePrefs.Start),
		"time_end":      shortWallTime(s.Goal.TimePrefs.End),
		"created_at":    s.CreatedAt.UTC().Format(time.RFC3339),
		"poll_interval": s.PollInterval.String(),
	}
	if s.ExpiresAt != nil {
		out["expires_at"] = s.ExpiresAt.UTC().Format(time.RFC3339)
	} else {
		out["expires_at"] = nil
	}
	if s.FulfilledBy != nil {
		out["fulfilled_by"] = string(*s.FulfilledBy)
	} else {
		out["fulfilled_by"] = nil
	}
	if !s.NextPollAt.IsZero() {
		out["next_poll_at"] = s.NextPollAt.UTC().Format(time.RFC3339)
	} else {
		out["next_poll_at"] = nil
	}
	return out
}

// parseSubscriptionStatusList parses comma-separated status names into
// []domain.SubscriptionStatus. Empty input returns nil (no filter).
func parseSubscriptionStatusList(s string) ([]domain.SubscriptionStatus, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, nil
	}
	var out []domain.SubscriptionStatus
	seen := map[domain.SubscriptionStatus]struct{}{}
	add := func(st domain.SubscriptionStatus) {
		if _, ok := seen[st]; ok {
			return
		}
		seen[st] = struct{}{}
		out = append(out, st)
	}
	for tok := range strings.SplitSeq(trimmed, ",") {
		t := strings.ToLower(strings.TrimSpace(tok))
		if t == "" {
			continue
		}
		switch t {
		case "active":
			add(domain.SubscriptionActive)
		case "paused":
			add(domain.SubscriptionPaused)
		case "fulfilled":
			add(domain.SubscriptionFulfilled)
		case "expired":
			add(domain.SubscriptionExpired)
		case "cancelled":
			add(domain.SubscriptionCancelled)
		default:
			return nil, fmt.Errorf("unknown status %q (want one of: active|paused|fulfilled|expired|cancelled)", tok)
		}
	}
	return out, nil
}
