package resolver

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"resy-snipe/internal/domain"
)

// resyHosts is the closed allow-list of hosts a Resy venue URL may
// carry. It mirrors domain.resyHosts on purpose: identity is decided
// here and re-validated by Goal.Validate so each layer is independent.
var resyHosts = map[string]struct{}{
	"resy.com":     {},
	"www.resy.com": {},
}

// URLHints carries the data extracted from a Resy venue URL beyond
// the canonical URL itself.
//
// Slug and City are required and always populated: they are the key
// the resolver feeds to /3/venue (url_slug=<Slug>&location=<City>) in
// M1-03, and the cache key the resolver consults in M1-05. They are
// stored lowercased and trimmed.
//
// Date and Party are optional: they reflect the ?date= and ?seats=
// query parameters Resy sprinkles on share links. Pointer types keep
// "absent" distinct from "zero" — a URL with seats=0 sets Party to a
// pointer to 0, which Goal.Validate will later reject; a URL with no
// seats= leaves Party nil. A malformed date or seats value is dropped
// silently so a bad query parameter never fails the URL parse.
type URLHints struct {
	// Slug is the venue path segment, e.g. "astoria-dc".
	Slug string
	// City is the city path segment, e.g. "washington-dc".
	City string
	// Date is the optional ?date=YYYY-MM-DD hint.
	Date *domain.Date
	// Party is the optional ?seats= hint.
	Party *int
}

// ParseResyURL parses a Resy venue URL and returns the canonical
// VenueQueryURL plus the parsed hints. The canonical URL strips the
// fragment and every query parameter, lowercases the host, and
// preserves the path verbatim (a trailing slash is kept if present).
//
// Errors are mutually exclusive sentinels: ErrInvalidURL when the
// input cannot be parsed at all, ErrNotResyHost when the host is
// foreign, ErrNotVenueURL when the path does not match the
// /cities/<city>/venues/<slug> shape. Each wrapped error includes a
// short detail string for log lines.
func ParseResyURL(raw string) (domain.VenueQueryURL, *URLHints, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return domain.VenueQueryURL{}, nil, fmt.Errorf("%w: empty input", ErrInvalidURL)
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return domain.VenueQueryURL{}, nil, fmt.Errorf("%w: %s", ErrInvalidURL, err.Error())
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return domain.VenueQueryURL{}, nil, fmt.Errorf("%w: scheme %q is not http(s)", ErrInvalidURL, u.Scheme)
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return domain.VenueQueryURL{}, nil, fmt.Errorf("%w: missing host", ErrInvalidURL)
	}
	if _, ok := resyHosts[host]; !ok {
		return domain.VenueQueryURL{}, nil, fmt.Errorf("%w: host %q", ErrNotResyHost, host)
	}

	slug, city, hadTrailingSlash, ok := splitVenuePath(u.Path)
	if !ok {
		return domain.VenueQueryURL{}, nil, fmt.Errorf("%w: path %q", ErrNotVenueURL, u.Path)
	}

	hints := &URLHints{
		Slug: slug,
		City: city,
	}
	if d, ok := parseDateParam(u.Query().Get("date")); ok {
		hints.Date = &d
	}
	if p, ok := parsePartyParam(u.Query().Get("seats")); ok {
		hints.Party = &p
	}

	canonical := canonicalURL(scheme, host, city, slug, hadTrailingSlash)
	return domain.VenueQueryURL{URL: canonical}, hints, nil
}

// splitVenuePath checks whether p matches /cities/<city>/venues/<slug>
// (optionally followed by extra segments) and returns the canonical
// (lowercased, trimmed) slug and city. The hadTrailingSlash flag lets
// the canonical URL preserve the user's trailing slash so two equal
// inputs round-trip to themselves.
func splitVenuePath(p string) (slug, city string, hadTrailingSlash, ok bool) {
	// Normalise: trim a single leading slash so Split works cleanly.
	trimmed := strings.TrimPrefix(p, "/")
	hadTrailingSlash = strings.HasSuffix(trimmed, "/") && trimmed != ""
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return "", "", false, false
	}
	parts := strings.Split(trimmed, "/")
	// Need at least: cities, <city>, venues, <slug>.
	if len(parts) < 4 {
		return "", "", false, false
	}
	if parts[0] != "cities" || parts[2] != "venues" {
		return "", "", false, false
	}
	rawCity := strings.TrimSpace(parts[1])
	rawSlug := strings.TrimSpace(parts[3])
	if rawCity == "" || rawSlug == "" {
		return "", "", false, false
	}
	return strings.ToLower(rawSlug), strings.ToLower(rawCity), hadTrailingSlash, true
}

// canonicalURL builds the stripped-tracking, lowercased-host form the
// planner persists. Scheme is preserved (http stays http) so a hash
// of the canonical URL still distinguishes the two; the host check is
// what enforces Resy identity.
func canonicalURL(scheme, host, city, slug string, trailingSlash bool) string {
	path := "/cities/" + city + "/venues/" + slug
	if trailingSlash {
		path += "/"
	}
	return scheme + "://" + host + path
}

// parseDateParam accepts a YYYY-MM-DD date hint. A malformed value
// returns ok=false; the caller drops the hint silently per the design
// rule "a bad query parameter never fails the URL parse."
func parseDateParam(s string) (domain.Date, bool) {
	if s == "" {
		return domain.Date{}, false
	}
	d, err := domain.ParseDate(s)
	if err != nil {
		return domain.Date{}, false
	}
	return d, true
}

// parsePartyParam accepts a non-negative integer seats hint. Zero is
// preserved so the caller can surface "seats=0" to Goal.Validate; a
// negative value or non-numeric string is dropped silently.
func parsePartyParam(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	if n < 0 {
		return 0, false
	}
	return n, true
}
