package http_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"resy-snipe/internal/providers"
	"resy-snipe/internal/service"
	transport "resy-snipe/internal/transport/http"
)

func TestVersionHeaderOnEveryResponse(t *testing.T) {
	t.Parallel()
	s := transport.NewServer("127.0.0.1:0", "2.0.0-test", nil)
	s.HandleFunc("GET /v1/echo", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		transport.WriteJSON(w, nil, stdhttp.StatusOK, map[string]string{"ok": "yes"})
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	for _, path := range []string{"/v1/echo", "/does-not-exist", "/"} {
		req, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, ts.URL+path, stdhttp.NoBody)
		if err != nil {
			t.Fatalf("NewRequest %s: %v", path, err)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		v := resp.Header.Get("X-Resy-Snipe-Version")
		if v != "2.0.0-test" {
			t.Errorf("%s: X-Resy-Snipe-Version=%q want 2.0.0-test", path, v)
		}
		_ = resp.Body.Close()
	}
}

func TestUnknownRouteReturns404Envelope(t *testing.T) {
	t.Parallel()
	s := transport.NewServer("127.0.0.1:0", "test", nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, ts.URL+"/v1/no-such-route", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
	var env transport.Envelope
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, body)
	}
	if env.Code != "not_found" {
		t.Errorf("code=%q want not_found", env.Code)
	}
	if env.Details["path"] != "/v1/no-such-route" {
		t.Errorf("details.path=%v want /v1/no-such-route", env.Details["path"])
	}
}

func TestMapErrorSentinels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unauthorized", service.ErrUnauthorized, stdhttp.StatusUnauthorized, "unauthenticated"},
		{"token_expired", service.ErrTokenExpired, stdhttp.StatusUnauthorized, "token_expired"},
		{"token_revoked", service.ErrTokenRevoked, stdhttp.StatusUnauthorized, "token_revoked"},
		{"not_found", service.ErrNotFound, stdhttp.StatusNotFound, "not_found"},
		{"venue_not_found", service.ErrVenueNotFound, stdhttp.StatusNotFound, "venue_not_found"},
		{"conflict", service.ErrConflict, stdhttp.StatusConflict, "conflict"},
		{"venue_ambiguous", service.ErrVenueAmbiguous, stdhttp.StatusConflict, "venue_ambiguous"},
		{"idempotency_conflict", service.ErrIdempotencyConflict, stdhttp.StatusConflict, "idempotency_conflict"},
		{"invite_consumed", service.ErrInviteConsumed, stdhttp.StatusConflict, "invite_consumed"},
		{"invite_expired", service.ErrInviteExpired, stdhttp.StatusGone, "invite_expired"},
		{"invalid_argument", service.ErrInvalidArgument, stdhttp.StatusUnprocessableEntity, "validation_failed"},
		{"plan_hash", service.ErrInvalidPlanHash, stdhttp.StatusUnprocessableEntity, "plan_hash_mismatch"},
		{"rate_limited", providers.ErrRateLimited, stdhttp.StatusTooManyRequests, "rate_limited"},
		{"auth_expired", providers.ErrAuthExpired, stdhttp.StatusBadGateway, "provider_session_expired"},
		{"upstream", service.ErrUpstreamUnavailable, stdhttp.StatusBadGateway, "provider_unavailable"},
		{"random", errors.New("random untyped"), stdhttp.StatusInternalServerError, "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := transport.MapError(tc.err)
			if m.Status != tc.status {
				t.Errorf("status=%d want %d", m.Status, tc.status)
			}
			if m.Code != tc.code {
				t.Errorf("code=%q want %q", m.Code, tc.code)
			}
		})
	}
}

func TestWriteErrorPreservesWrappedSentinel(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	wrapped := fmt.Errorf("looking up quest %s: %w", "q_abc", service.ErrNotFound)
	transport.WriteError(rr, nil, wrapped)
	if rr.Code != stdhttp.StatusNotFound {
		t.Fatalf("status=%d want 404", rr.Code)
	}
	var env transport.Envelope
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != "not_found" {
		t.Errorf("code=%q want not_found", env.Code)
	}
	if !strings.Contains(env.Message, "q_abc") {
		t.Errorf("message=%q should preserve wrapped detail", env.Message)
	}
}

func TestWriteError500SuppressesMessage(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	transport.WriteError(rr, nil, errors.New("ssh keys leaked: /etc/passwd"))
	if rr.Code != stdhttp.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rr.Code)
	}
	var env transport.Envelope
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != "internal" {
		t.Errorf("code=%q want internal", env.Code)
	}
	if strings.Contains(env.Message, "ssh keys") || strings.Contains(env.Message, "/etc/passwd") {
		t.Errorf("500 leaked underlying error: %q", env.Message)
	}
}

func TestServerRunShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()
	s := transport.NewServer("127.0.0.1:0", "test", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// 127.0.0.1:0 is OS-assigned; we don't need the port for this test
	// because we cancel ctx before issuing any request.
	go func() { done <- s.Run(ctx, 100*time.Millisecond) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on context cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within drain window")
	}
}
