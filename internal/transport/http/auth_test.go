package http_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/blake2b"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
	transport "resy-snipe/internal/transport/http"
)

// fakeLookup is a minimal TokenLookup for middleware tests. It maps a
// presented token plaintext (after BLAKE2b) to a TokenInfo; an unknown
// hash surfaces as ErrTokenNotFound. touches counts how often
// TouchSeen fires so the coalescer can be observed end-to-end.
type fakeLookup struct {
	byHash    map[string]transport.TokenInfo
	touches   atomic.Int64
	touchFail error
}

func (f *fakeLookup) FindTokenByHash(_ context.Context, hash []byte) (transport.TokenInfo, error) {
	info, ok := f.byHash[string(hash)]
	if !ok {
		return transport.TokenInfo{}, transport.ErrTokenNotFound
	}
	return info, nil
}

func (f *fakeLookup) TouchSeen(_ context.Context, _ []byte, _ time.Time) error {
	f.touches.Add(1)
	return f.touchFail
}

// newAuthedEchoHandler returns a /v1/echo handler that reports the
// caller userID + scope from ctx (so tests can confirm the middleware
// stamped them). Wrapped with AuthMiddleware via the supplied lookup.
func newAuthedEcho(t *testing.T, lookup transport.TokenLookup, now func() time.Time) *httptest.Server {
	t.Helper()
	s := transport.NewServer("127.0.0.1:0", "test", nil)
	mw := transport.NewAuthMiddleware(lookup, nil, now)
	s.HandleFunc("GET /v1/whoami", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		transport.WriteJSON(w, nil, stdhttp.StatusOK, map[string]string{
			"user":  string(service.CallerUserID(r.Context())),
			"scope": service.CallerScope(r.Context()),
		})
	})
	// Wrap the bare mux with auth before returning the test server.
	ts := httptest.NewServer(mw.Wrap(s.Handler()))
	t.Cleanup(ts.Close)
	return ts
}

func TestAuthMiddlewareAcceptsValidBearer(t *testing.T) {
	t.Parallel()
	plain := "tok_PLAINTEXTPLAINTEXT"
	hash := blake2b.Sum256([]byte(plain))
	lookup := &fakeLookup{
		byHash: map[string]transport.TokenInfo{
			string(hash[:]): {
				UserID: domain.UserID("usr_alice"),
				Scope:  "operator",
				Hash:   hash[:],
			},
		},
	}
	ts := newAuthedEcho(t, lookup, time.Now)

	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, ts.URL+"/v1/whoami", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, body=%s", resp.StatusCode, body)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["user"] != "usr_alice" || got["scope"] != "operator" {
		t.Errorf("ctx stamp: got %+v, want user=usr_alice scope=operator", got)
	}
	if lookup.touches.Load() != 1 {
		t.Errorf("touches: got %d, want 1 (first request always touches)", lookup.touches.Load())
	}
}

func TestAuthMiddlewareRejectsMissingHeader(t *testing.T) {
	t.Parallel()
	ts := newAuthedEcho(t, &fakeLookup{}, time.Now)
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, ts.URL+"/v1/whoami", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
	var env transport.Envelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Code != "unauthenticated" {
		t.Errorf("code=%q want unauthenticated", env.Code)
	}
}

func TestAuthMiddlewareRejectsBadScheme(t *testing.T) {
	t.Parallel()
	ts := newAuthedEcho(t, &fakeLookup{}, time.Now)
	cases := []string{
		"Basic dXNlcjpwYXNz", // wrong scheme
		"BearerNoSpace",      // missing space
		"Bearer ",            // empty token
		"   ",                // garbage
	}
	for _, header := range cases {
		req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, ts.URL+"/v1/whoami", stdhttp.NoBody)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", header)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("Do(%q): %v", header, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != stdhttp.StatusUnauthorized {
			t.Errorf("header %q: status=%d, want 401", header, resp.StatusCode)
		}
	}
}

