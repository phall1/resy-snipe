package main

import (
	"context"
	"fmt"
	"log/slog"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/engine"
	"resy-snipe/internal/resolver"
	"resy-snipe/internal/resy"
	"resy-snipe/internal/service"
)

// resyAuthAdapter binds the *resy.Client login surface to the
// service.ResyAuthBackend interface defined at the consumer per Law
// 5. The Service does not import internal/resy directly so the
// production wiring lives here.
//
// The returned email is the upstream-normalized account identity the
// session was filed under — *resy.Client uses the email verbatim,
// so this adapter passes the caller's input back. A future M2 sealed
// secrets path may transform the input (lowercasing, trim) before
// it reaches the v1 bridge.
type resyAuthAdapter struct {
	inner *resy.Client
}

// Compile-time check (Law 6).
var _ service.ResyAuthBackend = (*resyAuthAdapter)(nil)

// newResyAuthAdapter wires the adapter. nil panics at boot — a
// missing auth backend means Login can't work, which we'd rather
// surface at startup than at first call.
func newResyAuthAdapter(c *resy.Client) *resyAuthAdapter {
	if c == nil {
		panic("newResyAuthAdapter: nil resy client")
	}
	return &resyAuthAdapter{inner: c}
}

// Login forwards to the resy adapter and returns the email the
// session was filed under (the caller-supplied email — the v1
// bridge keys on it verbatim).
func (a *resyAuthAdapter) Login(ctx context.Context, email, password string) (string, error) {
	sess, err := a.inner.Login(ctx, resy.Credentials{Email: email, Password: password})
	if err != nil {
		return "", err
	}
	return string(sess.User()), nil
}

// newStandardService bootstraps a *service.Standard wired for CLI
// use: a real *resy.Client backed by the operator's sqlite DB, the
// production resolver + engine, and the package-level store CRUD
// functions wrapped in serviceStoreAdapter. The returned cleanup
// closes the underlying *sql.DB exactly once.
//
// This is the keystone wiring helper for M1-10: any CLI subcommand
// that wants to call into the Service layer constructs one of these
// and disposes it on exit. The helper lands here so M1-18+ CLI
// quest subcommands (plan/create/list/get/cancel) can call straight
// into a fully wired Service.
func newStandardService(
	ctx context.Context,
	logger *slog.Logger,
	clk clock.Clock,
) (*service.Standard, func() error, error) {
	rclient, sqlStore, cleanup, err := openSnipeBackend(ctx, logger, clk)
	if err != nil {
		return nil, nil, fmt.Errorf("standard service: %w", err)
	}

	provider := &providerAdapter{Client: rclient}
	res := resolver.New(provider, newResolverCacheAdapter(sqlStore), clk, resolver.WithLogger(logger))
	eng := engine.New(sqlStore, clk, logger, engine.WithProvider(provider))
	storeBackend := newServiceStoreAdapter(sqlStore)
	auth := newResyAuthAdapter(rclient)

	svc, err := service.New(res, eng, storeBackend, clk,
		service.WithLogger(logger),
		service.WithProvider(provider),
		service.WithAuth(auth),
	)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("standard service: %w", err)
	}
	return svc, cleanup, nil
}
