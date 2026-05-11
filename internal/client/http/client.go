// Package httpclient is the resy-snipe CLI's HTTP client for the
// daemon's /v1 surface. It mirrors the verbs exposed by
// internal/transport/http (see docs/v2/design/daemon.md §4.1) and
// converts wire envelopes back into domain/service types plus
// errors.Is-compatible sentinels.
//
// The package is deliberately decoupled from internal/transport/http
// — it owns its own wire codec (wire.go) and its own envelope decoder
// (errors.go) so the CLI binary does not pull in the daemon's full
// dependency graph (sqlite, store, engine). The codecs MUST stay
// byte-compatible with the server's; any wire change lands on both
// sides in the same commit.
package httpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// defaultUserAgent is what we send when WithUserAgent isn't supplied.
// The version suffix is intentionally generic — release tooling can
// override via WithUserAgent at binary-build time.
const defaultUserAgent = "resy-snipe-cli/0.0"

// defaultTimeout bounds the non-streaming HTTP calls. SSE subscribes
// are explicitly long-lived and bypass this via a separate http.Client
// (the streaming client has no per-request timeout — the cancellation
// path is ctx.Done).
const defaultTimeout = 30 * time.Second

// Client is the resy-snipe daemon client. It is safe for concurrent
// use by multiple goroutines — http.Client is concurrent-safe and the
// other fields are read-only after construction.
type Client struct {
	baseURL   *url.URL
	token     string
	userAgent string
	httpc     *http.Client
	// streamc is a separate client for SSE — it has no per-request
	// timeout so the connection can stay open until terminal status or
	// caller cancellation. Sharing the transport across both clients
	// would let SSE inherit a Timeout the streaming verbs cannot
	// tolerate.
	streamc *http.Client
}

// Option configures a Client. Pass options to New.
type Option func(*clientOpts)

type clientOpts struct {
	httpc     *http.Client
	userAgent string
	timeout   time.Duration
}

// WithHTTPClient replaces the default *http.Client used for unary
// (non-streaming) verbs. The supplied client is reused as-is — the
// caller controls Timeout, TLS config, transport pool. SSE always
// uses an internal client with no per-request timeout (see Client).
func WithHTTPClient(c *http.Client) Option {
	return func(o *clientOpts) {
		if c != nil {
			o.httpc = c
		}
	}
}

// WithUserAgent overrides the User-Agent header sent on every request.
// Empty input restores the default.
func WithUserAgent(ua string) Option {
	return func(o *clientOpts) {
		if ua != "" {
			o.userAgent = ua
		}
	}
}

// WithTimeout sets the per-request timeout on the unary http.Client.
// Ignored when WithHTTPClient is also passed (the caller's client
// controls its own timeout). Zero or negative input is ignored.
func WithTimeout(d time.Duration) Option {
	return func(o *clientOpts) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// New constructs a Client. baseURL must be an absolute http or https
// URL (e.g. "http://127.0.0.1:7777"); token is the bearer token the
// daemon issued via /v1/auth/tokens. An empty token is permitted at
// construction so admin tooling that does not yet have a token can
// still talk to public-by-design endpoints — every verb that requires
// auth will surface the daemon's 401 envelope as ErrUnauthorized.
func New(baseURL, token string, opts ...Option) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("httpclient: baseURL is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("httpclient: parse baseURL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("httpclient: baseURL scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("httpclient: baseURL has no host")
	}
	o := clientOpts{
		userAgent: defaultUserAgent,
		timeout:   defaultTimeout,
	}
	for _, opt := range opts {
		opt(&o)
	}
	httpc := o.httpc
	if httpc == nil {
		httpc = &http.Client{Timeout: o.timeout}
	}
	streamc := &http.Client{Transport: httpc.Transport}
	return &Client{
		baseURL:   u,
		token:     token,
		userAgent: o.userAgent,
		httpc:     httpc,
		streamc:   streamc,
	}, nil
}

// ----------------------------------------------------------------------
// Venues
// ----------------------------------------------------------------------

type resolveVenueRequest struct {
	VenueQuery venueQueryWire `json:"venue_query"`
}

// ResolveVenue calls POST /v1/venues/resolve. Returns the resolved
// Venue or an *Error (errors.Is against service.ErrVenueNotFound /
// ErrVenueAmbiguous covers the dual-error path).
func (c *Client) ResolveVenue(ctx context.Context, q domain.VenueQuery) (domain.Venue, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/venues/resolve", nil, resolveVenueRequest{
		VenueQuery: encodeVenueQuery(q),
	})
	if err != nil {
		return domain.Venue{}, err
	}
	var w venueWire
	if err := c.do(req, &w); err != nil {
		return domain.Venue{}, err
	}
	return decodeVenue(w)
}

