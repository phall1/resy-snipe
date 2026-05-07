package resy

import (
	"errors"
	"net/http"
	"testing"

	"resy-snipe/internal/providers"
)

func TestClassifyTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		status   int
		body     string
		wantSent error
		wantNil  bool
	}{
		{
			name:     "401 unauthorized",
			status:   http.StatusUnauthorized,
			body:     `{"message":"signature expired"}`,
			wantSent: providers.ErrAuthExpired,
		},
		{
			name:     "401 with specific message",
			status:   http.StatusUnauthorized,
			body:     `{"specific_message":"Authentication header invalid"}`,
			wantSent: providers.ErrAuthExpired,
		},
		{
			name:     "429 too many requests",
			status:   http.StatusTooManyRequests,
			body:     `{"message":"rate limit"}`,
			wantSent: providers.ErrRateLimited,
		},
		{
			name:     "410 gone implies book token expired",
			status:   http.StatusGone,
			body:     `{"message":"token expired"}`,
			wantSent: providers.ErrBookTokenExpired,
		},
		{
			name:     "409 conflict implies slot taken",
			status:   http.StatusConflict,
			body:     `{"message":"slot taken"}`,
			wantSent: providers.ErrSlotTaken,
		},
		{
			name:     "403 with captcha marker = anti-bot",
			status:   http.StatusForbidden,
			body:     `{"message":"please solve captcha"}`,
			wantSent: providers.ErrAntiBotChallenge,
		},
		{
			name:     "403 with challenge marker = anti-bot",
			status:   http.StatusForbidden,
			body:     `{"message":"px-captcha challenge required"}`,
			wantSent: providers.ErrAntiBotChallenge,
		},
		{
			name:     "403 with no anti-bot marker = auth-expired fallback",
			status:   http.StatusForbidden,
			body:     `{"message":"forbidden"}`,
			wantSent: providers.ErrAuthExpired,
		},
		{
			name:     "specific_code BOOK_TOKEN_EXPIRED on 400",
			status:   http.StatusBadRequest,
			body:     `{"specific_code":"BOOK_TOKEN_EXPIRED"}`,
			wantSent: providers.ErrBookTokenExpired,
		},
		{
			name:     "specific_code SLOT_NOT_AVAILABLE on 400",
			status:   http.StatusBadRequest,
			body:     `{"specific_code":"SLOT_NOT_AVAILABLE"}`,
			wantSent: providers.ErrSlotTaken,
		},
		{
			name:     "specific_code RATE_LIMITED on 400",
			status:   http.StatusBadRequest,
			body:     `{"specific_code":"RATE_LIMITED"}`,
			wantSent: providers.ErrRateLimited,
		},
		{
			name:     "specific_code AUTH_REQUIRED on 400",
			status:   http.StatusBadRequest,
			body:     `{"specific_code":"AUTH_REQUIRED"}`,
			wantSent: providers.ErrAuthExpired,
		},
		{
			name:     "200 with mfa_required",
			status:   http.StatusOK,
			body:     `{"mfa_required":true,"challenge_id":"chal-1"}`,
			wantSent: providers.ErrMFARequired,
		},
		{
			name:     "200 with empty find venues",
			status:   http.StatusOK,
			body:     `{"results":{"venues":[]}}`,
			wantSent: providers.ErrInventoryEmpty,
		},
		{
			name:    "200 with populated find venues — nil",
			status:  http.StatusOK,
			body:    `{"results":{"venues":[{"venue":{"id":1}}]}}`,
			wantNil: true,
		},
		{
			name:    "200 success with no relevant pattern — nil",
			status:  http.StatusOK,
			body:    `{"id":42,"token":"jwt"}`,
			wantNil: true,
		},
		{
			name:    "201 created — nil",
			status:  http.StatusCreated,
			body:    `{"resy_token":"tok"}`,
			wantNil: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := classify(c.status, []byte(c.body))
			if c.wantNil {
				if got != nil {
					t.Fatalf("classify=%v, want nil", got)
				}
				return
			}
			if !errors.Is(got, c.wantSent) {
				t.Fatalf("classify=%v, want errors.Is %v", got, c.wantSent)
			}
		})
	}
}

func TestClassifyServerErrorWrapped(t *testing.T) {
	t.Parallel()
	got := classify(http.StatusInternalServerError, []byte(`oops`))
	if got == nil {
		t.Fatal("classify(500)=nil, want wrapped error")
	}
	// 5xx is NOT one of the sentinels — engine retries with backoff
	// based on the error message, not classification.
	for _, sentinel := range []error{
		providers.ErrAuthExpired,
		providers.ErrSlotTaken,
		providers.ErrBookTokenExpired,
		providers.ErrRateLimited,
		providers.ErrAntiBotChallenge,
		providers.ErrInventoryEmpty,
		providers.ErrMFARequired,
	} {
		if errors.Is(got, sentinel) {
			t.Errorf("5xx must not classify to %v", sentinel)
		}
	}
}

func TestClassifyUnknown4xxIsWrappedNotSentinel(t *testing.T) {
	t.Parallel()
	got := classify(http.StatusTeapot, []byte(`{}`))
	if got == nil {
		t.Fatal("classify(418)=nil, want wrapped error")
	}
	for _, sentinel := range []error{
		providers.ErrAuthExpired,
		providers.ErrSlotTaken,
	} {
		if errors.Is(got, sentinel) {
			t.Errorf("418 must not classify to %v", sentinel)
		}
	}
}

func TestClassifyHandlesNonJSONBody(t *testing.T) {
	t.Parallel()
	// 401 with HTML body (anti-bot or proxy interception). Status
	// should still classify; the JSON-only helpers must not panic.
	got := classify(http.StatusUnauthorized, []byte(`<html>blocked</html>`))
	if !errors.Is(got, providers.ErrAuthExpired) {
		t.Fatalf("got %v, want ErrAuthExpired", got)
	}
}
