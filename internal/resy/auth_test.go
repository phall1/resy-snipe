package resy_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resy"
)

// memSessionStore satisfies resy.SessionStore for tests. It mirrors
// store.SQLiteStore's session-table behavior: upsert overwrites and
// GetSession returns ErrSessionExpired when the exp has passed.
type memSessionStore struct {
	mu  sync.Mutex
	row *resy.SessionRow
}

var (
	errMemNotFound = errors.New("memstore: not found")
	errMemExpired  = errors.New("memstore: session expired")
)

func (m *memSessionStore) UpsertSession(_ context.Context, s resy.SessionRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := s
	m.row = &cp
	return nil
}

func (m *memSessionStore) GetSession(_ context.Context, user domain.UserID, provider domain.ProviderID, now time.Time) (resy.SessionRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row == nil {
		return resy.SessionRow{}, errMemNotFound
	}
	if m.row.UserID != user || m.row.Provider != provider {
		return resy.SessionRow{}, errMemNotFound
	}
	if !m.row.ExpiresAt.IsZero() && !m.row.ExpiresAt.After(now) {
		return *m.row, errMemExpired
	}
	return *m.row, nil
}

// makeJWT builds a fake unsigned JWT with the given exp claim. Resy
// signs its tokens, but the adapter does not verify the signature
// (only parses the payload), so an unsigned three-part token is
// indistinguishable from the real thing for our parser.
func makeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	type claims struct {
		Exp int64 `json:"exp"`
	}
	payload, err := json.Marshal(claims{Exp: exp.Unix()})
	if err != nil {
		t.Fatalf("marshal jwt claims: %v", err)
	}
	enc := func(b []byte) string {
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(payload) + ".sig"
}

func newAuthClient(t *testing.T, srv *httptest.Server, c clock.Clock, store resy.SessionStore) *resy.Client {
	t.Helper()
	opts := []resy.Option{
		resy.WithBaseURL(srv.URL),
		resy.WithAPIKey("test-key"),
		resy.WithHTTPClient(srv.Client()),
		resy.WithUserAgent("test-ua"),
	}
	if store != nil {
		opts = append(opts, resy.WithStore(store))
	}
	return resy.NewClient(slog.New(slog.DiscardHandler), c, opts...)
}

