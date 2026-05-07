package resy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// MFAError is returned by Login when Resy responds with an MFA
// challenge. Callers use errors.As to extract the opaque Challenge
// token, then pass it back through CompleteMFA along with the
// user-entered code.
//
// The error wraps providers.ErrMFARequired so engine code can branch
// on errors.Is(err, providers.ErrMFARequired) without knowing about
// this concrete type.
type MFAError struct {
	Challenge string
}

// Error satisfies the error interface.
func (*MFAError) Error() string { return "resy: MFA required" }

// Unwrap satisfies errors.Is(err, providers.ErrMFARequired).
func (*MFAError) Unwrap() error { return providers.ErrMFARequired }

// authResponseEnvelope is the JSON shape returned by /3/auth/password.
// Real Resy responses also carry an mfa_required signal on success
// status when the account has 2FA on; we model that here so the
// detection path is testable.
type authResponseEnvelope struct {
	AuthResponse

	MFARequired   bool   `json:"mfa_required"`
	ChallengeID   string `json:"challenge_id"`
	ChallengeType string `json:"challenge_type"`
}

// Login posts email + password to /3/auth/password, parses the JWT,
// derives the expiry from the JWT exp claim, and (when a Store is
// configured) persists the resulting Session. It returns:
//
//   - a typed *Session on success;
//   - *MFAError (which Is providers.ErrMFARequired) when the response
//     carries an MFA challenge;
//   - providers.ErrAuthExpired (wrapped) on HTTP 401 / bad credentials;
//   - a wrapped transport error on anything else.
func (c *Client) Login(ctx context.Context, creds providers.Credentials) (*Session, error) {
	rc, ok := creds.(Credentials)
	if !ok {
		return nil, fmt.Errorf("resy.Login: expected resy.Credentials, got %T", creds)
	}
	if rc.Email == "" || rc.Password == "" {
		return nil, errors.New("resy.Login: email and password are required")
	}

	form := url.Values{
		"email":    {rc.Email},
		"password": {rc.Password},
	}
	body, resp, err := c.do(ctx, http.MethodPost, "/3/auth/password",
		nil, strings.NewReader(form.Encode()), "")
	if err != nil {
		return nil, fmt.Errorf("resy.Login: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("resy.Login: bad credentials: %w", providers.ErrAuthExpired)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("resy.Login: status %d: %s", resp.StatusCode, truncate(string(body), 256))
	}

	var env authResponseEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("resy.Login: parse response: %w", err)
	}

	if env.MFARequired {
		challenge := env.ChallengeID
		if challenge == "" {
			challenge = env.ChallengeType
		}
		return nil, &MFAError{Challenge: challenge}
	}

	if env.Token == "" {
		return nil, errors.New("resy.Login: response had no token")
	}

	exp, err := parseJWTExp(env.Token)
	if err != nil {
		return nil, fmt.Errorf("resy.Login: %w", err)
	}

	sess := &Session{
		userID:    domain.UserID(rc.Email),
		jwt:       env.Token,
		expiresAt: exp,
	}

	if c.store != nil {
		now := c.clock.Now()
		row := SessionRow{
			UserID:    sess.userID,
			Provider:  providerID,
			JWT:       sess.jwt,
			ExpiresAt: sess.expiresAt,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := c.store.UpsertSession(ctx, row); err != nil {
			return nil, fmt.Errorf("resy.Login: persist session: %w", err)
		}
	}

	return sess, nil
}

// CompleteMFA submits a user-entered code against an opaque challenge
// returned by Login. The seam exists for the Phase 2 daemon /
// interactive CLI flow; Phase 1 surfaces this as not-implemented so
// callers can wire UX while the actual endpoint shape calibrates.
func (c *Client) CompleteMFA(_ context.Context, challenge, code string) (*Session, error) {
	if challenge == "" || code == "" {
		return nil, errors.New("resy.CompleteMFA: challenge and code are required")
	}
	return nil, errors.New("resy.CompleteMFA: not implemented in Phase 1 — calibrate against the real MFA endpoint in Phase 2")
}

// LoadSession reads a previously persisted session for `user`. It
// returns the same (wrapped) sentinel errors the underlying store
// uses (store.ErrNotFound when there is no row, store.ErrSessionExpired
// when the row exists but its exp has passed).
func (c *Client) LoadSession(ctx context.Context, user domain.UserID) (*Session, error) {
	if c.store == nil {
		return nil, errors.New("resy.LoadSession: no SessionStore configured")
	}
	row, err := c.store.GetSession(ctx, user, providerID, c.clock.Now())
	if err != nil {
		return nil, fmt.Errorf("resy.LoadSession: %w", err)
	}
	return &Session{
		userID:    row.UserID,
		jwt:       row.JWT,
		expiresAt: row.ExpiresAt,
	}, nil
}

// Ping verifies that `sess` is still accepted by Resy. It does NOT
// round-trip if the session is locally known to be expired (per the
// injected Clock); in that case it returns providers.ErrAuthExpired
// without burning a request. This is the "GetSession first, skip if
// exp passed" rule from R2's spec applied to the Ping path.
func (c *Client) Ping(ctx context.Context, sess providers.Session) error {
	rs, ok := sess.(*Session)
	if !ok {
		return fmt.Errorf("resy.Ping: expected *resy.Session, got %T", sess)
	}
	if providers.IsSessionExpired(rs, c.clock.Now()) {
		return fmt.Errorf("resy.Ping: locally expired: %w", providers.ErrAuthExpired)
	}

	_, resp, err := c.do(ctx, http.MethodGet, "/2/user", nil, nil, rs.jwt)
	if err != nil {
		return fmt.Errorf("resy.Ping: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("resy.Ping: 401: %w", providers.ErrAuthExpired)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("resy.Ping: status %d", resp.StatusCode)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