// ----------------------------------------------------------------------
// Plans
// ----------------------------------------------------------------------

type planQuestRequest struct {
	Goal goalWire `json:"goal"`
}

type planQuestResponse struct {
	Plan planWire `json:"plan"`
}

// PlanQuest calls POST /v1/quests/plan. Returns the canonical Plan
// (its Hash is what CreateOpts.PlanHash should pin in a subsequent
// CreateQuest call).
func (c *Client) PlanQuest(ctx context.Context, goal domain.Goal) (domain.Plan, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/quests/plan", nil, planQuestRequest{
		Goal: encodeGoal(goal),
	})
	if err != nil {
		return domain.Plan{}, err
	}
	var resp planQuestResponse
	if err := c.do(req, &resp); err != nil {
		return domain.Plan{}, err
	}
	return decodePlan(resp.Plan)
}

// ----------------------------------------------------------------------
// Quests: create / get / list / cancel
// ----------------------------------------------------------------------

// CreateOpts mirrors service.CreateOpts. PlanHash pins the create to a
// Plan computed earlier (TOCTOU defense); IdempotencyKey makes the
// call replayable within the retention window.
type CreateOpts struct {
	PlanHash       *string
	IdempotencyKey *string
}

type createQuestRequest struct {
	Goal           goalWire `json:"goal"`
	PlanHash       *string  `json:"plan_hash,omitempty"`
	IdempotencyKey *string  `json:"idempotency_key,omitempty"`
}

type createQuestResponse struct {
	ID string `json:"id"`
}

// CreateQuest calls POST /v1/quests. Returns the freshly-minted
// QuestID. A nil opts is equivalent to a zero CreateOpts.
func (c *Client) CreateQuest(ctx context.Context, goal domain.Goal, opts CreateOpts) (domain.QuestID, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/quests", nil, createQuestRequest{
		Goal:           encodeGoal(goal),
		PlanHash:       opts.PlanHash,
		IdempotencyKey: opts.IdempotencyKey,
	})
	if err != nil {
		return "", err
	}
	var resp createQuestResponse
	if err := c.do(req, &resp); err != nil {
		return "", err
	}
	return domain.QuestID(resp.ID), nil
}

