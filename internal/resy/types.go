package resy

// Typed request/response shapes for Resy's REST endpoints. Every field
// the engine touches is named here; arbitrary JSON keys we don't read
// stay unrepresented so unmarshalling stays cheap.
//
// Endpoint methods that POST/GET against these structs land in their
// own files (R2: auth, R3: calendar, R4: find/details/book). R1 is
// the type surface plus the Client transit foundation.

// AuthRequest carries the form-encoded body of POST /3/auth/password.
type AuthRequest struct {
	Email    string
	Password string
}

// AuthResponse is the typed body of POST /3/auth/password. Token is the
// JWT the engine uses on every subsequent call until the JWT expires
// (~45d). PaymentMethodID is the user's default payment method, which
// /3/book wants in struct_payment_method.
type AuthResponse struct {
	ID        int64           `json:"id"`
	Email     string          `json:"em_address"`
	FirstName string          `json:"first_name"`
	LastName  string          `json:"last_name"`
	Token     string          `json:"token"`
	Mobile    string          `json:"mobile_number"`
	Payment   AuthPaymentBlob `json:"payment_method"`
}

// AuthPaymentBlob is the user's default payment method in the
// /3/auth/password response.
type AuthPaymentBlob struct {
	ID int64 `json:"id"`
}

// FindRequest carries the query parameters for GET /4/find.
type FindRequest struct {
	Day       string
	PartySize int
	VenueID   string
	Lat       string
	Long      string
}

// FindResponse is the typed body of GET /4/find.
type FindResponse struct {
	Results FindResults `json:"results"`
}

// FindResults wraps Resy's "results" envelope.
type FindResults struct {
	Venues []FindVenue `json:"venues"`
}

// FindVenue contains the slots a venue has open for the requested day.
type FindVenue struct {
	Slots []FindSlot     `json:"slots"`
	Venue FindVenueIdent `json:"venue"`
}

// FindVenueIdent identifies the venue the slots belong to.
type FindVenueIdent struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// FindSlot is one available reservation as returned by /4/find.
// Config.Token is the value passed as config_id to /3/details.
type FindSlot struct {
	Config FindSlotConfig `json:"config"`
	Date   FindSlotDate   `json:"date"`
}

// FindSlotConfig identifies the booking template for this slot.
type FindSlotConfig struct {
	Token string `json:"token"`
	Type  string `json:"type"`
	ID    int64  `json:"id"`
}

// FindSlotDate is the slot's wall-clock window (venue-local).
type FindSlotDate struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// DetailsRequest carries the query parameters for GET /3/details.
type DetailsRequest struct {
	ConfigID  string
	Day       string
	PartySize int
}

// DetailsResponse is the typed body of GET /3/details. BookToken's
// inner Value is the short-TTL token POST /3/book requires.
type DetailsResponse struct {
	BookToken DetailsBookToken `json:"book_token"`
	User      DetailsUser      `json:"user"`
}

// DetailsBookToken is the wrapper Resy uses around the book_token value.
type DetailsBookToken struct {
	Value string `json:"value"`
}

// DetailsUser holds payment methods on the /3/details response so the
// caller can pick a default for /3/book without a separate /3/auth
// round-trip.
type DetailsUser struct {
	PaymentMethods []DetailsPaymentMethod `json:"payment_methods"`
}

// DetailsPaymentMethod is one payment method on file.
type DetailsPaymentMethod struct {
	ID      int64  `json:"id"`
	Display string `json:"display"`
}

// BookRequest is the form-body of POST /3/book.
//
// IdempotencyKey, when non-empty, is sent as the X-Resy-Idempotency-Key
// HTTP header. Resy's documented idempotency surface uses a header,
// not a body field, so the request struct keeps it separate from the
// form data.
type BookRequest struct {
	BookToken           string
	StructPaymentMethod string
	IdempotencyKey      string
}

// BookResponse is the success body of POST /3/book.
type BookResponse struct {
	ResyToken     string `json:"resy_token"`
	ReservationID int64  `json:"reservation_id"`
}

// VenueResponse is the typed (and intentionally narrow) view of the
// GET /3/venue response. The Resy payload is large — gallery URLs,
// social links, hours-of-operation copy, and so on — so we only name
// the fields the resolver actually consumes. Unrepresented keys stay
// in the wire bytes and cost us nothing to ignore.
//
// VenueIdentBlock.Resy is the integer the rest of the system calls a
// "venue ref"; the resolver stringifies it before populating Venue.Ref.
//
// VenueLocale.TimeZone arrives as a POSIX TZ string (e.g. "EST5EDT")
// rather than the more familiar "America/New_York" IANA form. Go's
// time.LoadLocation accepts both, so the resolver passes the value
// through unchanged.
type VenueResponse struct {
	ID      VenueIdentBlock `json:"id"`
	Name    string          `json:"name"`
	URLSlug string          `json:"url_slug"`
	Locale  VenueLocale     `json:"locale"`
}

// VenueIdentBlock is the cross-platform identifier bag Resy attaches to
// every venue. Only the resy field matters to this codebase.
type VenueIdentBlock struct {
	Resy int64 `json:"resy"`
}

// VenueLocale carries currency and timezone metadata for the venue.
type VenueLocale struct {
	Currency string `json:"currency"`
	TimeZone string `json:"time_zone"`
}

// CalendarRequest carries the query parameters for GET /4/venue/calendar.
type CalendarRequest struct {
	VenueID   string
	NumSeats  int
	StartDate string
	EndDate   string
}

// CalendarResponse is the typed body of GET /4/venue/calendar.
type CalendarResponse struct {
	Scheduled []CalendarScheduled `json:"scheduled"`
}

// CalendarScheduled is one date in the venue's released window.
type CalendarScheduled struct {
	Date      string            `json:"date"`
	Inventory CalendarInventory `json:"inventory"`
}

// CalendarInventory is the per-day availability summary.
type CalendarInventory struct {
	Reservation string `json:"reservation"`
	EventTimes  string `json:"event_times,omitempty"`
}
