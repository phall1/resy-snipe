package domain

// UserID identifies an authenticated user inside resy-snipe.
type UserID string

// ProviderID identifies a reservation backend (currently only "resy").
type ProviderID string

// SnipeID is the per-snipe identifier produced when a SnipeState is created.
type SnipeID string

// IntentHash is the stable content-addressable digest of an Intent.
type IntentHash string

// AccountID identifies a Resy account (the credentialed login that
// pays for a reservation). Per ADR-0001 a Goal carries the account so
// per-account rate limits and signer sessions resolve unambiguously.
type AccountID string

// QuestID identifies a v2 Quest — the persistent record of a user's
// Goal, the Plans we've derived from it, and the engine Runs we've
// scheduled. Distinct from SnipeID, which names a single engine Run.
type QuestID string

// SubscriptionID identifies a persistent subscription hunt.
type SubscriptionID string