// GetQuest calls GET /v1/quests/{id} and returns the full state view.
func (c *Client) GetQuest(ctx context.Context, id domain.QuestID) (service.QuestState, error) {
	if id == "" {
		return service.QuestState{}, fmt.Errorf("httpclient: GetQuest: %w: empty quest id", service.ErrInvalidArgument)
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/quests/"+url.PathEscape(string(id)), nil, nil)
	if err != nil {
		return service.QuestState{}, err
	}
	var w questStateWire
	if err := c.do(req, &w); err != nil {
		return service.QuestState{}, err
	}
	return decodeQuestState(w)
}

// ListFilter mirrors service.ListFilter and is encoded into the query
// string the server's parseListFilter understands.
type ListFilter struct {
	Status    []domain.Status
	AccountID *domain.AccountID
	Since     *time.Time
	Until     *time.Time
	Limit     int
	Cursor    string
}

type listQuestsResponse struct {
	Quests []questSummaryWire `json:"quests"`
}

// ListQuests calls GET /v1/quests with the supplied filter encoded as
// query parameters.
func (c *Client) ListQuests(ctx context.Context, filter ListFilter) ([]service.QuestSummary, error) {
	q := url.Values{}
	if len(filter.Status) > 0 {
		names := make([]string, 0, len(filter.Status))
		for _, s := range filter.Status {
			names = append(names, s.String())
		}
		q.Set("status", strings.Join(names, ","))
	}
	if filter.AccountID != nil {
		q.Set("account_id", string(*filter.AccountID))
	}
	if filter.Since != nil {
		q.Set("since", filter.Since.UTC().Format(time.RFC3339))
	}
	if filter.Until != nil {
		q.Set("until", filter.Until.UTC().Format(time.RFC3339))
	}
	if filter.Limit > 0 {
		q.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Cursor != "" {
		q.Set("cursor", filter.Cursor)
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/quests", q, nil)
	if err != nil {
		return nil, err
	}
	var resp listQuestsResponse
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	out := make([]service.QuestSummary, 0, len(resp.Quests))
	for _, w := range resp.Quests {
		s, err := decodeQuestSummary(w)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// CancelOpts mirrors service.CancelOpts. Reason is the audit
// annotation; IdempotencyKey makes the call replayable.
type CancelOpts struct {
	Reason         string
	IdempotencyKey *string
}

type cancelQuestRequest struct {
	Reason         string  `json:"reason,omitempty"`
	IdempotencyKey *string `json:"idempotency_key,omitempty"`
}

// CancelQuest calls DELETE /v1/quests/{id}. The DELETE body is optional
// per the server — we omit it entirely when opts is the zero value to
// keep the wire compact and to match curl-by-hand usage.
func (c *Client) CancelQuest(ctx context.Context, id domain.QuestID, opts CancelOpts) error {
	if id == "" {
		return fmt.Errorf("httpclient: CancelQuest: %w: empty quest id", service.ErrInvalidArgument)
	}
	var body any
	if opts.Reason != "" || opts.IdempotencyKey != nil {
		body = cancelQuestRequest(opts)
	}
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/quests/"+url.PathEscape(string(id)), nil, body)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// ----------------------------------------------------------------------
// SSE: SubscribeQuest
// ----------------------------------------------------------------------

// SubscribeQuest opens GET /v1/quests/{id}/events and invokes callback
// for each event the daemon emits. Events arrive in order; the call
// returns when the daemon closes the stream (terminal status) or ctx
// is canceled. A non-2xx status response is translated to *Error
// before any callback fires.
//
// The callback runs on the caller's goroutine (the reader loop). It
// must not block indefinitely — a slow callback delays subsequent
// events. The daemon's contract is "the connection stays open until
// terminal status", so the goroutine eventually returns even if the
// caller stops canceling.
//
// An in-stream "error" event (the server's mid-stream signal that the
// engine failed after the 200 OK was committed) is surfaced as an
// error return; the callback is not invoked for it.
func (c *Client) SubscribeQuest(ctx context.Context, id domain.QuestID, callback func(domain.Event)) error {
	if id == "" {
		return fmt.Errorf("httpclient: SubscribeQuest: %w: empty quest id", service.ErrInvalidArgument)
	}
	if callback == nil {
		return errors.New("httpclient: SubscribeQuest: nil callback")
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/quests/"+url.PathEscape(string(id))+"/events", nil, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.streamc.Do(req)
	if err != nil {
		return fmt.Errorf("httpclient: transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return parseErrorEnvelope(resp.StatusCode, body)
	}
	return parseSSE(resp.Body, callback)
}

// parseSSE is the minimal SSE frame parser the daemon's stream demands.
// The server emits the basic "event: <type>\ndata: <json>\n\n" framing
// with no id/retry fields and no multi-line data; we keep the parser
// matched to that shape to avoid surprising MIME edge cases.
func parseSSE(r io.Reader, callback func(domain.Event)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	var (
		evType string
		data   strings.Builder
	)
	flush := func() error {
		if evType == "" && data.Len() == 0 {
			return nil
		}
		defer func() {
			evType = ""
			data.Reset()
		}()
		if evType == "error" {
			// The server JSON-encodes the error string. Best-effort
			// decode; fall back to the raw payload if decoding fails.
			var msg string
			if err := json.Unmarshal([]byte(data.String()), &msg); err == nil {
				return fmt.Errorf("httpclient: subscribe: stream error: %s", msg)
			}
			return fmt.Errorf("httpclient: subscribe: stream error: %s", data.String())
		}
		var w eventWire
		if err := json.Unmarshal([]byte(data.String()), &w); err != nil {
			return fmt.Errorf("httpclient: subscribe: decode event: %w", err)
		}
		ev, err := decodeEvent(w)
		if err != nil {
			return fmt.Errorf("httpclient: subscribe: decode event: %w", err)
		}
		callback(ev)
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "event: "):
			evType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line, "data: "))
		default:
			// Unknown line — includes comment frames (":heartbeat") the
			// server may use as keep-alives. Ignore to stay lenient.
		}
	}
	if err := scanner.Err(); err != nil {
		// A context-canceled read shows up here as the wrapped ctx
		// error from the http transport; surface it unchanged so the
		// caller can errors.Is(err, context.Canceled).
		return fmt.Errorf("httpclient: subscribe: read: %w", err)
	}
	// Flush a trailing frame that wasn't terminated by a blank line.
	return flush()
}

// ----------------------------------------------------------------------
// Accounts: login + list
// ----------------------------------------------------------------------

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	AccountID string `json:"account_id"`
}

// Login calls POST /v1/accounts/login. The plaintext password is
// transmitted in the request body (TLS is assumed when the daemon is
// not on the local loopback); the daemon never logs the password and
// stores only a sealed session token derived from it.
func (c *Client) Login(ctx context.Context, email, password string) (domain.AccountID, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/accounts/login", nil, loginRequest{
		Email:    email,
		Password: password,
	})
	if err != nil {
		return "", err
	}
	var resp loginResponse
	if err := c.do(req, &resp); err != nil {
		return "", err
	}
	return domain.AccountID(resp.AccountID), nil
}

type listAccountsResponse struct {
	Accounts []accountWire `json:"accounts"`
}

// ListAccounts calls GET /v1/accounts.
func (c *Client) ListAccounts(ctx context.Context) ([]service.Account, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/accounts", nil, nil)
	if err != nil {
		return nil, err
	}
	var resp listAccountsResponse
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	out := make([]service.Account, 0, len(resp.Accounts))
	for _, w := range resp.Accounts {
		a, err := decodeAccount(w)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// ----------------------------------------------------------------------
// Tokens: issue / revoke / list
// ----------------------------------------------------------------------

type issueTokenRequest struct {
	Label string `json:"label"`
	Scope string `json:"scope"`
}

type issueTokenResponse struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Label     string `json:"label"`
	Scope     string `json:"scope"`
	CreatedAt string `json:"created_at"`
}

// IssueToken calls POST /v1/auth/tokens. Returns a BearerToken
// containing the plaintext token (returned exactly once — the daemon
// stores only its hash).
func (c *Client) IssueToken(ctx context.Context, label, scope string) (service.BearerToken, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/auth/tokens", nil, issueTokenRequest{
		Label: label,
		Scope: scope,
	})
	if err != nil {
		return service.BearerToken{}, err
	}
	var resp issueTokenResponse
	if err := c.do(req, &resp); err != nil {
		return service.BearerToken{}, err
	}
	created, err := parseRFC3339(resp.CreatedAt)
	if err != nil {
		return service.BearerToken{}, fmt.Errorf("httpclient: IssueToken: decode created_at: %w", err)
	}
	return service.BearerToken{
		ID:        resp.ID,
		Token:     resp.Token,
		Label:     resp.Label,
		Scope:     resp.Scope,
		CreatedAt: created,
	}, nil
}

