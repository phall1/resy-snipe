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
// flow), assembles a domain.Intent, and — for now — emits the assembled
// Intent through the structured logger. engine.Run wiring lands in a
// downstream task.
//
// run is split out from main so it can be exercised in tests without
// touching real os.Exit / os.Args / os.Stderr / os.Stdin.
func run(args []string, stdin io.Reader, logOut io.Writer, clk clock.Clock) error {
	// Subcommand dispatch. The first positional arg (when present and
	// not a flag) selects the subcommand; everything else falls through
	// to the existing snipe flow. Today only `login` is recognized;
	// future subcommands (e.g. `logout`, `status`) plug in here.
	if len(args) > 0 && args[0] == "login" {
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

	// Construct the user-facing notifier. The Notifier interface is the
	// seam where SMS / iMessage / chat-bot frontends will plug in; here
	// we wire the Phase 1 stdout impl. The notifier is intentionally
	// instantiated even though no engine.Run wiring exists yet — that
	// integration lands in a downstream task. Holding the reference
	// proves the construction is side-effect-free for tests that
	// exercise run() without a TTY.
	//
	// TODO(engine-integration): once the engine exposes its lifecycle
	// event stream, subscribe here and forward to notifier.Transition /
	// notifier.Result. Today the snipe-running path is still a no-op.
	_ = newCLINotifier(os.Stdout, clk)

	// Session load: when the user passed -user, look up the persisted
	// session before handing off to the engine. ErrNotFound /
	// ErrSessionExpired both surface as the actionable
	// "run 'resy-snipe login' first" message — the spec is explicit
	// that an expired session must NOT silently re-login mid-snipe.
	//
	// When -user is empty we skip this step so the existing flag-only
	// invocation paths (and the ones the README documents today) keep
	// working. Engine wiring in a downstream task will tighten this.
	if strings.TrimSpace(opts.user) != "" {
		ctx := context.Background()
		client, cleanup, err := openCLIClient(ctx, logger, clk)
		if err != nil {
			return fmt.Errorf("snipe bootstrap: %w", err)
		}
		defer func() { _ = cleanup() }()
		if _, err := loadSessionForSnipe(ctx, client, domain.UserID(opts.user), logger); err != nil {
			return err
		}
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
