// Command resy-snipe is the Phase 1 CLI entry point. It is intentionally
// thin — all reservation logic lives in internal/engine and the provider
// adapter under internal/resy.
package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "resy-snipe: error:", err)
		os.Exit(1)
	}
}

// run parses CLI flags and constructs the dependencies needed by the
// engine. It is split out from main so it can be exercised in tests
// without touching real os.Exit / os.Args / os.Stderr.
func run(args []string, logOut io.Writer) error {
	fs := flag.NewFlagSet("resy-snipe", flag.ContinueOnError)
	fs.SetOutput(logOut)
	levelFlag := fs.String("log-level", "info", "log level: debug, info, warn, error")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}

	level, err := parseLogLevel(*levelFlag)
	if err != nil {
		return err
	}

	logger := newCLILogger(logOut, level)
	_ = logger // engine wiring lands in a later task; logger is constructed
	// here so main's responsibility is settled and DI plumbing is ready.

	return nil
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