// RevokeToken calls DELETE /v1/auth/tokens/{id}.
func (c *Client) RevokeToken(ctx context.Context, tokenID string) error {
	if tokenID == "" {
		return fmt.Errorf("httpclient: RevokeToken: %w: empty token id", service.ErrInvalidArgument)
	}
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/auth/tokens/"+url.PathEscape(tokenID), nil, nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

type listTokensResponse struct {
	Tokens []tokenResponseWire `json:"tokens"`
}

// ListTokens calls GET /v1/auth/tokens.
func (c *Client) ListTokens(ctx context.Context) ([]service.Token, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/auth/tokens", nil, nil)
	if err != nil {
		return nil, err
	}
	var resp listTokensResponse
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	out := make([]service.Token, 0, len(resp.Tokens))
	for _, w := range resp.Tokens {
		t, err := decodeToken(w)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// AsError is a convenience for callers that want the structured
// envelope view. Returns nil when err is not a wire-mapped error
// (transport errors, decoder errors, etc.).
func AsError(err error) *Error { return asError(err) }

// ----------------------------------------------------------------------
// Internal helpers (placed at end of file per the funcorder lint —
// unexported methods follow the last exported method).
// ----------------------------------------------------------------------

// resolve builds an absolute URL for a /v1 path. Query parameters are
// appended via the q argument; pass nil for no query.
func (c *Client) resolve(path string, q url.Values) string {
	u := *c.baseURL
	// Preserve any base path the operator configured (e.g. behind a
	// reverse proxy that prefixes /api).
	u.Path = strings.TrimRight(u.Path, "/") + path
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// newRequest constructs an *http.Request with Bearer auth, User-Agent,
// and a JSON body (nil body is allowed for verbs that take no body).
func (c *Client) newRequest(ctx context.Context, method, path string, q url.Values, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := marshalJSON(body)
		if err != nil {
			return nil, fmt.Errorf("httpclient: marshal request body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.resolve(path, q), rdr)
	if err != nil {
		return nil, fmt.Errorf("httpclient: build request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// do executes req on the unary client and routes the response: 2xx
// responses are decoded into out (when non-nil); non-2xx responses are
// translated to *Error via parseErrorEnvelope. Network/transport
// failures wrap with %w so callers can errors.Is against the
// underlying net error.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("httpclient: transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("httpclient: read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseErrorEnvelope(resp.StatusCode, body)
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("httpclient: decode response: %w", err)
	}
	return nil
}
