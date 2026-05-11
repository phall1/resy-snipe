package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// TokenLookup is the slim auth seam the bearer middleware depends on.
// Defined-at-the-consumer per Law 5: the HTTP transport owns the
// contract, the cmd/ wiring layer binds it to the store.FindTokenByHash
// + store.TouchTokenSeen package functions, and the daemon glues
// everything together. The transport never imports internal/store
// directly.
//
// TouchSeen is called best-effort on every successful auth — the
// middleware coalesces calls so the store update fires at most ~1Hz
// per token (design/daemon.md §4.2). A TouchSeen failure logs but
// does not fail the request: the caller has already authenticated;
// a missed last_seen stamp is operational degradation, not a
// correctness bug.
type TokenLookup interface {
	FindTokenByHash(ctx context.Context, hash []byte) (TokenInfo, error)
	TouchSeen(ctx context.Context, hash []byte, now time.Time) error
}

// TokenInfo is the projection the middleware needs from a tokens row.
// UserID + Scope feed the ctx the Service layer reads; Hash is the
// coalescing key for TouchSeen. Live-only — revoked rows surface as
// the ErrTokenNotFound sentinel below.
type TokenInfo struct {
	UserID domain.UserID
	Scope  string // 'user' | 'operator'
	Hash   []byte // BLAKE2b-256 digest; matches the stored column
}

// ErrTokenNotFound is the sentinel TokenLookup implementations return
// when the presented hash does not match a live (non-revoked) row.
// The middleware translates this to a 401 unauthenticated envelope.
// Distinct from service.ErrNotFound so a store-layer ErrNotFound from
// an unrelated path can't be confused with a missing-token auth
// failure.
var ErrTokenNotFound = errors.New("transport/http: token not found")

// touchInterval is the floor between two TouchSeen calls for the same
// token hash. design/daemon.md §4.2 specifies "~1Hz" — we go a bit
// looser at 1.5s to absorb retry storms (CLI watch reconnects) without
// drumming the writer. The interval is process-local: a daemon
// restart resets the coalescer, which is fine; the worst case is one
// extra UPDATE per token at startup.
const touchInterval = 1500 * time.Millisecond

// AuthMiddleware wraps a Handler with bearer-token authentication.
// The middleware parses the Authorization: Bearer <token> header,
// hashes the plaintext via BLAKE2b-256, looks the hash up in
// TokenLookup, and on success stamps the caller (userID + scope)
// onto the request context via service.ContextWithCaller. On
// failure it emits a 401 envelope.
//
// Routes that should bypass auth (e.g. /healthz, /readyz) compose
// outside this middleware. The daemon's serve subcommand mounts the
// public-by-design routes on the bare mux and wraps only the /v1
// routes with this middleware.
type AuthMiddleware struct {
	lookup TokenLookup
	log    *slog.Logger
	now    func() time.Time

	// coalescer keys are the hex of the hash; value is the wall-time
	// of the most recent TouchSeen we issued for that hash. A sync.Map
	// is fine here — read-heavy, key-set bounded by live tokens, no
	// reclamation needed beyond what the daemon process lifetime
	// implies.
	touchMu   sync.Mutex
	lastTouch map[string]time.Time
}

// NewAuthMiddleware constructs the middleware. lookup is required; a
// nil lookup panics at construction rather than at first request.
// log is optional (defaults to slog.Default). now is the clock seam
// for the TouchSeen coalescer; tests pin it via a fixed time source,
// production passes time.Now.
func NewAuthMiddleware(lookup TokenLookup, log *slog.Logger, now func() time.Time) *AuthMiddleware {
	if lookup == nil {
		panic("NewAuthMiddleware: nil lookup")
	}
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &AuthMiddleware{
		lookup:    lookup,
		log:       log,
		now:       now,
		lastTouch: make(map[string]time.Time),
	}
}

// Wrap returns an http.Handler that authenticates every request before
// delegating to next. Failed auth short-circuits with a 401 envelope.
func (m *AuthMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := parseBearer(r.Header.Get("Authorization"))
		if err != nil {
			WriteError(w, m.log, fmt.Errorf("auth: %w", service.ErrUnauthorized))
			return
		}
		hash := blake2b.Sum256([]byte(token))
		info, err := m.lookup.FindTokenByHash(r.Context(), hash[:])
		if err != nil {
			// Both ErrTokenNotFound and any unexpected lookup error
			// collapse to 401. Unexpected errors also log so the
			// operator can see a wedge in the store layer.
			if !errors.Is(err, ErrTokenNotFound) {
				m.log.Warn("transport.auth_lookup_failed",
					slog.String("err", err.Error()))
			}
			WriteError(w, m.log, fmt.Errorf("auth: %w", service.ErrUnauthorized))
			return
		}
		// Coalesced last_seen update. We hold the lock for the
		// shortest possible duration — just long enough to decide
		// whether to fire, and to stamp the new wall-time before
		// the call. The lookup happens outside the lock so a slow
		// SQLite writer can't stall concurrent unrelated requests.
		now := m.now()
		if m.shouldTouch(info.Hash, now) {
			if terr := m.lookup.TouchSeen(r.Context(), info.Hash, now); terr != nil {
				m.log.Warn("transport.touch_seen_failed",
					slog.String("err", terr.Error()))
			}
		}
		ctx := service.ContextWithCaller(r.Context(), info.UserID, info.Scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// shouldTouch returns true and stamps the touch wall-time when the
// supplied hash has not been touched within touchInterval. False
// otherwise — the caller skips the store write.
func (m *AuthMiddleware) shouldTouch(hash []byte, now time.Time) bool {
	key := string(hash)
	m.touchMu.Lock()
	defer m.touchMu.Unlock()
	if prev, ok := m.lastTouch[key]; ok && now.Sub(prev) < touchInterval {
		return false
	}
	m.lastTouch[key] = now
	return true
}

// parseBearer extracts the token plaintext from an Authorization
// header value. Returns an error if the header is missing, the scheme
// is not Bearer, or the token is empty. The scheme match is case-
// insensitive (RFC 7235) but the plaintext is preserved verbatim —
// the hash function is byte-sensitive.
func parseBearer(header string) (string, error) {
	if header == "" {
		return "", errors.New("missing Authorization header")
	}
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", errors.New("authorization header is not a Bearer scheme")
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", errors.New("bearer token is empty")
	}
	return token, nil
}
