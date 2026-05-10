package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/notify"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resy"
	"resy-snipe/internal/store"
)

// fakeAuthClient is the test seam for the cmd/ login + snipe flows. It
// captures the credentials passed to Login, can be programmed to fail
// the first call with an MFAError so we can exercise the MFA branch,
// and exposes a hand-set Session for LoadSession success and the
// store sentinels for the error paths.
type fakeAuthClient struct {
	loginCalls   int32
	mfaCalls     int32
	loginErr     error
	loginSession *resy.Session

	mfaErr     error
	mfaSession *resy.Session

	loadErr     error
	loadSession *resy.Session

	gotEmail    string
	gotPassword string
	gotChalleng string
	gotCode     string
}

func (f *fakeAuthClient) Login(_ context.Context, creds resy.Credentials) (*resy.Session, error) {
	atomic.AddInt32(&f.loginCalls, 1)
	f.gotEmail = creds.Email
	f.gotPassword = creds.Password
	if f.loginErr != nil {
		return nil, f.loginErr
	}
	return f.loginSession, nil
}

func (f *fakeAuthClient) CompleteMFA(_ context.Context, challenge, code string) (*resy.Session, error) {
	atomic.AddInt32(&f.mfaCalls, 1)
	f.gotChalleng = challenge
	f.gotCode = code
	if f.mfaErr != nil {
		return nil, f.mfaErr
	}
	return f.mfaSession, nil
}

func (f *fakeAuthClient) LoadSession(_ context.Context, _ domain.UserID) (*resy.Session, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.loadSession, nil
}

// loginFixedExp is the canonical session expiry the login tests pin —
// well in the future relative to fixedNow so the rendered "expires"
// string is stable across machines.
var loginFixedExp = time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)

