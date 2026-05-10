// Package resolver answers "who is this venue?" — it converts a
// domain.VenueQuery (a pasted Resy URL, a slug+city pair, or a
// freeform name) into the canonical identifiers the rest of the
// system uses. M1-02 ships only the URL-parsing piece: later
// milestones layer the /3/venue provider call (M1-03), the
// /venuesearch fallback (M1-04), and the SQLite cache (M1-05) on
// top. See docs/v2/design/resolver.md for the full algorithm.
package resolver
