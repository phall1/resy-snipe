package resolver

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"resy-snipe/internal/domain"
)

func TestParseResyURL_RealWorldWithTracking(t *testing.T) {
	t.Parallel()
	raw := "https://resy.com/cities/washington-dc/venues/astoria-dc" +
		"?extlink=ps-US-Resy-Brand-Conversion-Evergreen" +
		"&utm_source=google&utm_medium=paid_search" +
		"&date=2026-05-10&seats=2"

	q, hints, err := ParseResyURL(raw)
	if err != nil {
		t.Fatalf("ParseResyURL: %v", err)
	}
	if hints == nil {
		t.Fatal("expected hints, got nil")
	}
	if hints.Slug != "astoria-dc" {
		t.Errorf("slug = %q, want astoria-dc", hints.Slug)
	}
	if hints.City != "washington-dc" {
		t.Errorf("city = %q, want washington-dc", hints.City)
	}
	if hints.Date == nil || *hints.Date != domain.NewDate(2026, time.May, 10) {
		t.Errorf("date hint = %v, want 2026-05-10", hints.Date)
	}
	if hints.Party == nil || *hints.Party != 2 {
		t.Errorf("party hint = %v, want 2", hints.Party)
	}
	// Canonical URL should be tracking-free.
	want := "https://resy.com/cities/washington-dc/venues/astoria-dc"
	if q.URL != want {
		t.Errorf("canonical URL = %q, want %q", q.URL, want)
	}
}

func TestParseResyURL_BareURL_NoHints(t *testing.T) {
	t.Parallel()
	raw := "https://resy.com/cities/washington-dc/venues/astoria-dc"
	q, hints, err := ParseResyURL(raw)
	if err != nil {
		t.Fatalf("ParseResyURL: %v", err)
	}
	if hints == nil {
		t.Fatal("expected hints (with slug/city), got nil")
	}
	if hints.Date != nil {
		t.Errorf("date hint = %v, want nil", *hints.Date)
	}
	if hints.Party != nil {
		t.Errorf("party hint = %v, want nil", *hints.Party)
	}
	if q.URL != raw {
		t.Errorf("canonical URL = %q, want %q", q.URL, raw)
	}
}

func TestParseResyURL_SeatsOnlyHint(t *testing.T) {
	t.Parallel()
	raw := "https://resy.com/cities/washington-dc/venues/astoria-dc?seats=4"
	_, hints, err := ParseResyURL(raw)
	if err != nil {
		t.Fatalf("ParseResyURL: %v", err)
	}
	if hints.Date != nil {
		t.Errorf("date hint = %v, want nil", *hints.Date)
	}
	if hints.Party == nil || *hints.Party != 4 {
		t.Errorf("party hint = %v, want 4", hints.Party)
	}
}

func TestParseResyURL_TrailingSlash(t *testing.T) {
	t.Parallel()
	raw := "https://resy.com/cities/washington-dc/venues/astoria-dc/"
	q, hints, err := ParseResyURL(raw)
	if err != nil {
		t.Fatalf("ParseResyURL: %v", err)
	}
	if hints.Slug != "astoria-dc" || hints.City != "washington-dc" {
		t.Errorf("slug/city = %q/%q, want astoria-dc/washington-dc",
			hints.Slug, hints.City)
	}
	if !strings.HasSuffix(q.URL, "/astoria-dc/") {
		t.Errorf("canonical URL = %q, want trailing slash preserved", q.URL)
	}
}

func TestParseResyURL_HTTPScheme(t *testing.T) {
	t.Parallel()
	raw := "http://resy.com/cities/washington-dc/venues/astoria-dc"
	q, _, err := ParseResyURL(raw)
	if err != nil {
		t.Fatalf("http should parse: %v", err)
	}
	if !strings.HasPrefix(q.URL, "http://") {
		t.Errorf("canonical URL = %q, want http:// preserved", q.URL)
	}
}

