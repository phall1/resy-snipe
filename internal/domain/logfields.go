package domain

// Canonical structured-log field keys. Packages that emit slog records
// about a snipe must use these constants so log queries stay stable
// across the codebase. Tests assert these values do not change.
const (
	LogKeySnipeID       = "snipe_id"
	LogKeyVenueRef      = "venue_ref"
	LogKeyAttempt       = "attempt"
	LogKeyResyRequestID = "resy_request_id"
	LogKeyIntentHash    = "intent_hash"
	LogKeyProvider      = "provider"
)
