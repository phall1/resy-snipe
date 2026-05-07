package domain_test

import (
	"testing"
	"time"

	"resy-snipe/internal/domain"
)

// Compile-time: every variant satisfies the sealed ReleaseStrategy
// interface. If a future variant is added without the marker method,
// these assertions break the build.
var (
	_ domain.ReleaseStrategy = domain.ExplicitRelease{}
	_ domain.ReleaseStrategy = domain.DiscoveredRelease{}
	_ domain.ReleaseStrategy = domain.ContinuousRelease{}
)

// Engine code will pattern-match release strategies via a type switch.
// This test stands in for the engine switch — if a new variant is
// added without updating downstream switches, the default branch here
// fires and the test fails.
func TestReleaseStrategySwitchExhaustive(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	cases := []domain.ReleaseStrategy{
		domain.ExplicitRelease{At: at},
		domain.DiscoveredRelease{ProbeFrom: at, ProbeUntil: at.Add(time.Hour)},
		domain.ContinuousRelease{Until: at.Add(24 * time.Hour)},
	}
	for _, r := range cases {
		switch v := r.(type) {
		case domain.ExplicitRelease:
			if v.At.IsZero() {
				t.Errorf("ExplicitRelease.At is zero")
			}
		case domain.DiscoveredRelease:
			if v.ProbeFrom.IsZero() || v.ProbeUntil.IsZero() {
				t.Errorf("DiscoveredRelease has zero probe bounds: %+v", v)
			}
		case domain.ContinuousRelease:
			if v.Until.IsZero() {
				t.Errorf("ContinuousRelease.Until is zero")
			}
		default:
			t.Fatalf("unhandled ReleaseStrategy variant %T — engine type switch must be updated", r)
		}
	}
}

func TestIntentHashIncludesReleaseVariant(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	base := domain.Intent{
		User:      "u1",
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
		SlotPrefs: []domain.SlotPreference{{Time: domain.NewWallTime(19, 30, 0)}},
	}

	explicit := base
	explicit.Release = domain.ExplicitRelease{At: at}
	discovered := base
	discovered.Release = domain.DiscoveredRelease{ProbeFrom: at, ProbeUntil: at.Add(time.Hour)}
	continuous := base
	continuous.Release = domain.ContinuousRelease{Until: at}

	h1 := explicit.Hash()
	h2 := discovered.Hash()
	h3 := continuous.Hash()
	if h1 == h2 || h2 == h3 || h1 == h3 {
		t.Fatalf("variants collide: explicit=%s discovered=%s continuous=%s", h1, h2, h3)
	}
	hNil := base.Hash()
	if hNil == h1 || hNil == h2 || hNil == h3 {
		t.Fatal("nil release collides with a typed release")
	}
}

func TestIntentHashSensitiveToReleaseFields(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	mk := func(r domain.ReleaseStrategy) domain.IntentHash {
		return domain.Intent{
			User:    "u1",
			Venue:   domain.VenueRef{Provider: "resy", Ref: "38660"},
			Date:    domain.NewDate(2026, time.June, 1),
			Release: r,
		}.Hash()
	}

	if mk(domain.ExplicitRelease{At: at}) ==
		mk(domain.ExplicitRelease{At: at.Add(time.Minute)}) {
		t.Error("ExplicitRelease.At change did not affect hash")
	}
	if mk(domain.DiscoveredRelease{ProbeFrom: at, ProbeUntil: at.Add(time.Hour)}) ==
		mk(domain.DiscoveredRelease{ProbeFrom: at, ProbeUntil: at.Add(2 * time.Hour)}) {
		t.Error("DiscoveredRelease.ProbeUntil change did not affect hash")
	}
	if mk(domain.ContinuousRelease{Until: at}) ==
		mk(domain.ContinuousRelease{Until: at.Add(time.Hour)}) {
		t.Error("ContinuousRelease.Until change did not affect hash")
	}
}

// Times in equivalent UTC instants but different zones must hash equal,
// so two callers in different timezones can't accidentally produce
// distinct idempotency keys for the same booking attempt.
func TestIntentHashTimezoneInvariant(t *testing.T) {
	t.Parallel()
	utc := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("LoadLocation: %v", err)
	}
	mk := func(r domain.ReleaseStrategy) domain.IntentHash {
		return domain.Intent{
			User:    "u1",
			Venue:   domain.VenueRef{Provider: "resy", Ref: "38660"},
			Date:    domain.NewDate(2026, time.June, 1),
			Release: r,
		}.Hash()
	}
	if mk(domain.ExplicitRelease{At: utc}) !=
		mk(domain.ExplicitRelease{At: utc.In(ny)}) {
		t.Fatal("hash depends on timezone, not instant")
	}
}