func TestParseResyURL_WWWHost(t *testing.T) {
	t.Parallel()
	raw := "https://www.resy.com/cities/ny/venues/carbone"
	q, hints, err := ParseResyURL(raw)
	if err != nil {
		t.Fatalf("www.resy.com should parse: %v", err)
	}
	if hints.Slug != "carbone" || hints.City != "ny" {
		t.Errorf("slug/city = %q/%q", hints.Slug, hints.City)
	}
	if !strings.Contains(q.URL, "www.resy.com") {
		t.Errorf("canonical URL = %q, want www.resy.com", q.URL)
	}
}

func TestParseResyURL_MixedCaseHost(t *testing.T) {
	t.Parallel()
	raw := "https://Resy.COM/cities/ny/venues/carbone"
	q, _, err := ParseResyURL(raw)
	if err != nil {
		t.Fatalf("mixed-case host should parse: %v", err)
	}
	if !strings.Contains(q.URL, "resy.com") || strings.Contains(q.URL, "Resy") {
		t.Errorf("canonical URL = %q, want lowercased host", q.URL)
	}
}

func TestParseResyURL_WrongHost(t *testing.T) {
	t.Parallel()
	raw := "https://opentable.com/cities/ny/venues/carbone"
	_, _, err := ParseResyURL(raw)
	if !errors.Is(err, ErrNotResyHost) {
		t.Errorf("err = %v, want ErrNotResyHost", err)
	}
}

func TestParseResyURL_WrongPath(t *testing.T) {
	t.Parallel()
	cases := []string{
		"https://resy.com/about/",
		"https://resy.com/",
		"https://resy.com",
		"https://resy.com/cities/washington-dc/events/something",
		"https://resy.com/cities/washington-dc/venues",   // no slug
		"https://resy.com/cities//venues/astoria",        // empty city
		"https://resy.com/cities/washington-dc/venues//", // empty slug
	}
	for _, raw := range cases {
		_, _, err := ParseResyURL(raw)
		if !errors.Is(err, ErrNotVenueURL) {
			t.Errorf("ParseResyURL(%q): err = %v, want ErrNotVenueURL", raw, err)
		}
	}
}

func TestParseResyURL_Garbage(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"   ",
		"::not a url::",
		"ftp://resy.com/cities/x/venues/y",
		"javascript:alert(1)",
		"not-a-url",
		"//resy.com/cities/x/venues/y", // missing scheme
	}
	for _, raw := range cases {
		_, _, err := ParseResyURL(raw)
		if !errors.Is(err, ErrInvalidURL) {
			t.Errorf("ParseResyURL(%q): err = %v, want ErrInvalidURL", raw, err)
		}
	}
}

func TestParseResyURL_SeatsZero(t *testing.T) {
	t.Parallel()
	raw := "https://resy.com/cities/washington-dc/venues/astoria-dc?seats=0"
	_, hints, err := ParseResyURL(raw)
	if err != nil {
		t.Fatalf("ParseResyURL: %v", err)
	}
	if hints.Party == nil {
		t.Fatal("expected Party hint set to 0, got nil")
	}
	if *hints.Party != 0 {
		t.Errorf("party hint = %d, want 0", *hints.Party)
	}
}

func TestParseResyURL_MalformedHints(t *testing.T) {
	t.Parallel()
	cases := []string{
		"https://resy.com/cities/washington-dc/venues/astoria-dc?date=not-a-date",
		"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-13-40",
		"https://resy.com/cities/washington-dc/venues/astoria-dc?seats=many",
		"https://resy.com/cities/washington-dc/venues/astoria-dc?seats=-3",
		"https://resy.com/cities/washington-dc/venues/astoria-dc?date=&seats=",
	}
	for _, raw := range cases {
		_, hints, err := ParseResyURL(raw)
		if err != nil {
			t.Errorf("ParseResyURL(%q): err = %v, want nil", raw, err)
			continue
		}
		if hints.Date != nil {
			t.Errorf("ParseResyURL(%q): date hint = %v, want nil", raw, *hints.Date)
		}
		if hints.Party != nil {
			t.Errorf("ParseResyURL(%q): party hint = %v, want nil", raw, *hints.Party)
		}
	}
}

