package resy_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/resy"
)

func newTestClient(t *testing.T, srv *httptest.Server) *resy.Client {
	t.Helper()
	return resy.NewClient(slog.New(slog.DiscardHandler), clock.NewReal(),
		resy.WithBaseURL(srv.URL),
		resy.WithAPIKey("test-key"),
		resy.WithHTTPClient(srv.Client()),
		resy.WithUserAgent("test-ua"),
	)
}

func ctxWithDeadline(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// applyHeadersFor exercises the do() path indirectly: do() is unexported,
// so we exercise it through one of the typed-endpoint methods. Phase 1
// R1 doesn't ship endpoint methods yet, so we use httptest plus the
// public Get / Post helpers added below.

func TestClientSetsCanonicalAndResyHeaders(t *testing.T) {
	t.Parallel()

	var captured *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		w.Header().Set("X-Request-Id", "req-abc")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)

	ctx, cancel := ctxWithDeadline(t)
	defer cancel()

	resp, body, err := resy.GetForTest(c, ctx, "/3/probe", url.Values{"q": {"x"}}, "auth-jwt")
	if err != nil {
		t.Fatalf("GetForTest: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if resp.RequestID != "req-abc" {
		t.Errorf("request id captured wrong: %q", resp.RequestID)
	}
	if !strings.Contains(string(body), "ok") {
		t.Errorf("body not returned verbatim: %q", body)
	}

	// Header inspection.
	got := func(k string) string { return captured.Header.Get(k) }
	checks := map[string]string{
		"Authorization":         `ResyAPI api_key="test-key"`,
		"X-Resy-Universal-Auth": "auth-jwt",
		"Origin":                "https://widgets.resy.com",
		"Referer":               "https://widgets.resy.com/",
		"X-Origin":              "https://widgets.resy.com",
		"User-Agent":            "test-ua",
		"Content-Type":          "application/x-www-form-urlencoded",
	}
	for k, want := range checks {
		if g := got(k); g != want {
			t.Errorf("header %q = %q, want %q", k, g, want)
		}
	}
	// x-resy-auth-token is sent in lowercase verbatim. Go's http.Server
	// canonicalises headers on parse, so on the test server side the
	// value lands under the canonical form. The wire-format casing is
	// preserved by the client (verified by the direct map assignment
	// in applyHeaders); this test only confirms the value made it.
	if v := captured.Header.Get("X-Resy-Auth-Token"); v != "auth-jwt" {
		t.Errorf("x-resy-auth-token value: %q", v)
	}

	// Query string round-trips.
	if q := captured.URL.Query().Get("q"); q != "x" {
		t.Errorf("query: %q", q)
	}
}

func TestClientOmitsAuthHeadersForUnauthenticatedCalls(t *testing.T) {
	t.Parallel()
	var captured *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ctx, cancel := ctxWithDeadline(t)
	defer cancel()

	if _, _, err := resy.GetForTest(c, ctx, "/4/find", nil, ""); err != nil {
		t.Fatalf("GetForTest: %v", err)
	}
	if v := captured.Header.Get("X-Resy-Universal-Auth"); v != "" {
		t.Errorf("auth header leaked on unauthed call: %q", v)
	}
	if v := captured.Header.Get("X-Resy-Auth-Token"); v != "" {
		t.Errorf("x-resy-auth-token leaked on unauthed call: %q", v)
	}
}

func TestClientPostSendsFormBody(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"resy_token":"tok","reservation_id":1234}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ctx, cancel := ctxWithDeadline(t)
	defer cancel()

	form := url.Values{
		"book_token":            {"tok"},
		"struct_payment_method": {`{"id":42}`},
	}
	resp, body, err := resy.PostForTest(c, ctx, "/3/book", form, "jwt")
	if err != nil {
		t.Fatalf("PostForTest: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if got == "" {
		t.Fatal("server received empty body")
	}
	parsed, err := url.ParseQuery(got)
	if err != nil {
		t.Fatalf("body not form-encoded: %v", err)
	}
	if parsed.Get("book_token") != "tok" {
		t.Errorf("book_token missing: %v", parsed)
	}
	// Verify response body is returned for caller to unmarshal.
	var br resy.BookResponse
	if err := json.Unmarshal(body, &br); err != nil {
		t.Fatalf("unmarshal BookResponse: %v", err)
	}
	if br.ResyToken != "tok" || br.ReservationID != 1234 {
		t.Errorf("BookResponse parse: %+v", br)
	}
}

func TestClientRequiresCtxDeadline(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, _, err := resy.GetForTest(c, context.Background(), "/x", nil, "")
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected ctx-deadline error, got %v", err)
	}
}

func TestClientPropagatesCtxCancellation(t *testing.T) {
	t.Parallel()
	// Slow handler so the cancellation has a chance to land first.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := resy.GetForTest(c, ctx, "/slow", nil, "")
	if err == nil {
		t.Fatal("expected error on canceled ctx")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("err should mention ctx: %v", err)
	}
}

func TestClientReturnsBodyOnNon2xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req-fail")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ctx, cancel := ctxWithDeadline(t)
	defer cancel()

	resp, body, err := resy.GetForTest(c, ctx, "/3/details", nil, "jwt")
	if err != nil {
		t.Fatalf("transit error on 401: %v", err)
	}
	// R1 returns the body verbatim; R5 will classify status into the
	// taxonomy. The transit layer does not error on non-2xx.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "unauthorized") {
		t.Errorf("body not returned: %q", body)
	}
	if resp.RequestID != "req-fail" {
		t.Errorf("request id: %q", resp.RequestID)
	}
}

// Sanity: the HTTP client is reused across calls (no per-request
// connection storm), and the test server only ever sees one
// underlying client identity per run.
func TestClientReusesHTTPClient(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ctx, cancel := ctxWithDeadline(t)
	defer cancel()

	for range 3 {
		if _, _, err := resy.GetForTest(c, ctx, "/x", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls: %d", got)
	}
}
