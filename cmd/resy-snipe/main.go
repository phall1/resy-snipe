// Command resy-snipe is the Phase 1 CLI entry point. It is intentionally
// thin — all reservation logic lives in internal/engine and the provider
// adapter under internal/resy.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/notify"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stderr, clock.NewReal()); err != nil {
		fmt.Fprintln(os.Stderr, "resy-snipe: error:", err)
		os.Exit(1)
	}
}

// run parses CLI flags (and optionally walks the interactive prompt
// flow), assembles a domain.Intent, and dispatches to one of three
// paths:
//
//  1. `resy-snipe login …` — interactive credential capture, persists a
//     session row, and exits.
//  2. `resy-snipe …` with -user — full snipe lifecycle: open the store,
//     load the persisted session, build the engine, subscribe the
//     stdout notifier, run the chosen release strategy, and (on
//     Awaiting) drive the booking race. Exits non-zero unless the snipe
//     terminates in StatusBooked.
//  3. `resy-snipe …` without -user — dry-run preview that logs the
//     assembled Intent and returns. Useful for validating the flag
//     surface without touching the network or risking a misfire.
//
// run is split out from main so it can be exercised in tests without
// touching real os.Exit / os.Args / os.Stderr / os.Stdin.
func run(args []string, stdin io.Reader, logOut io.Writer, clk clock.Clock) error {
	// Subcommand dispatch. The first positional arg (when present and
	// not a flag) selects the subcommand; everything else falls through
	// to the existing snipe flow. Recognized subcommands: `login`,
	// `user`, `venue resolve`. Future subcommands plug in here.
	if len(args) > 1 && args[0] == "venue" && args[1] == "resolve" {
		return runResolveCmd(context.Background(), args[2:], stdin, logOut, clk)
	}
	if len(args) > 1 && args[0] == "quest" {
		switch args[1] {
		case "plan":
			return runQuestPlanCmd(context.Background(), args[2:], stdin, logOut, clk)
		case "list":
			return runQuestListCmd(context.Background(), args[2:], stdin, logOut, clk)
		case "get":
			return runQuestGetCmd(context.Background(), args[2:], stdin, logOut, clk)
		case "cancel":
			return runQuestCancelCmd(context.Background(), args[2:], stdin, logOut, clk)
		}
	}
	if len(args) > 0 && args[0] == "user" {
		return runUserCmd(context.Background(), args[1:], stdin, logOut, clk)
	}
	if len(args) > 0 && args[0] == "login" {
		// Parent ctx has no deadline because runLogin's prompt loop is
		// interactive (the user types their email + password — could
		// take any reasonable amount of time). runLogin wraps each
		// individual HTTP call in its own timeout to satisfy invariant
		// I-11 (every Resy HTTP call needs a deadline) without holding
		// the typist hostage to it.
		ctx := context.Background()
		client, cleanup, err := openCLIClient(ctx,
			newCLILogger(logOut, slog.LevelInfo), clk)
		if err != nil {
			return fmt.Errorf("login bootstrap: %w", err)
		}
		defer func() { _ = cleanup() }()
		return runLogin(ctx, stdin, logOut, client)
	}

	opts, err := parseFlags(args, logOut)
	if err != nil {
		return err
	}

	level, err := parseLogLevel(opts.logLevel)
	if err != nil {
		return err
	}
	logger := newCLILogger(logOut, level)

	// Default missing legacy flags from the historical config values so
	// `resy-snipe -snipe-time 00:00` (the README's hello-world example)
	// still produces a runnable Intent. The clock is injected so this
	// stays deterministic in tests.
	now := clk.Now().In(time.Local)
	opts.applyDefaults(now)

	if opts.interactive {
		if err := runInteractive(&opts, stdin, logOut); err != nil {
			return fmt.Errorf("interactive prompt: %w", err)
		}
	}

	intent, err := toIntent(opts, now)
	if err != nil {
		return err
	}

	logger.Info("intent assembled",
		slog.String(domain.LogKeyVenueRef, intent.Venue.String()),
		slog.String("date", intent.Date.String()),
		slog.Int("party_size", intent.PartySize),
		slog.Int("slot_prefs", len(intent.SlotPrefs)),
		slog.String("release", releaseSummary(intent.Release)),
		slog.String("intent_hash", string(intent.Hash())),
	)

	// No -user: dry-run preview path. We log the assembled Intent and
	// exit. This is intentional — running an end-to-end snipe without
	// an explicit user is too easy to do by accident, and silently
	// using whichever session happens to be in the store is exactly
	// the kind of footgun the spec calls out. The user confirms the
	// account by passing -user; without it, this binary will not
	// touch the network.
	if strings.TrimSpace(opts.user) == "" {
		logger.Info("dry-run: pass -user <email> to enable the live snipe path")
		return nil
	}

	parent, stopSignals := installSignalCancel(context.Background())
	defer stopSignals()

	rclient, sqlStore, cleanup, err := openSnipeBackend(parent, logger, clk)
	if err != nil {
		return fmt.Errorf("snipe bootstrap: %w", err)
	}
	defer func() { _ = cleanup() }()

	sess, err := loadSessionForSnipe(parent,
		&clientAdapter{inner: rclient}, domain.UserID(opts.user), logger)
	if err != nil {
		return err
	}

	notifier := newCLINotifier(os.Stdout, clk)
	defer func() { _ = notifier.Close() }()

	provider := &providerAdapter{Client: rclient}
	finalStatus, snipeErr := runSnipeFn(parent, intent, sess, sqlStore, provider, notifier, logger, clk)
	if snipeErr != nil {
		return snipeErr
	}
	if finalStatus != domain.StatusBooked {
		return fmt.Errorf("snipe did not book (final status: %s)", finalStatus)
	}
	return nil
}

// newCLINotifier returns the stdout notifier the CLI uses for live
// transition + result rendering. Split out so tests can swap in a
// fake writer without growing run()'s signature.
func newCLINotifier(w io.Writer, clk clock.Clock) *notify.StdoutNotifier {
	return notify.NewStdoutNotifier(w, clk)
}

// releaseSummary collapses the typed release strategy to a short
// human-readable tag for log output. Engine wiring will replace this
// with the actual scheduled timeline.
func releaseSummary(r domain.ReleaseStrategy) string {
	switch v := r.(type) {
	case domain.ExplicitRelease:
		return "explicit@" + v.At.Format(time.RFC3339)
	case domain.DiscoveredRelease:
		return "discovered[" + v.ProbeFrom.Format(time.RFC3339) + "→" + v.ProbeUntil.Format(time.RFC3339) + "]"
	case domain.ContinuousRelease:
		return "continuous→" + v.Until.Format(time.RFC3339)
	default:
		return "<unknown>"
	}
}

// parseLogLevel maps the CLI string into an slog.Level.
func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid --log-level %q (want debug|info|warn|error)", s)
	}
}

// newCLILogger returns the text-handler logger used by the CLI. The
// daemon (Phase 2) will construct a JSON-handler logger via the same DI
// path; production code never reaches for the global default logger.
func newCLILogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
