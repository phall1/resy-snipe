package resolver

import "errors"

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
)
