package resy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"resy-snipe/internal/domain"
)

// Session is the typed authenticated handle for the Resy adapter. It
// carries the JWT and the parsed expiry so the engine can honor
// "never re-login when a valid session exists" without round-tripping
// to Resy.
type Session struct {
	userID    domain.UserID
	jwt       string
	expiresAt time.Time
}

// User satisfies providers.Session.
func (s *Session) User() domain.UserID { return s.userID }

// Provider satisfies providers.Session.
func (*Session) Provider() domain.ProviderID { return providerID }

// ExpiresAt satisfies providers.Session.
func (s *Session) ExpiresAt() time.Time { return s.expiresAt }

// JWT returns the underlying token. Other adapter files use this when
// building authed requests; engine code never reads it directly.
func (s *Session) JWT() string { return s.jwt }

// errMissingJWTExp is returned when a JWT decodes successfully but has
// no exp claim. Resy's tokens always carry an exp; missing one means a
// malformed or fake token, which is a hard error not a soft warning.
var errMissingJWTExp = errors.New("resy: JWT has no exp claim")

// parseJWTExp extracts the exp claim from a JWT and returns it as a
// UTC time.Time. Resy's JWT is the standard three-part dot-separated
// form; we only inspect the payload (we do not verify the signature
// because Resy's signing key is not public — the token's authority is
// established by the API accepting it on a subsequent call).
func parseJWTExp(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("resy: not a JWT (got %d parts, want 3)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some JWTs come padded; fall through to standard decoding.
		var stdErr error
		payload, stdErr = base64.StdEncoding.DecodeString(parts[1])
		if stdErr != nil {
			return time.Time{}, fmt.Errorf("resy: decode JWT payload: %w", err)
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("resy: parse JWT claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errMissingJWTExp
	}
	return time.Unix(claims.Exp, 0).UTC(), nil
}
