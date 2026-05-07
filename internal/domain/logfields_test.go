package domain_test

import (
	"testing"

	"resy-snipe/internal/domain"
)

// Lock the canonical key set. If a key value changes, log queries that
// reference it break across the codebase — this test is the canary.
func TestLogFieldKeys(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"snipe_id":        domain.LogKeySnipeID,
		"venue_ref":       domain.LogKeyVenueRef,
		"attempt":         domain.LogKeyAttempt,
		"resy_request_id": domain.LogKeyResyRequestID,
		"intent_hash":     domain.LogKeyIntentHash,
		"provider":        domain.LogKeyProvider,
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("log key drifted: got %q want %q", got, want)
		}
	}
}
