package http_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	transport "resy-snipe/internal/transport/http"
)

// readyAlways is a ReadinessChecker stub that always returns nil.
type readyAlways struct{}

func (readyAlways) CheckReady(_ context.Context) error { return nil }

// readyFails is a ReadinessChecker stub that returns a fixed error.
type readyFails struct{ msg string }

func (r readyFails) CheckReady(_ context.Context) error { return errors.New(r.msg) }

// newHealthServer builds a test Server with health routes mounted
// and returns a started httptest.Server. The BuiltinMetrics
// fallback is exercised by passing nil for the MetricsProvider; the
// only assertion this test file makes about metrics output is the
// build_info line, which BuiltinMetrics emits.
func newHealthServer(t *testing.T, ready transport.ReadinessChecker) *httptest.Server {
	t.Helper()
	s := transport.NewServer("127.0.0.1:0", "2.0.0-test", nil)
	transport.MountHealthRoutes(s, ready, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// doGet issues a GET against ts.URL+path and returns the response.
// Caller must close resp.Body. Centralizes the request-build
// boilerplate (NewRequestWithContext + ts.Client().Do) so individual
// assertions stay focused on the protocol semantics they test.
func doGet(t *testing.T, ts *httptest.Server, path string) *stdhttp.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, ts.URL+path, stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest %s: %v", path, err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// decodeJSONStatus reads the body, decodes it as map[string]string,
// and asserts that body["status"] matches wantStatus. Returns the
// decoded map so the caller can make further assertions.
func decodeJSONStatus(t *testing.T, resp *stdhttp.Response, wantStatus string) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != wantStatus {
		t.Errorf("status=%q want %q", body["status"], wantStatus)
	}
	return body
}

func TestHealthzReturns200Unauthenticated(t *testing.T) {
	t.Parallel()
	ts := newHealthServer(t, readyAlways{})
	resp := doGet(t, ts, "/healthz")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != stdhttp.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	decodeJSONStatus(t, resp, "ok")
}

func TestReadyzOKWhenCheckerPasses(t *testing.T) {
	t.Parallel()
	ts := newHealthServer(t, readyAlways{})
	resp := doGet(t, ts, "/readyz")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != stdhttp.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	decodeJSONStatus(t, resp, "ready")
}

func TestReadyz503WhenCheckerFails(t *testing.T) {
	t.Parallel()
	ts := newHealthServer(t, readyFails{msg: "db unreachable"})
	resp := doGet(t, ts, "/readyz")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != stdhttp.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", resp.StatusCode)
	}
	body := decodeJSONStatus(t, resp, "not_ready")
	if !strings.Contains(body["reason"], "db unreachable") {
		t.Errorf("reason=%q should contain checker error", body["reason"])
	}
}

func TestMetricsReturnsBuildInfoOverLoopback(t *testing.T) {
	t.Parallel()
	ts := newHealthServer(t, readyAlways{})
	resp := doGet(t, ts, "/metrics")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != stdhttp.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type=%q want text/plain prefix", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, `resy_snipe_build_info{version="2.0.0-test"} 1`) {
		t.Errorf("missing build_info line; body=%q", bodyStr)
	}
	if !strings.Contains(bodyStr, "resy_snipe_uptime_seconds") {
		t.Errorf("missing uptime line; body=%q", bodyStr)
	}
}

// assertForbiddenOffLoopback drives the server's handler chain
// directly with a spoofed RemoteAddr (httptest.NewServer always
// dials 127.0.0.1, so the loopback gate would always pass from a
// live client). The Handler chain is the production composition.
func assertForbiddenOffLoopback(t *testing.T, path string) {
	t.Helper()
	s := transport.NewServer("127.0.0.1:0", "test", nil)
	transport.MountHealthRoutes(s, readyAlways{}, nil)

	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, path, stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest %s: %v", path, err)
	}
	req.RemoteAddr = "203.0.113.7:54321"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != stdhttp.StatusForbidden {
		t.Fatalf("%s status=%d want 403", path, rr.Code)
	}
	var env transport.Envelope
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if env.Code != "forbidden" {
		t.Errorf("%s code=%q want forbidden", path, env.Code)
	}
}

func TestMetricsForbiddenOffLoopback(t *testing.T) {
	t.Parallel()
	assertForbiddenOffLoopback(t, "/metrics")
}

func TestPprofCmdlineOverLoopback(t *testing.T) {
	t.Parallel()
	ts := newHealthServer(t, readyAlways{})
	resp := doGet(t, ts, "/debug/pprof/cmdline")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != stdhttp.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
}

func TestPprofForbiddenOffLoopback(t *testing.T) {
	t.Parallel()
	assertForbiddenOffLoopback(t, "/debug/pprof/cmdline")
}

func TestReadinessCheckerFuncAdapts(t *testing.T) {
	t.Parallel()
	var called bool
	f := transport.ReadinessCheckerFunc(func(_ context.Context) error {
		called = true
		return nil
	})
	if err := f.CheckReady(context.Background()); err != nil {
		t.Fatalf("CheckReady: %v", err)
	}
	if !called {
		t.Error("adapter did not invoke the wrapped func")
	}
}
