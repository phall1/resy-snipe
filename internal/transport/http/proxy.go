package http

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// TrustedProxies is the parsed CIDR allow-list that governs whether
// the daemon honors X-Forwarded-* headers on an inbound request. Per
// docs/v2/design/daemon.md §5 and ADR-0009, the daemon does not
// terminate TLS; a reverse proxy (Caddy, nginx, tailscale serve) sits
// in front. The proxy injects the original client identity via the
// X-Forwarded-{For,Proto,Host} triple, and the daemon honors those
// headers only when the immediate TCP peer is in the trusted list.
//
// Misconfiguring this is the most common deployment bug — the audit
// log fills with 127.0.0.1 when the operator forgets to add their
// proxy network. An empty list ("trust no proxy") is the right
// setting for direct binds with no proxy in front; in that mode every
// request is treated as direct and the forwarding headers are
// ignored.
//
// The zero value of TrustedProxies is the empty list: safe by default.
type TrustedProxies struct {
	nets []*net.IPNet
}

// defaultTrustedCIDRs is the design's recommended allow-list:
// loopback (v4 + v6) plus the RFC1918 private ranges and the IPv6
// unique-local block. Operators on a single-host install with the
// proxy on the same machine need no override.
var defaultTrustedCIDRs = []string{
	"127.0.0.0/8",
	"::1/128",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"fd00::/8",
}

// DefaultTrustedProxies returns a TrustedProxies populated with the
// design-document default CIDR list. Construction is infallible: the
// hard-coded list is validated by the package's tests.
func DefaultTrustedProxies() TrustedProxies {
	tp, err := ParseTrustedProxies(defaultTrustedCIDRs)
	if err != nil {
		// Unreachable: the defaults are constants and any drift is
		// caught by TestDefaultTrustedProxiesParseClean. Panic so a
		// future bad edit surfaces immediately rather than silently
		// degrading to "trust nothing".
		panic(fmt.Sprintf("transport/http: default trusted CIDRs failed to parse: %v", err))
	}
	return tp
}

// ParseTrustedProxies parses a slice of CIDR strings into a
// TrustedProxies value. Blank entries are skipped so a config that
// uses a trailing comma or an empty array element does not fail.
// Returns a non-nil error on the first malformed CIDR; the daemon
// surfaces that as a config validation failure at boot.
//
// A nil or empty cidrs slice yields the zero-value TrustedProxies —
// trust nothing. That is the safe default for "--bind 0.0.0.0 with no
// proxy in front" (design §5).
func ParseTrustedProxies(cidrs []string) (TrustedProxies, error) {
	if len(cidrs) == 0 {
		return TrustedProxies{}, nil
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, raw := range cidrs {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return TrustedProxies{}, fmt.Errorf("trusted_proxies: parse %q: %w", s, err)
		}
		nets = append(nets, n)
	}
	return TrustedProxies{nets: nets}, nil
}

// Trusts reports whether remoteAddr — a host:port pair in
// net/http.Request.RemoteAddr form — has a host component that falls
// inside any of the configured CIDRs. Returns false for the zero
// value (no CIDRs configured), for a malformed remoteAddr, and for an
// unparseable host. IPv6 host:port forms with bracketed addresses
// ("[::1]:1234") are handled by net.SplitHostPort.
func (tp TrustedProxies) Trusts(remoteAddr string) bool {
	if len(tp.nets) == 0 {
		return false
	}
	host := hostFromRemote(remoteAddr)
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range tp.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// hostFromRemote extracts the host component from an http.Request
// RemoteAddr string. RemoteAddr is documented as host:port, but some
// transports (httptest, Unix-domain) may pass a bare host. We accept
// both rather than failing closed on a shape mismatch.
func hostFromRemote(remoteAddr string) string {
	if remoteAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	// No port: treat the whole string as the host (after trimming any
	// IPv6 brackets a caller may have included).
	s := strings.TrimSpace(remoteAddr)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	return s
}

// ClientInfo is the per-request projection of "who is calling and how
// did they reach us" that downstream handlers (audit log, rate
// limiter, link generation) read off the request context. Populated
// once by Middleware and never mutated.
//
// IP is the client's source address as a string (no port). Proto is
// "http" or "https" depending on whether TLS was terminated upstream.
// Host is the externally-visible host the client used to reach the
// daemon — usually equal to r.Host, but rewritten when a trusted
// proxy supplied X-Forwarded-Host.
type ClientInfo struct {
	IP    string
	Proto string
	Host  string
}

// Middleware returns an http.Handler that resolves the per-request
// ClientInfo and stamps it onto the request context before delegating
// to next. Composes outside AuthMiddleware so the audit log can see
// the client identity even on auth-failed requests.
//
// Resolution rules (design §5):
//
//  1. If the immediate TCP peer (r.RemoteAddr) is NOT in the trusted
//     list, ClientInfo is taken from the request directly: IP from
//     RemoteAddr, Proto from r.URL.Scheme (falling back to "http"
//     because the daemon does not terminate TLS), Host from r.Host.
//     The X-Forwarded-* headers are ignored.
//
//  2. If the peer IS trusted, X-Forwarded-For (leftmost non-blank
//     entry, comma-separated, whitespace-trimmed) replaces IP;
//     X-Forwarded-Proto replaces Proto; X-Forwarded-Host replaces
//     Host. Each header is honored independently — a proxy that
//     supplies XFF but not XFP still gets the XFF treatment.
//
// Malformed XFF (all entries blank, leading commas, etc.) falls back
// to the TCP peer rather than panicking or stamping an empty IP.
func (tp TrustedProxies) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := tp.resolve(r)
		ctx := contextWithClientInfo(r.Context(), info)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolve computes the ClientInfo for r per the rules documented on
// Middleware. Split out so tests can exercise the decision without
// spinning up an httptest.Server.
func (tp TrustedProxies) resolve(r *http.Request) ClientInfo {
	peer := hostFromRemote(r.RemoteAddr)
	info := ClientInfo{
		IP:    peer,
		Proto: requestProto(r),
		Host:  r.Host,
	}
	if !tp.Trusts(r.RemoteAddr) {
		return info
	}
	if xff := leftmostXFF(r.Header.Get("X-Forwarded-For")); xff != "" {
		info.IP = xff
	}
	if xfp := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xfp != "" {
		info.Proto = xfp
	}
	if xfh := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); xfh != "" {
		info.Host = xfh
	}
	return info
}

