package providers

import (
	"context"
	"errors"
	"time"

	"resy-snipe/internal/domain"
)

// Credentials is the opaque per-provider login material. Resy's
// implementation (defined in internal/resy) carries email + password;
// future providers may carry an OAuth token, an API key, or anything
// else. Engine code treats this as a black box.
type Credentials interface {
	Provider() domain.ProviderID
}

// Session is the authenticated handle returned by Login. The engine
// must inspect ExpiresAt before reusing a session (JWT-based sessions
// have known TTLs and there is no refresh endpoint for Resy).
type Session interface {
	Provider() domain.ProviderID
	User() domain.UserID
	ExpiresAt() time.Time
}

// IsSessionExpired returns true when the session has passed (or
// reached) its expiry as measured against now. A zero ExpiresAt means
// the session does not expire.
func IsSessionExpired(s Session, now time.Time) bool {
	exp := s.ExpiresAt()
	if exp.IsZero() {
		return false
	}
	return !exp.After(now)
}

// Query parameterises SearchVenues. Phase 1 uses only Text; future
// expansion (filters, location bias) lands here.
type Query struct {
	Text string
}

// DateRange is an inclusive [Start, End] calendar window for a calendar
// query. PartySize is included because some providers (notably OpenTable)
// expose per-date availability as a party-size-dependent view: a date
// open for 2 may be closed for 6. A zero PartySize means the adapter
// picks a default (Resy's calendar endpoint is party-coarse and ignores
// it; OpenTable's per-day fan-out cannot).
type DateRange struct {
	Start, End domain.Date
	PartySize  int
}

// Calendar is the venue-availability view returned by Provider.Calendar.
// Provider implementations populate Days in chronological order; the
// far edge of the slice is the newly released horizon used by the
// DiscoveredRelease loop.
type Calendar struct {
	Venue domain.VenueRef
	Days  []CalendarDay
}

// CalendarDay describes a single date's availability.
type CalendarDay struct {
	Date      domain.Date
	Available bool
}

// FindRequest carries everything the engine needs to ask the provider
// for live slots. Session is optional; some endpoints accept anonymous
// requests, others embed user context.
type FindRequest struct {
	Venue     domain.VenueRef
	Date      domain.Date
	PartySize int
	Times     []domain.WallTime
	Session   Session
}

// Slot is one open reservation that matched a FindRequest. The engine
// orders them by user preference, fetches details for each (serially,
// per BookingPolicy), then races /3/book across the survivors.
//
// Date carries the venue-local calendar date so Book can re-issue
// /3/details for this slot without the engine threading the date
// separately.
type Slot struct {
	Venue     domain.VenueRef
	Date      domain.Date
	Time      domain.WallTime
	TableType string
	PartySize int
	Payload   domain.SlotPayload
}

// Confirmation is the receipt of a successful booking. The engine
// writes this onto SnipeState.Result.
type Confirmation struct {
	ID            string
	BookedAt      time.Time
	HumanReadable string
}

// Provider is the cross-provider seam. Concrete adapters live in
// sibling internal packages (currently only internal/resy). The
// engine depends on this interface and never on a concrete adapter.
//
// Implementations must classify failures into the sentinel taxonomy
// defined in this file: every returned error must either equal one of
// the sentinels or wrap one with %w so engine code can branch with
// errors.Is. Bare wall-clock errors that escape the taxonomy are a
// bug in the adapter, not a feature for callers to string-match.
type Provider interface {
	ID() domain.ProviderID
	Login(ctx context.Context, creds Credentials) (Session, error)
	Ping(ctx context.Context, sess Session) error
	SearchVenues(ctx context.Context, q Query) ([]domain.Venue, error)
	Calendar(ctx context.Context, ref domain.VenueRef, r DateRange) (Calendar, error)
	Find(ctx context.Context, req FindRequest) ([]Slot, error)
	Book(ctx context.Context, slot Slot, sess Session) (Confirmation, error)
}

// Sentinel errors. Engine branches on these via errors.Is; adapters
// must wrap any underlying transport error with one of these so the
// engine never has to string-match a body.
var (
	// ErrAuthExpired indicates a previously valid Session is no longer
	// accepted (Resy: HTTP 401 on a non-Login call). The engine should
	// re-Login and retry.
	ErrAuthExpired = errors.New("provider: auth expired")

	// ErrMFARequired indicates the credentials are valid but a
	// secondary factor is required to complete login. Provider impls
	// expose a per-provider CompleteMFA path.
	ErrMFARequired = errors.New("provider: MFA required")

	// ErrBookTokenExpired indicates the short-TTL book_token from
	// /3/details is no longer valid. The engine must re-walk
	// find→details→book.
	ErrBookTokenExpired = errors.New("provider: book token expired")

	// ErrSlotTaken indicates the requested slot was claimed before our
	// /3/book call won the race. The engine should fall through to
	// the next-priority slot (Booking → Finding).
	ErrSlotTaken = errors.New("provider: slot taken")

	// ErrRateLimited indicates the provider has thrown a rate-limit
	// response (Resy: HTTP 429). The engine backs off; this is a soft
	// failure.
	ErrRateLimited = errors.New("provider: rate limited")

	// ErrAntiBotChallenge indicates the provider's anti-bot system has
	// noticed us — a CAPTCHA, a challenge, an unusual block. The
	// engine treats this as terminal for the snipe and surfaces it.
	ErrAntiBotChallenge = errors.New("provider: anti-bot challenge")

	// ErrInventoryEmpty indicates a Find call succeeded but returned
	// no eligible slots. The engine loops in Awaiting/Finding until a
	// slot appears or the policy times out.
	ErrInventoryEmpty = errors.New("provider: inventory empty")
)