func TestAuthMiddlewareRejectsUnknownToken(t *testing.T) {
	t.Parallel()
	ts := newAuthedEcho(t, &fakeLookup{byHash: map[string]transport.TokenInfo{}}, time.Now)
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, ts.URL+"/v1/whoami", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer tok_unknown")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
}

func TestAuthMiddlewareSchemeIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	plain := "tok_LOWERCASESCHEME"
	hash := blake2b.Sum256([]byte(plain))
	lookup := &fakeLookup{
		byHash: map[string]transport.TokenInfo{
			string(hash[:]): {UserID: domain.UserID("usr_bob"), Scope: "user", Hash: hash[:]},
		},
	}
	ts := newAuthedEcho(t, lookup, time.Now)
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, ts.URL+"/v1/whoami", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "bearer "+plain)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusOK {
		t.Errorf("status=%d, want 200 (RFC 7235 scheme match is case-insensitive)", resp.StatusCode)
	}
}

// TestAuthMiddlewareTouchSeenCoalesces fires three back-to-back
// authenticated requests with a fixed clock; the coalescer should
// emit exactly one TouchSeen call. Advancing the clock past
// touchInterval (1500ms) unlocks a second touch.
func TestAuthMiddlewareTouchSeenCoalesces(t *testing.T) {
	t.Parallel()
	plain := "tok_COALESCEME"
	hash := blake2b.Sum256([]byte(plain))
	lookup := &fakeLookup{
		byHash: map[string]transport.TokenInfo{
			string(hash[:]): {UserID: domain.UserID("usr_c"), Scope: "user", Hash: hash[:]},
		},
	}
	// Pinned clock; we advance it manually.
	var nowAtomic atomic.Int64
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	nowAtomic.Store(now.UnixNano())
	clock := func() time.Time { return time.Unix(0, nowAtomic.Load()).UTC() }
	ts := newAuthedEcho(t, lookup, clock)

	doReq := func() {
		req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, ts.URL+"/v1/whoami", stdhttp.NoBody)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+plain)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_ = resp.Body.Close()
	}
	doReq()
	doReq()
	doReq()
	if got := lookup.touches.Load(); got != 1 {
		t.Errorf("touches after 3 quick requests: got %d, want 1 (coalesced)", got)
	}
	// Jump past the touch interval — next request fires a touch.
	nowAtomic.Store(now.Add(2 * time.Second).UnixNano())
	doReq()
	if got := lookup.touches.Load(); got != 2 {
		t.Errorf("touches after clock jump: got %d, want 2", got)
	}
}

func TestAuthMiddlewareTouchSeenFailureDoesNotFailRequest(t *testing.T) {
	t.Parallel()
	plain := "tok_TOUCHFAIL"
	hash := blake2b.Sum256([]byte(plain))
	lookup := &fakeLookup{
		byHash: map[string]transport.TokenInfo{
			string(hash[:]): {UserID: domain.UserID("usr_tf"), Scope: "user", Hash: hash[:]},
		},
		touchFail: errors.New("touch wedged"),
	}
	ts := newAuthedEcho(t, lookup, time.Now)
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, ts.URL+"/v1/whoami", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusOK {
		t.Errorf("status=%d, want 200 (touch failure must not fail auth)", resp.StatusCode)
	}
}

func TestAuthMiddlewareForbiddenSentinelMapsTo403(t *testing.T) {
	t.Parallel()
	// Independent of the middleware — just exercise the error mapper
	// to lock in 403 for ErrForbidden so /v1/users gating tests can
	// rely on it.
	m := transport.MapError(service.ErrForbidden)
	if m.Status != stdhttp.StatusForbidden {
		t.Errorf("status=%d want 403", m.Status)
	}
	if m.Code != "forbidden" {
		t.Errorf("code=%q want forbidden", m.Code)
	}
}