// makeFakeSession spins up a one-shot httptest server that returns a
// JWT with the supplied exp claim, drives resy.Client.Login through
// it, and returns the resulting *resy.Session. resy.Session has
// unexported fields, so this Login round trip is the cleanest way to
// build a typed Session from outside the resy package.
func makeFakeSession(t *testing.T, email string, exp time.Time) *resy.Session {
	t.Helper()
	tok := encodeFakeJWT(t, exp)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"token":%q}`, tok)
	}))
	t.Cleanup(srv.Close)

	c := resy.NewClient(slog.New(slog.DiscardHandler), clock.NewFake(fixedNow),
		resy.WithBaseURL(srv.URL), resy.WithAPIKey("test"), resy.WithHTTPClient(srv.Client()))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sess, err := c.Login(ctx, resy.Credentials{Email: email, Password: "pw"})
	if err != nil {
		t.Fatalf("makeFakeSession Login: %v", err)
	}
	return sess
}

// encodeFakeJWT builds a parseable but unsigned JWT carrying the
// supplied exp. Resy's adapter doesn't verify the signature, only the
// payload; an unsigned three-part token is indistinguishable from a
// real one for our parser.
func encodeFakeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	type claims struct {
		Exp int64 `json:"exp"`
	}
	body, err := json.Marshal(claims{Exp: exp.Unix()})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(body) + ".sig"
}

func TestRunLogin_Success(t *testing.T) {
	t.Parallel()
	sess := makeFakeSession(t, "user@example.com", loginFixedExp)
	c := &fakeAuthClient{loginSession: sess}

	stdin := strings.NewReader("user@example.com\npw-secret\n")
	var out bytes.Buffer
	if err := runLogin(context.Background(), stdin, &out, c); err != nil {
		t.Fatalf("runLogin: %v", err)
	}
	if c.gotEmail != "user@example.com" {
		t.Errorf("email passed to Login = %q", c.gotEmail)
	}
	if c.gotPassword != "pw-secret" {
		t.Errorf("password passed to Login = %q", c.gotPassword)
	}
	if got := out.String(); !strings.Contains(got, "Logged in as user@example.com") {
		t.Errorf("confirmation missing: %q", got)
	}
	if c.loginCalls != 1 {
		t.Errorf("login calls=%d", c.loginCalls)
	}
	if c.mfaCalls != 0 {
		t.Errorf("MFA path should not have fired; mfa calls=%d", c.mfaCalls)
	}
}

func TestRunLogin_MFAStubSurfacesNotImplemented(t *testing.T) {
	t.Parallel()
	// The Phase 1 spec is explicit: CompleteMFA returns
	// "not implemented" — runLogin must propagate that message
	// verbatim so the user sees an actionable diagnostic rather
	// than a silent retry loop.
	mfaErr := &resy.MFAError{Challenge: "chal-xyz"}
	c := &fakeAuthClient{
		loginErr: mfaErr,
		mfaErr:   errors.New("resy.CompleteMFA: not implemented in Phase 1"),
	}

	stdin := strings.NewReader("user@example.com\npw\n123456\n")
	var out bytes.Buffer
	err := runLogin(context.Background(), stdin, &out, c)
	if err == nil {
		t.Fatal("expected an error when CompleteMFA stub fires")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("err missing 'not implemented': %v", err)
	}
	if c.gotChalleng != "chal-xyz" {
		t.Errorf("challenge token: %q", c.gotChalleng)
	}
	if c.gotCode != "123456" {
		t.Errorf("code: %q", c.gotCode)
	}
	if c.mfaCalls != 1 {
		t.Errorf("MFA call count=%d", c.mfaCalls)
	}
}

func TestRunLogin_MFASuccessRendersConfirmation(t *testing.T) {
	t.Parallel()
	// Even though the Phase 1 stub returns not-implemented, the
	// runLogin code path that handles a *successful* MFA completion
	// is the one Phase 2 will exercise — covering it here pins the
	// rendering / branching so the Phase 2 swap is a one-line change.
	mfaErr := &resy.MFAError{Challenge: "chal-xyz"}
	sess := makeFakeSession(t, "user@example.com", loginFixedExp)
	c := &fakeAuthClient{
		loginErr:   mfaErr,
		mfaSession: sess,
	}

	stdin := strings.NewReader("user@example.com\npw\n123456\n")
	var out bytes.Buffer
	if err := runLogin(context.Background(), stdin, &out, c); err != nil {
		t.Fatalf("runLogin: %v", err)
	}
	if !strings.Contains(out.String(), "Logged in as user@example.com") {
		t.Errorf("confirmation missing: %q", out.String())
	}
}

func TestRunLogin_RejectsEmptyEmail(t *testing.T) {
	t.Parallel()
	c := &fakeAuthClient{}
	stdin := strings.NewReader("\n")
	var out bytes.Buffer
	err := runLogin(context.Background(), stdin, &out, c)
	if err == nil || !strings.Contains(err.Error(), "email") {
		t.Fatalf("expected email-required error, got %v", err)
	}
	if c.loginCalls != 0 {
		t.Errorf("Login should not be called; calls=%d", c.loginCalls)
	}
}

func TestRunLogin_RejectsEmptyPassword(t *testing.T) {
	t.Parallel()
	c := &fakeAuthClient{}
	stdin := strings.NewReader("u@x.io\n\n")
	var out bytes.Buffer
	err := runLogin(context.Background(), stdin, &out, c)
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("expected password-required error, got %v", err)
	}
}

func TestLoadSessionForSnipe_LoadsPersistedSession(t *testing.T) {
	t.Parallel()
	sess := makeFakeSession(t, "user@example.com", loginFixedExp)
	c := &fakeAuthClient{loadSession: sess}

	got, err := loadSessionForSnipe(context.Background(), c, "user@example.com",
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("loadSessionForSnipe: %v", err)
	}
	if got.User() != "user@example.com" {
		t.Errorf("user round-trip: %q", got.User())
	}
}

func TestLoadSessionForSnipe_ExpiredSurfacesActionableMessage(t *testing.T) {
	t.Parallel()
	c := &fakeAuthClient{
		loadErr: fmt.Errorf("session %s/%s exp ...: %w",
			"u", "resy", store.ErrSessionExpired),
	}
	_, err := loadSessionForSnipe(context.Background(), c, "u",
		slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected an error on expired session")
	}
	if !errors.Is(err, errNoSession) {
		t.Errorf("expected errNoSession, got %v", err)
	}
	if !strings.Contains(err.Error(), "resy-snipe login") {
		t.Errorf("err must point user at login subcommand: %v", err)
	}
}

func TestLoadSessionForSnipe_MissingSurfacesActionableMessage(t *testing.T) {
	t.Parallel()
	c := &fakeAuthClient{
		loadErr: fmt.Errorf("session: %w", store.ErrNotFound),
	}
	_, err := loadSessionForSnipe(context.Background(), c, "u",
		slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected an error on missing session")
	}
	if !errors.Is(err, errNoSession) {
		t.Errorf("expected errNoSession, got %v", err)
	}
}

func TestLoadSessionForSnipe_PassesThroughOtherErrors(t *testing.T) {
	t.Parallel()
	custom := errors.New("disk on fire")
	c := &fakeAuthClient{loadErr: custom}
	_, err := loadSessionForSnipe(context.Background(), c, "u",
		slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected error pass-through")
	}
	if !errors.Is(err, custom) {
		t.Errorf("expected wrapped %v, got %v", custom, err)
	}
	if errors.Is(err, errNoSession) {
		t.Errorf("non-sentinel store error must not be reclassified as errNoSession")
	}
}

// SessionStoreAdapter round-trips are exercised against a real
// in-memory SQLite via store.Open(":memory:"). The point is to prove
// the field-by-field translation between resy.SessionRow and
// store.SessionRow stays intact — adding a field on either side
// without updating the other will surface here.
func TestSessionStoreAdapter_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, dir+"/db.sqlite")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	sqlStore := store.NewSQLiteStore(db)
	adapter := newSessionStoreAdapter(sqlStore)
	seedUser(t, sqlStore, "u@x.io")

	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	exp := loginFixedExp
	in := resy.SessionRow{
		UserID:    "u@x.io",
		Provider:  "resy",
		JWT:       "tok-xyz",
		ExpiresAt: exp,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := adapter.UpsertSession(ctx, in); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	got, err := adapter.GetSession(ctx, "u@x.io", "resy", now)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.JWT != in.JWT || !got.ExpiresAt.Equal(in.ExpiresAt) {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, in)
	}

	// Past-exp probe surfaces ErrSessionExpired (wrapped).
	_, err = adapter.GetSession(ctx, "u@x.io", "resy", exp.Add(time.Hour))
	if !errors.Is(err, store.ErrSessionExpired) {
		t.Errorf("expected ErrSessionExpired, got %v", err)
	}
}

// TestRunDispatchesLoginSubcommand exercises the run() seam with
// args[0]="login" to confirm the dispatch routes to the login flow.
// Because runLogin opens the real default DB path, we run this as an
// end-to-end smoke against a temp HOME to keep the test hermetic.
//
// Stdin closes immediately so promptRaw returns io.EOF on the first
// read; we only check that the dispatch reached runLogin (which then
// errors on missing email) rather than exercising the snipe path.
func TestRun_DispatchesLoginSubcommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	var out bytes.Buffer
	err := run([]string{"login"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	// Empty stdin → empty email → "email is required". Reaching this
	// error proves dispatch went into runLogin (the snipe path would
	// have produced a parseFlags / intent error instead).
	if err == nil {
		t.Fatal("expected an error from empty-stdin login")
	}
	if !strings.Contains(err.Error(), "email") &&
		!strings.Contains(err.Error(), "EOF") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRun_SnipeWithUserFlag_ExpiredSession exercises the snipe path
// with the -user flag set: when the persisted session for that user
// is expired, run() must surface the actionable
// "run 'resy-snipe login' first" message. This is the second half of
// the C3 acceptance criterion.
func TestRun_SnipeWithUserFlag_ExpiredSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	// Seed the DB with an expired session for the user we'll target.
	ctx := context.Background()
	dbPath := dir + "/resy-snipe/db.sqlite"
	mkdirAllForTest(t, dir+"/resy-snipe")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := store.NewSQLiteStore(db)
	seedUser(t, s, "u@x.io")
	pastExp := fixedNow.Add(-24 * time.Hour)
	if err := s.UpsertSession(ctx, store.SessionRow{
		UserID: "u@x.io", Provider: "resy", JWT: "stale.jwt.tok",
		ExpiresAt: pastExp, CreatedAt: pastExp, UpdatedAt: pastExp,
	}); err != nil {
		t.Fatalf("seed UpsertSession: %v", err)
	}
	_ = db.Close()

	// Use a snipe time + user; run() should bail out before doing
	// anything snipe-y because LoadSession sees an expired row.
	args := []string{
		"-snipe-time", "00:00",
		"-user", "u@x.io",
	}
	var out bytes.Buffer
	err = run(args, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected expired-session error from run()")
	}
	if !errors.Is(err, errNoSession) {
		t.Errorf("expected errNoSession, got %v", err)
	}
}

// TestRun_SnipeWithUserFlag_LoadsPersistedSession is the happy-path
// half of the same acceptance criterion: when a valid (non-expired)
// session is in the store, the snipe path proceeds without error.
//
// We stub runSnipeFn so the test stays focused on the session-load
// semantics. The full engine wiring is exercised in snipe_test.go
// (TestRunSnipe_EndToEnd) where a fake provider drives a deterministic
// booking race.
func TestRun_SnipeWithUserFlag_LoadsPersistedSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	ctx := context.Background()
	dbPath := dir + "/resy-snipe/db.sqlite"
	mkdirAllForTest(t, dir+"/resy-snipe")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := store.NewSQLiteStore(db)
	seedUser(t, s, "u@x.io")
	futureExp := fixedNow.Add(24 * time.Hour)
	if err := s.UpsertSession(ctx, store.SessionRow{
		UserID: "u@x.io", Provider: "resy", JWT: "fresh.jwt.tok",
		ExpiresAt: futureExp, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}); err != nil {
		t.Fatalf("seed UpsertSession: %v", err)
	}
	_ = db.Close()

	args := []string{
		"-snipe-time", "00:00",
		"-user", "u@x.io",
	}

	prev := runSnipeFn
	t.Cleanup(func() { runSnipeFn = prev })
	var called atomic.Int32
	runSnipeFn = func(_ context.Context, _ domain.Intent, sess providers.Session, _ store.Store, _ providers.Provider, _ notify.Notifier, _ *slog.Logger, _ clock.Clock) (domain.Status, error) {
		called.Store(1)
		if sess == nil {
			t.Error("runSnipeFn: nil session — load failed silently")
		}
		return domain.StatusBooked, nil
	}

	var out bytes.Buffer
	if err := run(args, strings.NewReader(""), io.Discard, clock.NewFake(fixedNow)); err != nil {
		t.Fatalf("run with valid session: %v (out=%q)", err, out.String())
	}
	if called.Load() != 1 {
		t.Error("runSnipeFn was not invoked — snipe path skipped")
	}
}

// mkdirAllForTest is a tiny helper used by the run-level tests so the
// XDG-home → resy-snipe/ directory exists before store.Open creates
// the file. store.Open will MkdirAll on its own; this helper exists
// only to make the test ordering legible.
func mkdirAllForTest(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
}

// seedUser pre-creates an `accounts` row so a subsequent
// UpsertSession's FK lookup (sessions.account_id -> accounts.id)
// succeeds. Pre-v2 (migration 0002_v2_multi_user) this inserted into
// `users`; v2 moved the Resy-login table to `accounts` and reserved
// `users` for homelab tenants. The legacy v1 UserID (a Resy email)
// is parked on `accounts.resy_email` with NULL user_id until M1-16
// adopts these rows under the operator. The Store's session bridge
// (internal/store/sqlite.go UpsertSession et al.) reaches the row by
// resy_email.
func seedUser(t *testing.T, s *store.SQLiteStore, id string) {
	t.Helper()
	if _, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
		 VALUES (?, NULL, ?, 'legacy-v1', 0)`,
		"acct_legacy_"+id, id); err != nil {
		t.Fatalf("seed user %q: %v", id, err)
	}
}
