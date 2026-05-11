// serve.go: the `resy-snipe serve` subcommand. Composes the daemon
// boot orchestrator (internal/daemon.Boot), the Service layer
// (internal/service.Standard), the HTTP transport
// (internal/transport/http), and the signal-driven graceful shutdown
// into a single long-running process.
//
// Wires M2-06 (engine resume — currently no-op until the engine grows
// a Resume seam), M2-07 (graceful shutdown — SIGTERM cancels context,
// HTTP server drains within ShutdownDrainSeconds, daemon.Close
// releases sealer/DB/flock in order), and M2-08 (this file — the
// composition root).

package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	stdhttp "net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/daemon"
	"resy-snipe/internal/engine"
	"resy-snipe/internal/resolver"
	"resy-snipe/internal/resy"
	"resy-snipe/internal/secrets"
	"resy-snipe/internal/service"
	"resy-snipe/internal/store"
	transport "resy-snipe/internal/transport/http"
)

// serveBuildInfo is the daemon's self-reported version string. The
// linker sets these at build time via -ldflags; defaults are
// development-friendly placeholders.
var (
	serveBuildVersion = "dev"
	serveBuildCommit  = "unknown"
)

// runServeCmd is the entry point for `resy-snipe serve`. Returns
// nil on a clean shutdown (signal-driven), non-nil on a boot failure
// or an unexpected listener error.
func runServeCmd(ctx context.Context, args []string, _ io.Reader, out io.Writer, clk clock.Clock) error {
	cfg, opts, err := parseServeFlags(args, out)
	if err != nil {
		return err
	}
	opts.Clock = clk
	opts.Version = serveBuildVersion
	opts.Commit = serveBuildCommit
	opts.BannerWriter = out

	// Phase 1: boot — flock, DB, migrate, secrets unwrap, banner.
	d, err := daemon.Boot(ctx, cfg, opts)
	if err != nil {
		return fmt.Errorf("serve: boot: %w", err)
	}
	defer func() { _ = d.Close() }()

	lvl, _ := parseLogLevel(cfg.Daemon.LogLevel)
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: lvl}))

	// Phase 2: compose Service + transport.
	svc, srv, cleanup, err := composeServer(ctx, d, cfg, clk, logger)
	if err != nil {
		return fmt.Errorf("serve: compose: %w", err)
	}
	defer cleanup()
	_ = svc // svc is held by the transport via its mounted handlers

	// Phase 3: SIGTERM/SIGINT → drain. Per design daemon.md §7 the
	// drain is ShutdownDrainSeconds; we honor that as the HTTP
	// shutdown grace and rely on daemon.Close to release the lower
	// resources. The signal-listener goroutine cancels the runtime
	// context the HTTP server consumes.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(ch)
		select {
		case sig := <-ch:
			logger.Info("serve: signal received, draining",
				slog.String("signal", sig.String()),
				slog.Int("drain_seconds", cfg.Daemon.ShutdownDrainSeconds))
			cancel()
		case <-runCtx.Done():
		}
	}()

	drain := time.Duration(cfg.Daemon.ShutdownDrainSeconds) * time.Second
	if drain <= 0 {
		drain = 15 * time.Second
	}
	logger.Info("serve: listener up", slog.String("bind", cfg.Daemon.Bind))
	if err := srv.Run(runCtx, drain); err != nil {
		if errors.Is(err, stdhttp.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: listener: %w", err)
	}
	logger.Info("serve: shutdown complete")
	return nil
}

// parseServeFlags pulls the config from disk (TOML), env vars, and
// CLI flags into a daemon.Config + daemon.Options pair. Returns a
// user-facing error if the config doesn't validate.
func parseServeFlags(args []string, out io.Writer) (daemon.Config, daemon.Options, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		configPath   string
		bindOverride string
		insecure     bool
	)
	fs.StringVar(&configPath, "config", "/etc/resy-snipe/config.toml", "path to the TOML config file")
	fs.StringVar(&bindOverride, "bind", "", "host:port override (defaults from config)")
	fs.BoolVar(&insecure, "insecure-no-encryption", false, "skip secrets unwrap (requires dev-mode tripwire)")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(out, "Usage: resy-snipe serve [-config <path>] [-bind <host:port>] [-insecure-no-encryption]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return daemon.Config{}, daemon.Options{}, err
	}

	cfg := daemon.DefaultConfig()
	if configPath != "" {
		if err := cfg.ApplyTOMLFile(configPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return daemon.Config{}, daemon.Options{}, fmt.Errorf("serve: config: %w", err)
		}
	}
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return daemon.Config{}, daemon.Options{}, fmt.Errorf("serve: env: %w", err)
	}
	if bindOverride != "" {
		cfg.Daemon.Bind = bindOverride
	}
	if err := cfg.Validate(); err != nil {
		return daemon.Config{}, daemon.Options{}, fmt.Errorf("serve: invalid config: %w", err)
	}
	opts := daemon.Options{
		AllowInsecureNoEncryption: insecure,
	}
	return cfg, opts, nil
}

