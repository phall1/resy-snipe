package http_test

import (
	"context"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	transport "resy-snipe/internal/transport/http"
)

// Header names used by echoClientInfo. The canonicalheader linter
// flags non-canonical strings; defining the canonical forms once
// keeps the test body readable and the linter happy.
const (
	hdrEchoIP    = "X-Echo-Ip"
	hdrEchoProto = "X-Echo-Proto"
	hdrEchoHost  = "X-Echo-Host"
)

// echoClientInfo is a tiny handler that copies the ctx-stamped
// ClientInfo back into the response headers so tests can assert on
// the middleware's resolution without parsing a JSON body.
func echoClientInfo(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	info := transport.ClientInfoFrom(r.Context())
	w.Header().Set(hdrEchoIP, info.IP)
	w.Header().Set(hdrEchoProto, info.Proto)
	w.Header().Set(hdrEchoHost, info.Host)
	w.WriteHeader(stdhttp.StatusOK)
}

// runMiddleware drives a synthetic request through tp.Middleware and
// returns the recorded response. RemoteAddr is set explicitly so the
// "is the peer trusted" decision is deterministic; the test server is
// not involved.
func runMiddleware(t *testing.T, tp transport.TrustedProxies, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), stdhttp.MethodGet, "http://daemon.local/v1/whoami", stdhttp.NoBody)
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	tp.Middleware(stdhttp.HandlerFunc(echoClientInfo)).ServeHTTP(rr, req)
	return rr
}

func TestDefaultTrustedProxiesParseClean(t *testing.T) {
	t.Parallel()
	tp := transport.DefaultTrustedProxies()
	if !tp.Trusts("127.0.0.1:5555") {
		t.Errorf("default list should trust loopback")
	}
	if !tp.Trusts("[::1]:5555") {
		t.Errorf("default list should trust IPv6 loopback")
	}
	if !tp.Trusts("10.4.5.6:80") {
		t.Errorf("default list should trust RFC1918 10.0.0.0/8")
	}
	if tp.Trusts("8.8.8.8:80") {
		t.Errorf("default list must not trust a public IP")
	}
}

func TestParseTrustedProxiesEmpty(t *testing.T) {
	t.Parallel()
	tp, err := transport.ParseTrustedProxies(nil)
	if err != nil {
		t.Fatalf("nil cidrs: unexpected err %v", err)
	}
	if tp.Trusts("127.0.0.1:1234") {
		t.Errorf("empty list must trust nothing (got loopback trusted)")
	}
	tp2, err := transport.ParseTrustedProxies([]string{})
	if err != nil {
		t.Fatalf("empty slice: unexpected err %v", err)
	}
	if tp2.Trusts("127.0.0.1:1234") {
		t.Errorf("empty slice must trust nothing")
	}
}

func TestParseTrustedProxiesSkipsBlankEntries(t *testing.T) {
	t.Parallel()
	tp, err := transport.ParseTrustedProxies([]string{"10.0.0.0/8", "", "  "})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !tp.Trusts("10.1.2.3:1") {
		t.Errorf("10/8 should be trusted")
	}
}

func TestParseTrustedProxiesRejectsBadCIDR(t *testing.T) {
	t.Parallel()
	_, err := transport.ParseTrustedProxies([]string{"not-a-cidr"})
	if err == nil {
		t.Fatal("expected error on malformed CIDR")
	}
	_, err = transport.ParseTrustedProxies([]string{"10.0.0.0/8", "999.999.999.999/12"})
	if err == nil {
		t.Fatal("expected error on out-of-range CIDR")
	}
}

