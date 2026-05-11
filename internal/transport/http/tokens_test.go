package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
	transport "resy-snipe/internal/transport/http"
)

// fakeTokensBackend is a hand-rolled stub of the tokensBackend
// contract. We can't reference the unexported interface directly, but
// the handlers test through the server boundary so any backend that
// happens to satisfy the shape works. (Go duck-typing through
// MountTokenRoutes is verified by go build before this file even
// compiles.)
type fakeTokensBackend struct {
	issued    service.BearerToken
	issueErr  error
	listed    []service.Token
	listErr   error
	revokeErr error

	lastIssueUserID  domain.UserID
	lastIssueLabel   string
	lastIssueScope   string
	lastRevokeUserID domain.UserID
	lastRevokeID     string
}

func (f *fakeTokensBackend) IssueToken(_ context.Context, uid domain.UserID, label, scope string) (service.BearerToken, error) {
	f.lastIssueUserID = uid
	f.lastIssueLabel = label
	f.lastIssueScope = scope
	if f.issueErr != nil {
		return service.BearerToken{}, f.issueErr
	}
	return f.issued, nil
}

func (f *fakeTokensBackend) RevokeToken(_ context.Context, uid domain.UserID, id string) error {
	f.lastRevokeUserID = uid
	f.lastRevokeID = id
	return f.revokeErr
}

func (f *fakeTokensBackend) ListTokens(_ context.Context, _ domain.UserID) ([]service.Token, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listed, nil
}

// newTokensServer builds a server with the token routes mounted and
// wraps the whole thing in a stub auth middleware that stamps a
// fixed caller. Tests that need to vary the caller (e.g. unauthed)
// build a different scaffold.
func newTokensServer(t *testing.T, b *fakeTokensBackend, callerUID domain.UserID, callerScope string) *httptest.Server {
	t.Helper()
	s := transport.NewServer("127.0.0.1:0", "test", nil)
	transport.MountTokenRoutes(s, b)
	// Stub auth: stamp ctx with the supplied caller before delegating.
	wrap := stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		ctx := r.Context()
		if callerUID != "" {
			ctx = service.ContextWithCaller(ctx, callerUID, callerScope)
		}
		s.Handler().ServeHTTP(w, r.WithContext(ctx))
	})
	ts := httptest.NewServer(wrap)
	t.Cleanup(ts.Close)
	return ts
}

func TestIssueTokenRoute_HappyPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	b := &fakeTokensBackend{
		issued: service.BearerToken{
			ID:        "tok_aaaa0001",
			Token:     "tok_FAKEPLAIN",
			Label:     "phall-laptop",
			Scope:     "operator",
			CreatedAt: now,
		},
	}
	ts := newTokensServer(t, b, domain.UserID("usr_op"), "operator")

	body := bytes.NewBufferString(`{"label":"phall-laptop","scope":"operator"}`)
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodPost, ts.URL+"/v1/auth/tokens", body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusCreated {
		t.Fatalf("status=%d, want 201", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["id"] != "tok_aaaa0001" || got["token"] != "tok_FAKEPLAIN" ||
		got["label"] != "phall-laptop" || got["scope"] != "operator" {
		t.Errorf("response body: %+v", got)
	}
	if b.lastIssueUserID != "usr_op" || b.lastIssueLabel != "phall-laptop" || b.lastIssueScope != "operator" {
		t.Errorf("backend call: uid=%s label=%s scope=%s",
			b.lastIssueUserID, b.lastIssueLabel, b.lastIssueScope)
	}
}

func TestIssueTokenRoute_BadJSONReturns422(t *testing.T) {
	t.Parallel()
	b := &fakeTokensBackend{}
	ts := newTokensServer(t, b, domain.UserID("usr_op"), "operator")
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodPost, ts.URL+"/v1/auth/tokens",
		bytes.NewBufferString(`{not json`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422", resp.StatusCode)
	}
}

func TestIssueTokenRoute_ForbiddenMapsTo403(t *testing.T) {
	t.Parallel()
	b := &fakeTokensBackend{issueErr: errors.New("wrap: " + service.ErrForbidden.Error())}
	// Wrap properly via fmt-style — but easier: use the real sentinel.
	b.issueErr = service.ErrForbidden
	ts := newTokensServer(t, b, domain.UserID("usr_u"), "user")
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodPost, ts.URL+"/v1/auth/tokens",
		bytes.NewBufferString(`{"label":"x","scope":"user"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusForbidden {
		t.Errorf("status=%d, want 403", resp.StatusCode)
	}
}

func TestIssueTokenRoute_NoCallerCtxReturns401(t *testing.T) {
	t.Parallel()
	b := &fakeTokensBackend{}
	ts := newTokensServer(t, b, "", "") // no caller stamped
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodPost, ts.URL+"/v1/auth/tokens",
		bytes.NewBufferString(`{"label":"x","scope":"user"}`))
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
}

func TestRevokeTokenRoute_HappyPath(t *testing.T) {
	t.Parallel()
	b := &fakeTokensBackend{}
	ts := newTokensServer(t, b, domain.UserID("usr_op"), "operator")
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodDelete,
		ts.URL+"/v1/auth/tokens/tok_aaaa0001", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusNoContent {
		t.Errorf("status=%d, want 204", resp.StatusCode)
	}
	if b.lastRevokeID != "tok_aaaa0001" || b.lastRevokeUserID != "usr_op" {
		t.Errorf("backend call: id=%s uid=%s", b.lastRevokeID, b.lastRevokeUserID)
	}
}

func TestRevokeTokenRoute_NotFoundMapsTo404(t *testing.T) {
	t.Parallel()
	b := &fakeTokensBackend{revokeErr: service.ErrNotFound}
	ts := newTokensServer(t, b, domain.UserID("usr_op"), "operator")
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodDelete,
		ts.URL+"/v1/auth/tokens/tok_missing", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
	var env transport.Envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Details["token_id"] != "tok_missing" {
		t.Errorf("details.token_id=%v, want tok_missing", env.Details["token_id"])
	}
}

func TestListTokensRoute_HappyPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	lastSeen := now.Add(time.Minute)
	revokedAt := now.Add(2 * time.Minute)
	b := &fakeTokensBackend{
		listed: []service.Token{
			{
				ID:        "tok_live_001",
				UserID:    "usr_op",
				Scope:     "operator",
				Label:     "live",
				CreatedAt: now,
				LastSeen:  &lastSeen,
			},
			{
				ID:        "tok_dead_001",
				UserID:    "usr_op",
				Scope:     "user",
				Label:     "dead",
				CreatedAt: now,
				RevokedAt: &revokedAt,
			},
		},
	}
	ts := newTokensServer(t, b, domain.UserID("usr_op"), "operator")
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, ts.URL+"/v1/auth/tokens", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var got struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Tokens) != 2 {
		t.Fatalf("rows: %d, want 2", len(got.Tokens))
	}
	// Plaintext must NEVER appear on the listing surface.
	for i, row := range got.Tokens {
		if _, ok := row["token"]; ok {
			t.Errorf("row %d leaked plaintext token: %+v", i, row)
		}
	}
	if got.Tokens[0]["id"] != "tok_live_001" || got.Tokens[0]["scope"] != "operator" {
		t.Errorf("row 0: %+v", got.Tokens[0])
	}
	if got.Tokens[1]["revoked_at"] == nil {
		t.Errorf("row 1 revoked_at: nil, want set")
	}
}
