package store

// v1LegacyMethodAllowlist lists Store methods that predate the v2
// multi-tenant data model (see docs/v2/design/multi-user.md
// §tenancy-enforcement and ADR-0005). The tenancy harness in
// tenant_check_test.go skips these methods when asserting that every
// Store method accepts a homelab domain.UserID.
//
// Two kinds of method appear here:
//
//  1. v1 bridge methods on the legacy schema (snipes, events,
//     sessions). M1-14 ported the v1 Store API onto the v2 schema for
//     backward compatibility. These methods either key on Resy
//     domain.UserID-as-email (the session row's owner) or on SnipeID
//     directly. They will be removed once the v2 Service layer fully
//     replaces the v1 engine call sites.
//
//  2. Cross-tenant tables called out by the design doc: venues and
//     observed_release_times are not per-user. They are global lookup
//     data; every Service caller reads the same rows. These methods
//     therefore have no userID to take. The design doc lists them
//     explicitly as tenancy exemptions.
//
// New v2 Store methods MUST NOT be added to this list. The whole
// point of the harness is that new methods either accept a homelab
// domain.UserID or fail the build under -tags=tenancy.
//
// The variable is referenced only by tenant_check_test.go which is
// gated on `//go:build tenancy`; without the tag the variable looks
// unused. The nolint silences `unused` for the untagged build; the
// tagged build uses the variable so the directive is redundant
// under `-tags=tenancy` and `nolintlint` is suppressed alongside.
//
//nolint:unused,nolintlint // see comment above.
var v1LegacyMethodAllowlist = []string{
	// --- v1 session bridge (M1-14) ---
	// UpsertSession keys on the Resy domain.UserID (email), not the
	// homelab user. Sessions are per-Resy-account credentials, not
	// per-homelab-user data. The v2 accounts table owns the
	// homelab->Resy mapping.
	"UpsertSession",
	// GetSession reads a single session by Resy account identity.
	// Same rationale as UpsertSession.
	"GetSession",
	// DeleteSession deletes by Resy account identity. Same rationale
	// as UpsertSession.
	"DeleteSession",

	// --- v1 snipe/event bridge ---
	// CreateSnipe/GetSnipe/UpdateSnipe/ListSnipesByStatus operate on
	// the v1 snipes table by SnipeID. The v2 quests/intents/runs
	// schema replaces this; until then the v1 methods stay
	// allowlisted.
	"CreateSnipe",
	"GetSnipe",
	"UpdateSnipe",
	"ListSnipesByStatus",
	// AppendEvent/ListEventsBySnipe operate on the v1 events table
	// keyed by SnipeID. The v2 quest_events peer is per-user; new
	// quest-event Store methods will not be allowlisted.
	"AppendEvent",
	"ListEventsBySnipe",
	// TransitionSnipe is the v1 atomic state-machine transition on
	// the snipes table. Same rationale as CreateSnipe.
	"TransitionSnipe",

	// --- cross-tenant tables (per design §tenancy-enforcement) ---
	// UpsertVenue and GetVenue read/write the global venues lookup
	// table. Venues are not per-user.
	"UpsertVenue",
	"GetVenue",
	// UpsertObserved and GetObserved read/write the global
	// observed_release_times lookup table. Observed release times
	// are not per-user.
	"UpsertObserved",
	"GetObserved",
	// UpsertVenueCache/GetVenueCache and UpsertNameSearchCache/
	// GetNameSearchCache read/write the cross-tenant resolver caches
	// (`venues_cache`, `name_search_cache`). Resy venue identity and
	// name-search hits are public data — any homelab user benefits
	// from a cached lookup, and no per-user information rides on
	// these rows. See docs/v2/design/multi-user.md §10 (cross-tenant
	// tables) and docs/v2/design/resolver.md §caching-contract.
	// These are package-level functions, not Store interface
	// methods, so the tenancy harness would not flag them even if
	// they were omitted here; the entries serve as documentation of
	// the cross-tenant exemption alongside their UpsertVenue peers.
	"UpsertVenueCache",
	"GetVenueCache",
	"UpsertNameSearchCache",
	"GetNameSearchCache",

	// --- infrastructure ---
	// DB returns the raw *sql.DB for tests. Not a user-data
	// operation; production code goes through the Store interface.
	"DB",
}
