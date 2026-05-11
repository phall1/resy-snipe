package http

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"

	"resy-snipe/internal/service"
)

// ReadinessChecker is the seam between the HTTP transport and the
// daemon-level readiness aggregation. The daemon implements this
// with composite checks (store ping, signer up, secrets unlocked,
// engine running — see design/daemon.md §6); tests inject a stub.
// Defined at the consumer per Law 5: the transport owns the
// contract, the daemon's wiring layer binds it.
//
// CheckReady is expected to honor ctx — the readyz handler attaches
// a per-request budget (1s overall ceiling; subcheck splits live
// inside the daemon's composite checker per §6: DB ping ≤ 100ms,
// signer ping ≤ 200ms). A nil return means the daemon is ready; any
// non-nil error is surfaced as the readyz `reason` field, so
// implementations should return errors whose Error() text is
// operator-facing.
type ReadinessChecker interface {
	CheckReady(ctx context.Context) error
}

// ReadinessCheckerFunc adapts a plain function to ReadinessChecker.
// Useful for the daemon's composite-check closure and for tests.
type ReadinessCheckerFunc func(ctx context.Context) error

// CheckReady implements ReadinessChecker.
func (f ReadinessCheckerFunc) CheckReady(ctx context.Context) error { return f(ctx) }

// MetricsProvider is the seam between the HTTP transport and the
// daemon's metrics registry. Production wires a Prometheus registry
// here (or a richer exposition source); this wave ships with a
// minimal built-in provider (BuiltinMetrics) that exposes process
// build_info and uptime. Defined at the consumer per Law 5.
//
// WriteMetrics is expected to emit Prometheus text-format exposition
// (https://prometheus.io/docs/instrumenting/exposition_formats/).
// Errors during write land in the operator log via the handler's
// best-effort write; the response is already committed by then.
type MetricsProvider interface {
	WriteMetrics(w io.Writer) error
}

// BuiltinMetrics is the minimal MetricsProvider this wave ships
// with. It exposes:
//
//	resy_snipe_build_info{version="..."} 1
//	resy_snipe_uptime_seconds <n>
//
// Production composes this with the daemon's richer registry; the
// daemon's metrics package will wrap or replace it. The clock seam
// (now) is injectable for tests; production passes time.Now.
type BuiltinMetrics struct {
	version string
	started time.Time
	now     func() time.Time
}

// NewBuiltinMetrics constructs a BuiltinMetrics. version is the
// build version string baked into the build_info label; an empty
// string is normalised to "dev". now is the clock seam — tests pin
// it, production passes time.Now. The started timestamp is captured
// from now() at construction.
func NewBuiltinMetrics(version string, now func() time.Time) *BuiltinMetrics {
	if version == "" {
		version = "dev"
	}
	if now == nil {
		now = time.Now
	}
	return &BuiltinMetrics{
		version: version,
		started: now(),
		now:     now,
	}
}

// WriteMetrics implements MetricsProvider. The output is plain
// Prometheus text exposition; HELP/TYPE lines accompany each metric
// so scrapers don't warn.
func (b *BuiltinMetrics) WriteMetrics(w io.Writer) error {
	uptime := b.now().Sub(b.started).Seconds()
	if uptime < 0 {
		uptime = 0
	}
	_, err := fmt.Fprintf(w,
		"# HELP resy_snipe_build_info Build version label, value is always 1.\n"+
			"# TYPE resy_snipe_build_info gauge\n"+
			"resy_snipe_build_info{version=%q} 1\n"+
			"# HELP resy_snipe_uptime_seconds Seconds since daemon start.\n"+
			"# TYPE resy_snipe_uptime_seconds gauge\n"+
			"resy_snipe_uptime_seconds %s\n",
		b.version,
		strconv.FormatFloat(uptime, 'f', 3, 64),
	)
	return err
}

