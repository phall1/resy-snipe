package resy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"resy-snipe/internal/providers"
)

// classify maps a Resy HTTP response (status + body) to one of the
// providers.* sentinel errors. It is the single decision point so the
// engine never has to string-match a body — every adapter file routes
// transit failures through here.
//
// The classifier is intentionally table-driven over (status, body
// pattern). Specific patterns win over generic status codes; an
// unrecognized non-2xx becomes a wrapped error tagged with the status
// so it surfaces in logs.
//
// Example bodies seen during testing:
//
//   - 401 + `{"message":"signature expired"}`         → ErrAuthExpired
//   - 401 + `{"specific_message":"Authentication ..."} → ErrAuthExpired
//   - 410 + `{"specific_code":"BOOK_TOKEN_EXPIRED"}`  → ErrBookTokenExpired
//   - 409 + `{"specific_code":"SLOT_NOT_AVAILABLE"}`  → ErrSlotTaken
//   - 429 + `{"message":"rate limit"}`                → ErrRateLimited
//   - 403 + a body containing "captcha" or "challenge" → ErrAntiBotChallenge
//   - 200 + `{"results":{"venues":[]}}`               → ErrInventoryEmpty
//   - 200 + `{"mfa_required":true}` on auth response  → ErrMFARequired
//
// classify returns nil for 2xx responses that don't match the
// inventory-empty or MFA-required patterns. Callers parse the body on
// nil and fall through to taxonomy errors otherwise.
func classify(status int, body []byte) error {
	if status == http.StatusOK || status == http.StatusCreated {
		return classify2xx(body)
	}
	if status == http.StatusUnauthorized {
		return wrapWithBody(providers.ErrAuthExpired, status, body)
	}
	if status == http.StatusTooManyRequests {
		return wrapWithBody(providers.ErrRateLimited, status, body)
	}
	if status == http.StatusGone {
		return wrapWithBody(providers.ErrBookTokenExpired, status, body)
	}
	if status == http.StatusConflict {
		return wrapWithBody(providers.ErrSlotTaken, status, body)
	}
	if status == http.StatusForbidden {
		// 403 is overloaded by Resy. Most often it's an anti-bot
		// challenge; sometimes it's a permission failure on a venue.
		// We disambiguate via body content.
		if isAntiBotBody(body) {
			return wrapWithBody(providers.ErrAntiBotChallenge, status, body)
		}
		return wrapWithBody(providers.ErrAuthExpired, status, body)
	}
	// Specific code overrides — Resy sometimes returns 400 with a
	// specific_code that lines up with one of our sentinels.
	if code := extractSpecificCode(body); code != "" {
		switch strings.ToUpper(code) {
		case "BOOK_TOKEN_EXPIRED":
			return wrapWithBody(providers.ErrBookTokenExpired, status, body)
		case "SLOT_NOT_AVAILABLE", "SLOT_TAKEN", "SLOT_UNAVAILABLE":
			return wrapWithBody(providers.ErrSlotTaken, status, body)
		case "AUTH_REQUIRED", "AUTH_EXPIRED":
			return wrapWithBody(providers.ErrAuthExpired, status, body)
		case "RATE_LIMITED":
			return wrapWithBody(providers.ErrRateLimited, status, body)
		}
	}
	if status >= 500 && status < 600 {
		// 5xx is a generic upstream-broken; engine retries with backoff.
		return fmt.Errorf("resy: server error %d: %s", status, truncate(string(body), 256))
	}
	return fmt.Errorf("resy: unexpected status %d: %s", status, truncate(string(body), 256))
}

// classify2xx detects the inventory-empty and MFA-required patterns
// in 2xx responses. Returns nil when the body looks like a normal
// success.
func classify2xx(body []byte) error {
	if isMFARequiredBody(body) {
		return providers.ErrMFARequired
	}
	if isEmptyFindBody(body) {
		return providers.ErrInventoryEmpty
	}
	return nil
}

// wrapWithBody attaches the status code and a truncated body excerpt
// to a sentinel error so logs can pinpoint which response was
// classified, without leaking the entire payload at warn level.
func wrapWithBody(sentinel error, status int, body []byte) error {
	return fmt.Errorf("resy: status %d: %s: %w", status, truncate(string(body), 128), sentinel)
}

// isAntiBotBody is true when the body looks like an anti-bot
// challenge — CAPTCHA gates or "blocked" responses Resy returns when
// it has noticed unusual traffic.
func isAntiBotBody(body []byte) bool {
	lc := strings.ToLower(string(body))
	for _, marker := range []string{"captcha", "challenge", "denied", "blocked", "perimeterx", "px-captcha"} {
		if strings.Contains(lc, marker) {
			return true
		}
	}
	return false
}

// extractSpecificCode pulls the optional specific_code field that
// Resy attaches to error responses. Returns "" if the body is not
// JSON or has no specific_code.
func extractSpecificCode(body []byte) string {
	var env struct {
		SpecificCode string `json:"specific_code"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.SpecificCode
}

// isMFARequiredBody is true when an /3/auth/password response carries
// the mfa_required signal in addition to (or instead of) a JWT.
func isMFARequiredBody(body []byte) bool {
	var env struct {
		MFARequired bool `json:"mfa_required"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return false
	}
	return env.MFARequired
}

// isEmptyFindBody is true when a /4/find response decodes to a results
// envelope with zero venues — Resy's "no inventory at this time".
func isEmptyFindBody(body []byte) bool {
	var env struct {
		Results struct {
			Venues []struct{} `json:"venues"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return false
	}
	return len(env.Results.Venues) == 0 && bytes.Contains(body, []byte(`"venues"`))
}