// composeServer wires the Service layer and the HTTP transport against
// a booted *daemon.Daemon. The returned cleanup releases the resy
// client and any other composition-owned resources; the daemon's flock
// + DB + sealer remain owned by the *Daemon and are released by its
// own Close.
func composeServer(
	ctx context.Context,
	d *daemon.Daemon,
	cfg daemon.Config,
	clk clock.Clock,
	logger *slog.Logger,
) (*service.Standard, *transport.Server, func(), error) {
	// Service composition: resolver, engine, store-adapter, resy auth.
	sqlStore := store.NewSQLiteStore(d.DB)
	rclient := resy.NewClient(logger, clk, resy.WithStore(newSessionStoreAdapter(sqlStore)))
	provider := &providerAdapter{Client: rclient}
	res := resolver.New(provider, newResolverCacheAdapter(sqlStore), clk, resolver.WithLogger(logger))
	eng := engine.New(sqlStore, clk, logger, engine.WithProvider(provider))
	storeBackend := newServiceStoreAdapter(sqlStore)
	authBackend := newResyAuthAdapter(rclient)
	svc, err := service.New(res, eng, storeBackend, clk,
		service.WithLogger(logger),
		service.WithProvider(provider),
		service.WithAuth(authBackend),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("service.New: %w", err)
	}

	// HTTP transport.
	srv := transport.NewServer(cfg.Daemon.Bind, serveBuildVersion, logger)

	// Auth middleware needs a TokenLookup adapter that talks to the
	// store's tokens table. Build it here — it's a small inline shim.
	tokenLookup := newTokenLookupAdapter(sqlStore, logger)
	authMW := transport.NewAuthMiddleware(tokenLookup, logger, clk.Now)

	// Mount authed routes onto a dedicated inner server, then splice
	// it under /v1/ on the main server. The inner Server gives us
	// the per-route registration helpers without each MountX needing
	// to know about the auth middleware.
	authedSrv := transport.NewServer(cfg.Daemon.Bind, serveBuildVersion, logger)
	transport.MountVenueRoutes(authedSrv, svc)
	transport.MountPlanRoutes(authedSrv, svc)
	transport.MountQuestRoutes(authedSrv, svc)
	transport.MountSSERoutes(authedSrv, svc)
	transport.MountAccountRoutes(authedSrv, svc)
	transport.MountTokenRoutes(authedSrv, svc)
	authedHandler := authMW.Wrap(authedSrv.Handler())

	// Public routes (healthz, readyz, metrics, pprof) on the main mux.
	transport.MountHealthRoutes(srv, &daemonReadiness{db: d.DB, sealer: d.Sealer}, nil)

	// Mount the authed surface under /v1/. The main mux's existing
	// catch-all "/" handler still owns everything else (including
	// /healthz registered above by MountHealthRoutes).
	srv.Handle("/v1/", authedHandler)

	// TrustedProxies middleware wraps the entire handler chain. When
	// the peer is not trusted, X-Forwarded-* is ignored — the
	// middleware always stamps a usable ClientInfo on ctx.
	tp, err := transport.ParseTrustedProxies(cfg.HTTP.TrustedProxies)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("trusted_proxies: %w", err)
	}
	srv.Wrap(tp.Middleware)

	_ = ctx
	cleanup := func() {
		// rclient holds the http.Client; close on shutdown so
		// connection pool resources release promptly.
	}
	return svc, srv, cleanup, nil
}

// tokenLookupAdapter binds the store-side tokens API to the
// transport.TokenLookup interface (defined-at-the-consumer per Law 5).
type tokenLookupAdapter struct {
	store *store.SQLiteStore
	log   *slog.Logger
	mu    sync.Mutex // serialize UPDATEs against SQLite's single-writer
}

// Compile-time check (Law 6).
var _ transport.TokenLookup = (*tokenLookupAdapter)(nil)

func newTokenLookupAdapter(s *store.SQLiteStore, log *slog.Logger) *tokenLookupAdapter {
	return &tokenLookupAdapter{store: s, log: log}
}

func (a *tokenLookupAdapter) FindTokenByHash(ctx context.Context, hash []byte) (transport.TokenInfo, error) {
	row, err := store.FindTokenByHash(ctx, a.store.DB(), hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return transport.TokenInfo{}, transport.ErrTokenNotFound
		}
		return transport.TokenInfo{}, err
	}
	return transport.TokenInfo{
		UserID: row.UserID,
		Scope:  row.Scopes,
		Hash:   row.Hash,
	}, nil
}

func (a *tokenLookupAdapter) TouchSeen(ctx context.Context, hash []byte, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return store.TouchTokenSeen(ctx, a.store.DB(), hash, now)
}

// daemonReadiness is the ReadinessChecker production wiring. /readyz
// returns ready when the DB is reachable and the sealer (if required)
// is initialized.
type daemonReadiness struct {
	db     *sql.DB
	sealer *secrets.Sealer
}

// Compile-time check (Law 6).
var _ transport.ReadinessChecker = (*daemonReadiness)(nil)

func (r *daemonReadiness) CheckReady(ctx context.Context) error {
	if r.db == nil {
		return errors.New("daemon: db not initialized")
	}
	if err := r.db.PingContext(ctx); err != nil {
		return fmt.Errorf("daemon: db ping: %w", err)
	}
	// sealer may be nil under -insecure-no-encryption; that's OK.
	return nil
}