// MountHealthRoutes registers /healthz, /readyz, /metrics, and the
// /debug/pprof family on s. ready is required (a nil checker panics
// at mount time rather than at first request); metrics is optional
// (a nil provider falls back to NewBuiltinMetrics with s's version
// so the endpoint still serves the build_info line).
//
// /healthz and /readyz are unauthenticated — design/daemon.md §6
// makes /readyz auth optional and notes that without a token the
// response omits user-specific counts. This wave reports only
// status/reason, so the no-auth path is the whole contract.
//
// /metrics and /debug/pprof are wrapped in LoopbackOnly: off-
// loopback requests get a 403 forbidden envelope. design/daemon.md
// §10 specifies a 404 for off-loopback pprof, but the daemon's wire
// convention is the structured envelope; forbidden is the closest
// semantic match and surfaces predictably in the audit log.
func MountHealthRoutes(s *Server, ready ReadinessChecker, metrics MetricsProvider) {
	if s == nil {
		panic("MountHealthRoutes: nil server")
	}
	if ready == nil {
		panic("MountHealthRoutes: nil ready")
	}
	if metrics == nil {
		metrics = NewBuiltinMetrics(s.version, nil)
	}

	s.HandleFunc("GET /healthz", healthzHandler(s.log))
	s.HandleFunc("GET /readyz", readyzHandler(s.log, ready))

	s.Handle("GET /metrics", LoopbackOnly(metricsHandler(s.log, metrics)))

	// pprof handlers. We use net/http/pprof's exported per-name
	// handlers directly so the daemon's mux stays isolated from
	// DefaultServeMux. /debug/pprof/ is the index; the named
	// profiles (heap, goroutine, allocs, block, mutex,
	// threadcreate) are dispatched through pprof.Index by path.
	s.Handle("GET /debug/pprof/", LoopbackOnly(http.HandlerFunc(pprof.Index)))
	s.Handle("GET /debug/pprof/cmdline", LoopbackOnly(http.HandlerFunc(pprof.Cmdline)))
	s.Handle("GET /debug/pprof/profile", LoopbackOnly(http.HandlerFunc(pprof.Profile)))
	s.Handle("GET /debug/pprof/symbol", LoopbackOnly(http.HandlerFunc(pprof.Symbol)))
	s.Handle("GET /debug/pprof/trace", LoopbackOnly(http.HandlerFunc(pprof.Trace)))
}

// healthzHandler returns the cheap liveness probe. Per design/
// daemon.md §6 the wire body is "ok"; we serve a stable JSON
// envelope so the shape matches the rest of the surface and
// scrapers can parse uniformly.
func healthzHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, log, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// readyzHandler returns the deeper readiness probe. The handler
// attaches a 1s overall ceiling to ctx; the daemon's composite
// checker is expected to split that across its internal subchecks
// per §6. On nil error, status is 200 with {"status":"ready"}. On
// any error, status is 503 with {"status":"not_ready","reason":
// "<err>"}.
func readyzHandler(log *slog.Logger, ready ReadinessChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := ready.CheckReady(ctx); err != nil {
			WriteJSON(w, log, http.StatusServiceUnavailable, map[string]string{
				"status": "not_ready",
				"reason": err.Error(),
			})
			return
		}
		WriteJSON(w, log, http.StatusOK, map[string]string{"status": "ready"})
	}
}

// metricsHandler emits the Prometheus text exposition. Content-Type
// follows the Prometheus convention (`text/plain; version=0.0.4`).
// Write errors are best-effort: by the time the encoder fails the
// status line is already on the wire, so we log and move on.
func metricsHandler(log *slog.Logger, metrics MetricsProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if err := metrics.WriteMetrics(w); err != nil && log != nil {
			log.Error("transport: metrics write", "err", err.Error())
		}
	}
}

// LoopbackOnly is the middleware that gates pprof and metrics to
// loopback peers. A peer is loopback when r.RemoteAddr's host
// parses as an IP and that IP is in 127.0.0.0/8 or ::1. Anything
// else — including unparseable RemoteAddr — falls through to the
// forbidden envelope.
//
// The middleware does not consult X-Forwarded-For. The daemon's
// trusted-proxy chain (design/daemon.md §6.3) rewrites RemoteAddr
// before this handler runs; if a proxy is misconfigured, the safe
// default is to deny.
func LoopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			WriteError(w, nil, fmt.Errorf("loopback-only endpoint: %w", service.ErrForbidden))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopback reports whether remoteAddr's host is a loopback IP.
// remoteAddr is the canonical net/http RemoteAddr form (host:port).
// An empty or unparseable address is treated as non-loopback.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// Some test transports pass a bare host without a port. Try
		// that shape too before giving up.
		host = remoteAddr
	}
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
