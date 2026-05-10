package resy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/resy/sign"
)

// Resy's API host. Tests override via WithBaseURL.
const DefaultBaseURL = "https://api.resy.com"

// DefaultAPIKey is the shared static API key baked into Resy's frontend
// JavaScript. It is identical for every browser and is not user-tied;
// callers can override via WithAPIKey or the RESY_API_KEY env var.
//
//nolint:gosec // intentionally a public, browser-shared key — not a credential
const DefaultAPIKey = "VbWk7s3L4KiK5fzlO7JD3Q5EYolJI7n5"

// DefaultUserAgent matches a recent desktop Chrome string. Resy's
// anti-bot pipeline rejects unrealistic UAs; this string is the one
// the legacy code shipped with successfully.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"

const (
	originHost = "https://widgets.resy.com"
	refererURL = "https://widgets.resy.com/"
)

// Client is the typed HTTP client for Resy. It is responsible for
// transit only: header assembly, body read, x-request-id capture, and
// slog emission. Endpoint-specific logic (Login, Find, Calendar,
// Details, Book) lives in sibling files in this package.
type Client struct {
	httpClient  *http.Client
	baseURL     string
	apiKey      string
	userAgent   string
	log         *slog.Logger
	clock       clock.Clock
	store       SessionStore
	signer      sign.Signer
	idempotency *idempotencyTracker
}

// SessionStore is the slim subset of internal/store.Store this adapter
// needs for session persistence. Declaring it locally lets tests
// substitute an in-memory fake without spinning up SQLite, and lets
// the production wiring satisfy it with a real *store.SQLiteStore via
// a thin adapter.
type SessionStore interface {
	UpsertSession(ctx context.Context, s SessionRow) error
	GetSession(ctx context.Context, user domain.UserID, provider domain.ProviderID, now time.Time) (SessionRow, error)
}

