package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"time"

	"resy-snipe/internal/service"
)

// Server wraps net/http.Server with the daemon's wire conventions:
// the X-Resy-Snipe-Version header on every response, a structured
// 404 envelope for unknown routes, and a shutdown method that
// composes with the daemon's graceful-shutdown sequence (M2-07).
//
// Routes are registered by callers via Handle / HandleFunc before
// Run. Once Run is called, the route table is frozen.
type Server struct {
	addr    string
	version string
	log     *slog.Logger
	mux     *http.ServeMux
	srv     *http.Server

	// outerWraps is the chain of middlewares the daemon stacks
	// outside the version middleware. Registered via Wrap; applied
	// in Handler() in registration order (first-registered runs
	// outermost). Used for trusted-proxy parsing, request logging,
	// or any concern that must observe the version header on the
	// way out. Empty by default — production wiring composes the
	// chain explicitly.
	outerWraps []func(http.Handler) http.Handler
}

// NewServer constructs a Server bound to addr. addr is a host:port
// string in netip.ParseAddrPort form; the caller has already
// validated it via daemon.Config.Validate.
//
// version is the build version string baked into responses' X-Resy-
// Snipe-Version header. log is the structured logger; both are
// stored by reference, not value.
func NewServer(addr, version string, log *slog.Logger) *Server {
	mux := http.NewServeMux()
	s := &Server{
		addr:    addr,
		version: version,
		log:     log,
		mux:     mux,
	}
	mux.HandleFunc("/", s.notFound)
	return s
}

// Handle registers handler at pattern. The pattern syntax is Go's
// net/http ServeMux extended form: "POST /v1/quests", "GET
// /v1/quests/{id}", etc. (Go 1.22+). Method-prefixed patterns are
// recommended so an unsupported method returns 405 instead of being
// silently dispatched.
//
// Handlers receive a request with the daemon-version header
// already pre-set by the middleware in Run; they only need to call
// WriteJSON / WriteError.
func (s *Server) Handle(pattern string, handler http.Handler) {
	s.mux.Handle(pattern, handler)
}

// HandleFunc is the func variant of Handle.
func (s *Server) HandleFunc(pattern string, h http.HandlerFunc) {
	s.mux.HandleFunc(pattern, h)
}

// Handler returns the wrapped mux with the version middleware
// applied. Exposed so callers can mount the daemon's HTTP surface
// behind an httptest.Server in integration tests without running
// the real listener.
func (s *Server) Handler() http.Handler {
	h := s.versionMiddleware(s.mux)
	// Apply outer wraps in reverse registration order so the first-
	// registered middleware ends up outermost (closest to the
	// listener).
	for _, mw := range slices.Backward(s.outerWraps) {
		h = mw(h)
	}
	return h
}

// Wrap registers an outer middleware applied to every request after
// the version header is set. Mounting order matters: the first-
// registered Wrap runs outermost. Use this for proxy/X-Forwarded
// parsing, request logging, or any concern that operates on the full
// request envelope. Calling Wrap after Run is racy and not supported.
func (s *Server) Wrap(mw func(http.Handler) http.Handler) {
	s.outerWraps = append(s.outerWraps, mw)
}

// Run starts the listener and blocks until ctx is canceled or the
// underlying server.Serve returns. On ctx cancel, Run issues a
// graceful Shutdown with the supplied drain deadline.
func (s *Server) Run(ctx context.Context, drain time.Duration) error {
	if drain <= 0 {
		drain = 15 * time.Second
	}
	s.srv = &http.Server{
		Addr:              s.addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// context.WithoutCancel is deliberate: the parent ctx is
		// already done, but the drain window is the daemon's
		// promised shutdown grace from design/daemon.md §7. Wiring
		// drain to the canceled parent would cut drain to zero on
		// SIGTERM.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drain)
		defer cancel()
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		// Drain the Serve goroutine.
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// versionMiddleware sets X-Resy-Snipe-Version on every response,
// including those from /, /healthz, and the 404 envelope. The
// header is the wire contract for client log-correlation.
func (s *Server) versionMiddleware(next http.Handler) http.Handler {
	v := s.version
	if v == "" {
		v = "dev"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resy-Snipe-Version", v)
		next.ServeHTTP(w, r)
	})
}

// notFound writes the daemon's JSON 404 envelope for unhandled
// routes. net/http.ServeMux dispatches "/" as a catch-all when no
// more specific route matches; this handler is the catch-all body.
//
// Operator metric: a sudden 404 spike is usually a client/daemon
// version mismatch — the request log includes the path so it is
// easy to correlate.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	if s.log != nil {
		s.log.Info("transport: 404",
			"method", r.Method,
			"path", r.URL.Path,
		)
	}
	WriteErrorDetails(w, s.log, fmt.Errorf("route %s %s: %w", r.Method, r.URL.Path, service.ErrNotFound), map[string]any{
		"method": r.Method,
		"path":   r.URL.Path,
	})
}
