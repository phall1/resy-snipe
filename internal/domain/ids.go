package domain

// UserID identifies an authenticated user inside resy-snipe.
type UserID string

// ProviderID identifies a reservation backend (currently only "resy").
type ProviderID string

// SnipeID is the per-snipe identifier produced when a SnipeState is created.
type SnipeID string

// IntentHash is the stable content-addressable digest of an Intent.
type IntentHash string