func TestParseResyURL_CanonicalLowercase(t *testing.T) {
	t.Parallel()
	// Path segments arrive in mixed case; canonical form lowercases them.
	raw := "https://resy.com/cities/Washington-DC/venues/Astoria-DC"
	_, hints, err := ParseResyURL(raw)
	if err != nil {
		t.Fatalf("ParseResyURL: %v", err)
	}
	if hints.Slug != "astoria-dc" || hints.City != "washington-dc" {
		t.Errorf("slug/city = %q/%q, want astoria-dc/washington-dc",
			hints.Slug, hints.City)
	}
}

func TestParseResyURL_ExtraPathSegments(t *testing.T) {
	t.Parallel()
	// The path may carry trailing segments past the venue slug (e.g. a
	// Resy SPA route appended on top); the parser ignores them.
	raw := "https://resy.com/cities/washington-dc/venues/astoria-dc/reserve"
	_, hints, err := ParseResyURL(raw)
	if err != nil {
		t.Fatalf("ParseResyURL: %v", err)
	}
	if hints.Slug != "astoria-dc" || hints.City != "washington-dc" {
		t.Errorf("slug/city = %q/%q", hints.Slug, hints.City)
	}
}

// TestParseResyURL_RoundTrip is a property test: for any randomly
// generated (city, slug) pair that survives canonicalisation, the URL
// built from them parses back to those same canonical values.
func TestParseResyURL_RoundTrip(t *testing.T) {
	t.Parallel()
	roundTrip := func(citySeed, slugSeed string) bool {
		city := canonicaliseSegment(citySeed)
		slug := canonicaliseSegment(slugSeed)
		if city == "" || slug == "" {
			return true // unrepresentable inputs are out of scope
		}
		raw := "https://resy.com/cities/" + url.PathEscape(city) +
			"/venues/" + url.PathEscape(slug)
		_, hints, err := ParseResyURL(raw)
		if err != nil {
			t.Logf("round-trip failure: raw=%q err=%v", raw, err)
			return false
		}
		if hints.City != city {
			t.Logf("city: got %q, want %q (raw=%q)", hints.City, city, raw)
			return false
		}
		if hints.Slug != slug {
			t.Logf("slug: got %q, want %q (raw=%q)", hints.Slug, slug, raw)
			return false
		}
		return true
	}
	if err := quick.Check(roundTrip, &quick.Config{MaxCount: 200}); err != nil {
		t.Error(err)
	}
}

// canonicaliseSegment reduces a quick-generated string to the same
// shape the parser produces: lowercased ASCII letters, digits, and
// dashes. Empty results signal "skip this case."
func canonicaliseSegment(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '-':
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// Sanity: the sentinels are not equal to each other so errors.Is is
// meaningful. Cheap to verify; protects against a refactor flattening
// them.
func TestSentinelsAreDistinct(t *testing.T) {
	t.Parallel()
	sentinels := []error{ErrInvalidURL, ErrNotResyHost, ErrNotVenueURL}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("sentinel[%d] errors.Is sentinel[%d]: %v vs %v", i, j, a, b)
			}
		}
	}
}

// Ensure ParseResyURL's returned VenueQueryURL still satisfies the
// VenueQuery sealed interface; the compile-time check lives in
// internal/domain, but exercising it through the resolver guards
// against accidental signature drift.
func TestParseResyURL_ReturnsVenueQuery(t *testing.T) {
	t.Parallel()
	q, _, err := ParseResyURL("https://resy.com/cities/ny/venues/carbone")
	if err != nil {
		t.Fatalf("ParseResyURL: %v", err)
	}
	var _ domain.VenueQuery = q
	got := q.String()
	if !strings.HasPrefix(got, "url=") {
		t.Errorf("VenueQueryURL.String() = %q, want url= prefix", got)
	}
}
