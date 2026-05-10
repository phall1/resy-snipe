package resolver

import (
	"errors"
	"strconv"

	"resy-snipe/internal/domain"
)

// Sentinel errors for URL parsing. Callers branch with errors.Is.
//
// ErrInvalidURL is the catch-all for inputs that net/url cannot parse
// or that lack the structural pieces a URL must have (scheme, host).
// ErrNotResyHost fires only after the URL is structurally valid but
// the host is not one of {resy.com, www.resy.com}.
// ErrNotVenueURL fires only after the host check passes but the path
// does not match the /cities/<city>/venues/<slug> shape.
//
// The three are mutually exclusive: a parse returns at most one.
var (
	// ErrInvalidURL means the input could not be parsed as a URL at
	// all, or is missing the scheme/host net/url could not infer.
	ErrInvalidURL = errors.New("resolver: invalid url")

	// ErrNotResyHost means the URL parsed but its host is not a
	// recognized Resy domain.
	ErrNotResyHost = errors.New("resolver: not a resy host")

	// ErrNotVenueURL means the URL is on a Resy host but its path
	// does not point at a venue page.
	ErrNotVenueURL = errors.New("resolver: not a venue url")

	// ErrVenueNotFound means the resolver definitively could not
	// identify the queried venue. For URL/slug queries this is the
	// provider's 404 surface (ResolveVenue returned
	// providers.ErrVenueNotFound). For name queries this is the
	// "zero candidates" signal from SearchVenuesByName. Terminal —
	// no retry helps.
	ErrVenueNotFound = errors.New("resolver: venue not found")

	// ErrVenueAmbiguous means a name query returned more than one
	// candidate and the resolver cannot pick a winner on the caller's
	// behalf. The concrete error value is *AmbiguousError; use
	// errors.As to recover the candidate list and errors.Is to test
	// the sentinel. See docs/v2/design/resolver.md §error-model.
	ErrVenueAmbiguous = errors.New("resolver: venue ambiguous")
)

// AmbiguousError carries the candidate list a name query produced.
// Service-layer code uses errors.As to extract it for picker UI;
// errors.Is(err, ErrVenueAmbiguous) returns true for any
// *AmbiguousError per the Is method below.
type AmbiguousError struct {
	Query      string
	Candidates []domain.Venue
}

// Error returns a stable single-line summary of the ambiguity.
func (e *AmbiguousError) Error() string {
	return "resolver: venue ambiguous: " + e.Query +
		" (" + strconv.Itoa(len(e.Candidates)) + " candidates)"
}

// Is reports whether target is the ErrVenueAmbiguous sentinel.
func (e *AmbiguousError) Is(target error) bool {
	return target == ErrVenueAmbiguous
}
