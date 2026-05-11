// Package main: `resy-snipe migrate-secrets` subcommand.
//
// One-shot migration of v1 plaintext session JWTs in `sessions.jwt`
// into sealed rows in `secrets` (kind=resy_session_jwt). See
// internal/secrets/migrate.go for the per-row transactional logic;
// this file is wiring only — config + sealer init + report rendering.
//
// Refuses to run if the daemon is up: daemon.Boot takes an exclusive
// flock on `data.db.lock`, and so does this tool. ErrLockHeld is the
// "daemon is running, stop it first" signal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/daemon"
	"resy-snipe/internal/secrets"
)

// runMigrateSecretsCmd is the entry point for `resy-snipe migrate-
// secrets`. The signature mirrors runUserCmd / runResolveCmd so
// main.go's dispatch table can call it without ceremony.
func runMigrateSecretsCmd(ctx context.Context, args []string, stdin io.Reader, out io.Writer, clk clock.Clock) error {
	fs := flag.NewFlagSet("migrate-secrets", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "path to daemon config TOML (optional; falls back to default + env)")
	dryRun := fs.Bool("dry-run", false, "report counts without committing the migration")
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe migrate-secrets [--config <path>] [--dry-run]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("migrate-secrets: %w", err)
	}
	if len(fs.Args()) > 0 {
		fs.Usage()
		return fmt.Errorf("migrate-secrets: unexpected positional args: %v", fs.Args())
	}

	cfg, err := loadDaemonConfig(*configPath)
	if err != nil {
		return fmt.Errorf("migrate-secrets: %w", err)
	}

	// Boot replays the same dance `serve` does: flock + open + migrate
	// + sealer init. The flock guarantees the daemon is not running;
	// if it is, ErrLockHeld surfaces and we tell the operator to stop
	// it first. The banner goes to a discard sink — the migration tool
	// has its own one-line summary.
	logger := newCLILogger(out, slog.LevelInfo)
	stdinFile, _ := stdin.(*os.File)
	if stdinFile == nil {
		stdinFile = os.Stdin
	}
	d, err := daemon.Boot(ctx, cfg, daemon.Options{
		BannerWriter:     io.Discard,
		Clock:            clk,
		PassphraseSource: daemon.NewStdinPassphraseSource(stdinFile, out, "Passphrase: "),
	})
	if err != nil {
		if errors.Is(err, daemon.ErrLockHeld) {
			return errors.New("migrate-secrets: daemon appears to be running (data.db.lock held); stop `resy-snipe serve` and retry")
		}
		return fmt.Errorf("migrate-secrets: boot: %w", err)
	}
	defer func() { _ = d.Close() }()

	if d.Sealer == nil {
		return errors.New("migrate-secrets: cannot run in insecure mode — secrets sealing is required to migrate plaintext JWTs")
	}

	report, err := secrets.MigratePlaintextSessions(ctx, d.DB, d.Sealer, logger, *dryRun)
	if err != nil {
		return fmt.Errorf("migrate-secrets: %w", err)
	}

	verb := "migrated"
	if *dryRun {
		verb = "would-migrate"
	}
	fprintf(out, "migrate-secrets: %s=%d skipped=%d errors=%d\n",
		verb, report.Migrated, report.Skipped, len(report.Errors))
	for i, e := range report.Errors {
		fprintf(out, "  error[%d]: %v\n", i, e)
	}
	return nil
}

// loadDaemonConfig assembles a daemon.Config by walking the standard
// precedence stack: DefaultConfig -> optional TOML file -> env. CLI
// flags are not applied here because `migrate-secrets` does not expose
// the full daemon flag surface — operators wanting to override should
// use env vars or amend their config.toml.
func loadDaemonConfig(path string) (daemon.Config, error) {
	cfg := daemon.DefaultConfig()
	if path != "" {
		if err := cfg.ApplyTOMLFile(path); err != nil {
			return daemon.Config{}, fmt.Errorf("config: %w", err)
		}
	}
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return daemon.Config{}, fmt.Errorf("config env: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return daemon.Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}