func TestLoginPersistsSessionAndParsesExp(t *testing.T) {
	t.Parallel()
	exp := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	token := makeJWT(t, exp)

	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capturedBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":99,"em_address":"u@x.io","token":%q,"payment_method":{"id":42}}`, token)
	}))
	defer srv.Close()

	store := &memSessionStore{}
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	c := newAuthClient(t, srv, clock.NewFake(now), store)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	sess, err := c.Login(ctx, resy.Credentials{Email: "u@x.io", Password: "pw"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sess.User() != "u@x.io" {
		t.Errorf("user: %q", sess.User())
	}
	if !sess.ExpiresAt().Equal(exp) {
		t.Errorf("exp: got %v want %v", sess.ExpiresAt(), exp)
	}
	if sess.JWT() != token {
		t.Errorf("token round-trip lost")
	}

	parsed, err := url.ParseQuery(capturedBody)
	if err != nil {
		t.Fatalf("body not form-encoded: %v", err)
	}
	if parsed.Get("email") != "u@x.io" || parsed.Get("password") != "pw" {
		t.Errorf("body wrong: %v", parsed)
	}

	// Persisted via store.
	stored, err := store.GetSession(ctx, "u@x.io", "resy", now)
	if err != nil {
		t.Fatalf("store.GetSession: %v", err)
	}
	if stored.JWT != token {
		t.Errorf("persisted token: %q", stored.JWT)
	}
	if !stored.ExpiresAt.Equal(exp) {
		t.Errorf("persisted exp: %v", stored.ExpiresAt)
	}
}

func TestLoginRejects401AsAuthExpired(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad creds"}`)
	}))
	defer srv.Close()

	c := newAuthClient(t, srv, clock.NewReal(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.Login(ctx, resy.Credentials{Email: "u@x.io", Password: "pw"})
	if !errors.Is(err, providers.ErrAuthExpired) {
		t.Fatalf("err = %v, want ErrAuthExpired", err)
	}
}

func TestLoginSurfacesMFAChallenge(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"mfa_required":true,"challenge_id":"chal-xyz","challenge_type":"sms"}`)
	}))
	defer srv.Close()

	c := newAuthClient(t, srv, clock.NewReal(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.Login(ctx, resy.Credentials{Email: "u@x.io", Password: "pw"})
	if !errors.Is(err, providers.ErrMFARequired) {
		t.Fatalf("err = %v, want ErrMFARequired", err)
	}
	var mfaErr *resy.MFAError
	if !errors.As(err, &mfaErr) {
		t.Fatalf("err is not *MFAError: %v", err)
	}
	if mfaErr.Challenge != "chal-xyz" {
		t.Errorf("challenge: %q", mfaErr.Challenge)
	}
}

func TestLoginRejectsMissingToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv, clock.NewReal(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.Login(ctx, resy.Credentials{Email: "u@x.io", Password: "pw"})
	if err == nil {
		t.Fatal("expected error on missing token")
	}
	if !strings.Contains(err.Error(), "no token") {
		t.Errorf("err message: %v", err)
	}
}

func TestLoginRejectsBadCredentialsType(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv, clock.NewReal(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	type fakeCreds struct{ providers.Credentials }
	_, err := c.Login(ctx, fakeCreds{})
	if err == nil {
		t.Fatal("expected error on wrong credentials type")
	}
}

func TestLoadSessionReadsFromStore(t *testing.T) {
	t.Parallel()
	exp := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	store := &memSessionStore{
		row: &resy.SessionRow{
			UserID: "u@x.io", Provider: "resy",
			JWT: "old-jwt", ExpiresAt: exp,
			CreatedAt: now, UpdatedAt: now,
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	c := newAuthClient(t, srv, clock.NewFake(now), store)

	sess, err := c.LoadSession(context.Background(), "u@x.io")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.JWT() != "old-jwt" {
		t.Errorf("jwt: %q", sess.JWT())
	}
}

func TestLoadSessionReportsExpiredViaStore(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	store := &memSessionStore{
		row: &resy.SessionRow{
			UserID: "u@x.io", Provider: "resy",
			JWT:       "stale",
			ExpiresAt: now.Add(-time.Hour),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	c := newAuthClient(t, srv, clock.NewFake(now), store)

	_, err := c.LoadSession(context.Background(), "u@x.io")
	if err == nil {
		t.Fatal("expected an error on expired session")
	}
	// memSessionStore returns its own sentinel; we just care that
	// LoadSession surfaced an error rather than returning a stale
	// session as if it were valid.
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("err message: %v", err)
	}
}

func TestPingShortCircuitsOnLocallyExpiredSession(t *testing.T) {
	t.Parallel()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	c := newAuthClient(t, srv, clock.NewFake(now), nil)

	// Build an artificially-expired session directly — Ping must
	// short-circuit on the local clock check without ever calling Login.
	expiredSess := resy.NewSessionForTest("u@x.io", "stale-jwt", now.Add(-time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Ping(ctx, expiredSess); !errors.Is(err, providers.ErrAuthExpired) {
		t.Fatalf("Ping err = %v, want ErrAuthExpired", err)
	}
	if hits != 0 {
		t.Errorf("Ping should not call the server when session is locally expired; hits=%d", hits)
	}
}

func TestPingClassifies401AsAuthExpired(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/auth/password":
			tok := makeJWT(t, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
			_, _ = fmt.Fprintf(w, `{"token":%q}`, tok)
		case "/2/user":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newAuthClient(t, srv, clock.NewFake(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)), nil)
	sess := loginViaServer(t, c, time.Time{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := c.Ping(ctx, sess)
	if !errors.Is(err, providers.ErrAuthExpired) {
		t.Fatalf("err = %v, want ErrAuthExpired", err)
	}
}

func TestPingSucceedsOn200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/auth/password":
			tok := makeJWT(t, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
			_, _ = fmt.Fprintf(w, `{"token":%q}`, tok)
		case "/2/user":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newAuthClient(t, srv, clock.NewFake(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)), nil)
	sess := loginViaServer(t, c, time.Time{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Ping(ctx, sess); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestCompleteMFAStubBehaviour(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	c := newAuthClient(t, srv, clock.NewReal(), nil)

	if _, err := c.CompleteMFA(context.Background(), "", ""); err == nil {
		t.Fatal("empty challenge/code must error")
	}
	_, err := c.CompleteMFA(context.Background(), "chal", "123456")
	if err == nil {
		t.Fatal("Phase 1 stub must surface a not-implemented error")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("err: %v", err)
	}
}

func TestParseJWTExpRejectsMalformed(t *testing.T) {
	t.Parallel()
	// Drive through Login so we exercise the parser via the public
	// surface. A malformed token must surface a parse error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"token":"not-a-jwt"}`)
	}))
	defer srv.Close()
	c := newAuthClient(t, srv, clock.NewReal(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.Login(ctx, resy.Credentials{Email: "u", Password: "p"})
	if err == nil {
		t.Fatal("malformed JWT should fail")
	}
}

// loginViaServer drives Login against the fixture server and returns
// the typed *Session. If exp is zero, Login picks the exp from the
// server response; pass an explicit exp by wrapping the handler.
func loginViaServer(t *testing.T, c *resy.Client, customExp time.Time) *resy.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sess, err := c.Login(ctx, resy.Credentials{Email: "u@x.io", Password: "pw"})
	if err != nil {
		t.Fatalf("loginViaServer: %v", err)
	}
	if !customExp.IsZero() {
		// Caller wants an artificially-expired session; rebuild a
		// session with the custom exp using the public test helper.
		return resy.NewSessionForTest(sess.User(), sess.JWT(), customExp)
	}
	return sess
}