func TestTrustsHandlesIPv6Brackets(t *testing.T) {
	t.Parallel()
	tp, err := transport.ParseTrustedProxies([]string{"::1/128"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !tp.Trusts("[::1]:54321") {
		t.Errorf("bracketed IPv6 host:port should be trusted")
	}
	if !tp.Trusts("::1") {
		t.Errorf("bare IPv6 (no port) should be trusted")
	}
}

func TestTrustsRejectsGarbageRemoteAddr(t *testing.T) {
	t.Parallel()
	tp := transport.DefaultTrustedProxies()
	if tp.Trusts("") {
		t.Errorf("empty RemoteAddr must not be trusted")
	}
	if tp.Trusts("not-an-address") {
		t.Errorf("garbage RemoteAddr must not be trusted")
	}
}

func TestMiddlewareUntrustedPeerIgnoresXFF(t *testing.T) {
	t.Parallel()
	tp, err := transport.ParseTrustedProxies([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rr := runMiddleware(t, tp, "8.8.8.8:55555", map[string]string{
		"X-Forwarded-For":   "203.0.113.7",
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "evil.example",
	})
	if got := rr.Header().Get(hdrEchoIP); got != "8.8.8.8" {
		t.Errorf("untrusted peer: IP=%q want 8.8.8.8", got)
	}
	if got := rr.Header().Get(hdrEchoProto); got != "http" {
		t.Errorf("untrusted peer: Proto=%q want http", got)
	}
	if got := rr.Header().Get(hdrEchoHost); got != "daemon.local" {
		t.Errorf("untrusted peer: Host=%q want daemon.local", got)
	}
}

func TestMiddlewareTrustedPeerHonorsXFF(t *testing.T) {
	t.Parallel()
	tp := transport.DefaultTrustedProxies()
	rr := runMiddleware(t, tp, "127.0.0.1:5555", map[string]string{
		"X-Forwarded-For":   "203.0.113.9, 10.0.0.1",
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "snipe.example.com",
	})
	if got := rr.Header().Get(hdrEchoIP); got != "203.0.113.9" {
		t.Errorf("trusted peer: IP=%q want 203.0.113.9 (leftmost XFF)", got)
	}
	if got := rr.Header().Get(hdrEchoProto); got != "https" {
		t.Errorf("trusted peer: Proto=%q want https", got)
	}
	if got := rr.Header().Get(hdrEchoHost); got != "snipe.example.com" {
		t.Errorf("trusted peer: Host=%q want snipe.example.com", got)
	}
}

func TestMiddlewareTrustedPeerSkipsBlankXFFEntries(t *testing.T) {
	t.Parallel()
	tp := transport.DefaultTrustedProxies()
	// Leading commas, whitespace-only entries: the leftmost
	// *non-blank* entry wins. A pathological all-blank header falls
	// back to the TCP peer.
	rr := runMiddleware(t, tp, "127.0.0.1:5555", map[string]string{
		"X-Forwarded-For": " , ,   203.0.113.42 ,10.0.0.1",
	})
	if got := rr.Header().Get(hdrEchoIP); got != "203.0.113.42" {
		t.Errorf("blank-skip: IP=%q want 203.0.113.42", got)
	}

	rr = runMiddleware(t, tp, "127.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "   ,   ,   ",
	})
	if got := rr.Header().Get(hdrEchoIP); got != "127.0.0.1" {
		t.Errorf("all-blank XFF: IP=%q want 127.0.0.1 (peer fallback)", got)
	}
}

func TestMiddlewareTrustedPeerPartialHeaders(t *testing.T) {
	t.Parallel()
	tp := transport.DefaultTrustedProxies()
	// Only XFF set; XFP/XFH should fall back to peer-side values
	// (no TLS, request Host header).
	rr := runMiddleware(t, tp, "127.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "203.0.113.99",
	})
	if got := rr.Header().Get(hdrEchoIP); got != "203.0.113.99" {
		t.Errorf("partial: IP=%q want 203.0.113.99", got)
	}
	if got := rr.Header().Get(hdrEchoProto); got != "http" {
		t.Errorf("partial: Proto=%q want http", got)
	}
	if got := rr.Header().Get(hdrEchoHost); got != "daemon.local" {
		t.Errorf("partial: Host=%q want daemon.local", got)
	}
}

func TestMiddlewareEmptyTrustListIgnoresXFF(t *testing.T) {
	t.Parallel()
	// "trust no proxy" mode — even loopback peers are treated direct.
	var tp transport.TrustedProxies
	rr := runMiddleware(t, tp, "127.0.0.1:5555", map[string]string{
		"X-Forwarded-For":   "203.0.113.7",
		"X-Forwarded-Proto": "https",
	})
	if got := rr.Header().Get(hdrEchoIP); got != "127.0.0.1" {
		t.Errorf("zero-value tp: IP=%q want 127.0.0.1", got)
	}
	if got := rr.Header().Get(hdrEchoProto); got != "http" {
		t.Errorf("zero-value tp: Proto=%q want http", got)
	}
}

func TestMiddlewareIPv6PeerBrackets(t *testing.T) {
	t.Parallel()
	tp := transport.DefaultTrustedProxies()
	rr := runMiddleware(t, tp, "[::1]:5555", map[string]string{
		"X-Forwarded-For": "2001:db8::1",
	})
	if got := rr.Header().Get(hdrEchoIP); got != "2001:db8::1" {
		t.Errorf("IPv6 trusted peer: IP=%q want 2001:db8::1", got)
	}
}

func TestClientInfoAccessors(t *testing.T) {
	t.Parallel()
	// Zero context yields zero values, never panics.
	ctx := context.Background()
	if ip := transport.ClientIP(ctx); ip != "" {
		t.Errorf("ClientIP on bare ctx: got %q want \"\"", ip)
	}
	if p := transport.ClientProto(ctx); p != "" {
		t.Errorf("ClientProto on bare ctx: got %q want \"\"", p)
	}
	if h := transport.ClientHost(ctx); h != "" {
		t.Errorf("ClientHost on bare ctx: got %q want \"\"", h)
	}
	if info := transport.ClientInfoFrom(ctx); info != (transport.ClientInfo{}) {
		t.Errorf("ClientInfoFrom on bare ctx: got %+v want zero", info)
	}

	// Round-trip through the middleware: the accessors agree with
	// ClientInfoFrom.
	tp := transport.DefaultTrustedProxies()
	req := httptest.NewRequestWithContext(ctx, stdhttp.MethodGet, "http://daemon.local/v1/whoami", stdhttp.NoBody)
	req.RemoteAddr = "127.0.0.1:42"
	req.Header.Set("X-Forwarded-For", "203.0.113.4")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "edge.example")
	tp.Middleware(stdhttp.HandlerFunc(func(_ stdhttp.ResponseWriter, r *stdhttp.Request) {
		if got := transport.ClientIP(r.Context()); got != "203.0.113.4" {
			t.Errorf("ClientIP: got %q want 203.0.113.4", got)
		}
		if got := transport.ClientProto(r.Context()); got != "https" {
			t.Errorf("ClientProto: got %q want https", got)
		}
		if got := transport.ClientHost(r.Context()); got != "edge.example" {
			t.Errorf("ClientHost: got %q want edge.example", got)
		}
	})).ServeHTTP(httptest.NewRecorder(), req)
}