// requestProto returns the wire protocol the client used to reach the
// daemon, ignoring any X-Forwarded-Proto header. r.URL.Scheme is
// populated for client-side requests but is typically empty on
// server-received requests; we fall back to "https" if r.TLS is set,
// "http" otherwise. The daemon itself does not terminate TLS (ADR-0009),
// so r.TLS will be nil in production — the fallback exists for tests
// that drive the middleware via httptest.NewTLSServer.
func requestProto(r *http.Request) string {
	if r.URL != nil && r.URL.Scheme != "" {
		return r.URL.Scheme
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// leftmostXFF parses an X-Forwarded-For header value and returns the
// leftmost non-blank entry per design §5. Entries are comma-
// separated; each entry is whitespace-trimmed. An all-blank header
// returns the empty string so the caller can fall back to the peer.
//
// The header value is otherwise opaque — we do not validate that
// each entry is a parseable IP. The audit log and rate limiter treat
// the IP as a key, not an authenticated identity; an upstream proxy
// that injects garbage gets garbage in its own audit trail.
func leftmostXFF(header string) string {
	if header == "" {
		return ""
	}
	for part := range strings.SplitSeq(header, ",") {
		s := strings.TrimSpace(part)
		if s != "" {
			return s
		}
	}
	return ""
}

// clientInfoCtxKey is the context key under which Middleware stamps
// the resolved ClientInfo. Private to this package so callers must go
// through the typed accessor — symmetrical with the caller-identity
// pattern in internal/service/ctxauth.go.
type clientInfoCtxKey struct{}

// contextWithClientInfo stamps info onto ctx. Internal: handlers must
// not stash a forged ClientInfo, only middleware may.
func contextWithClientInfo(ctx context.Context, info ClientInfo) context.Context {
	return context.WithValue(ctx, clientInfoCtxKey{}, info)
}

// ClientInfoFrom returns the ClientInfo stamped on ctx by Middleware,
// or the zero value if the context never traversed the middleware
// (e.g. a Service-layer unit test calling into a handler directly).
// Downstream code should treat a zero ClientInfo as "unknown" rather
// than as a security signal.
func ClientInfoFrom(ctx context.Context) ClientInfo {
	v, _ := ctx.Value(clientInfoCtxKey{}).(ClientInfo)
	return v
}

// ClientIP returns the client IP for the request, or the empty string
// if ctx never traversed Middleware. Sugar over ClientInfoFrom.
func ClientIP(ctx context.Context) string {
	return ClientInfoFrom(ctx).IP
}

// ClientProto returns the client-facing wire protocol ("http" or
// "https") for the request, or the empty string if ctx never
// traversed Middleware.
func ClientProto(ctx context.Context) string {
	return ClientInfoFrom(ctx).Proto
}

// ClientHost returns the externally-visible host the client used to
// reach the daemon, or the empty string if ctx never traversed
// Middleware. Link-generation code (email confirms, Location
// headers) reads this so deep links point at the public URL rather
// than the daemon's bind address.
func ClientHost(ctx context.Context) string {
	return ClientInfoFrom(ctx).Host
}