// SessionRow mirrors store.SessionRow at this seam. Wiring code at the
// cmd/ layer translates between the two so internal/resy doesn't have
// to import internal/store directly.
type SessionRow struct {
	UserID    domain.UserID
	Provider  domain.ProviderID
	JWT       string
	ExpiresAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Option configures a Client at construction.
type Option func(*Client)

// WithHTTPClient swaps the underlying *http.Client (for httptest).
func WithHTTPClient(c *http.Client) Option { return func(x *Client) { x.httpClient = c } }

// WithBaseURL overrides the API host (for httptest or staging).
func WithBaseURL(u string) Option { return func(x *Client) { x.baseURL = u } }

// WithAPIKey overrides the static API key.
func WithAPIKey(k string) Option { return func(x *Client) { x.apiKey = k } }

// WithUserAgent overrides the User-Agent string.
func WithUserAgent(ua string) Option { return func(x *Client) { x.userAgent = ua } }

// WithStore enables session persistence. Without this, Login still
// returns a Session but does not write it; LoadSession is a no-op.
func WithStore(s SessionStore) Option { return func(x *Client) { x.store = s } }

// WithSigner installs a sign.Signer so /3/details and /3/book pick up
// PerimeterX-aware signing headers and recover from
// providers.ErrAntiBotChallenge via the sign-and-retry path. Default
// is sign.Noop, which is a true no-op (no headers, no errors), so
// callers that have not opted in see today's behavior unchanged.
func WithSigner(s sign.Signer) Option {
	return func(x *Client) {
		if s == nil {
			s = sign.Noop{}
		}
		x.signer = s
	}
}

// NewClient constructs a Client with sensible defaults. All
// dependencies are required; nil panics fail-fast at boot.
//
// The default API key is read from RESY_API_KEY if set, else falls
// back to the constant; tests should always pass WithAPIKey to keep
// the env out of the test path.
func NewClient(log *slog.Logger, c clock.Clock, opts ...Option) *Client {
	if log == nil {
		panic("resy: nil logger")
	}
	if c == nil {
		panic("resy: nil clock")
	}
	cl := &Client{
		httpClient: &http.Client{
			// Per-call deadlines come from ctx; the http.Client timeout
			// is a hard ceiling so a mis-set ctx cannot hang forever.
			Timeout: 30 * time.Second,
		},
		baseURL:     DefaultBaseURL,
		apiKey:      resolveAPIKey(),
		userAgent:   DefaultUserAgent,
		log:         log,
		clock:       c,
		signer:      sign.Noop{},
		idempotency: &idempotencyTracker{},
	}
	for _, o := range opts {
		o(cl)
	}
	return cl
}

func resolveAPIKey() string {
	if v := os.Getenv("RESY_API_KEY"); v != "" {
		return v
	}
	return DefaultAPIKey
}

// Response describes the metadata of a single round trip. Endpoint
// methods unmarshal the body separately; this is for status / request
// id / header inspection.
type Response struct {
	StatusCode int
	RequestID  string
	Header     http.Header
}

// defaultPerCallTimeout caps any single Resy HTTP round-trip when the
// caller's context does not already carry a deadline. It mirrors the
// http.Client.Timeout ceiling so the practical worst case is the same
// either way. Engine code that passes a long-lived lifetime context
// (the snipe's lifetime, ctx.Background(), etc.) gets a deadline
// derived here automatically — the alternative was forcing every call
// site to wrap, which proved brittle when an engine refactor or a CLI
// subcommand forgot one round trip and the binary failed at the
// boundary instead of at the misbehaving server.
const defaultPerCallTimeout = 30 * time.Second

// do is the single transit point for Resy HTTP. authToken may be empty
// for unauthenticated calls (login bootstrap). All standard headers
// are applied; the x-request-id is captured and logged.
func (c *Client) do(
	ctx context.Context,
	method, path string,
	query url.Values,
	body io.Reader,
	authToken string,
) ([]byte, *Response, error) {
	return c.doWithExtraHeaders(ctx, method, path, query, body, authToken, nil)
}

// doWithExtraHeaders is do() plus a per-call header bag. Empty values
// are skipped — the X-Resy-Idempotency-Key path passes "" when no key
// applies, so the absence is handled here rather than at every caller.
func (c *Client) doWithExtraHeaders(
	ctx context.Context,
	method, path string,
	query url.Values,
	body io.Reader,
	authToken string,
	extra map[string]string,
) ([]byte, *Response, error) {
	// Apply a per-call deadline when the caller hasn't. The hard
	// http.Client.Timeout ceiling above is the safety net; this
	// derived ctx is what actually cancels NewRequestWithContext mid-
	// flight if the server hangs after sending headers.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultPerCallTimeout)
		defer cancel()
	}

	fullURL := c.baseURL + path
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, nil, fmt.Errorf("resy %s %s: build request: %w", method, path, err)
	}
	c.applyHeaders(req, authToken)
	for k, v := range extra {
		if v == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("resy %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("resy %s %s: read body: %w", method, path, err)
	}

	requestID := firstNonEmpty(
		resp.Header.Get("X-Request-Id"),
		resp.Header.Get("x-request-id"),
	)

	c.log.LogAttrs(ctx, slog.LevelDebug, "resy http",
		slog.String(domain.LogKeyProvider, "resy"),
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", resp.StatusCode),
		slog.Int("body_bytes", len(bodyBytes)),
		slog.String(domain.LogKeyResyRequestID, requestID),
	)

	return bodyBytes, &Response{
		StatusCode: resp.StatusCode,
		RequestID:  requestID,
		Header:     resp.Header,
	}, nil
}

func (c *Client) applyHeaders(req *http.Request, authToken string) {
	h := req.Header
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Authorization", fmt.Sprintf(`ResyAPI api_key=%q`, c.apiKey))
	if authToken != "" {
		h.Set("X-Resy-Universal-Auth", authToken)
		// Resy expects this header in lowercase verbatim. Direct map
		// assignment bypasses Header.Set's canonicalisation; without
		// this, Go would send "X-Resy-Auth-Token" and the API rejects
		// the call.
		h["x-resy-auth-token"] = []string{authToken}
	}
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("Origin", originHost)
	h.Set("Referer", refererURL)
	h.Set("X-Origin", originHost)
	h.Set("User-Agent", c.userAgent)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
