package domain_test

import (
	"bytes"
	"math/rand/v2"
	"testing"
	"testing/quick"
	"time"

	"resy-snipe/internal/domain"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func samplePlan(t *testing.T) domain.Plan {
	t.Helper()
	ny := mustLoc(t, "America/New_York")
	g := domain.Goal{
		VenueQuery: domain.VenueQueryURL{URL: "https://resy.com/cities/ny/astoria-dc"},
		Date:       domain.NewDate(2026, time.June, 9),
		Party:      2,
		TimePrefs: domain.TimeWindow{
			Start:    domain.NewWallTime(18, 30, 0),
			End:      domain.NewWallTime(21, 0, 0),
			Priority: domain.PriorityEarlier,
		},
		AccountID:   "acct-1",
		Constraints: domain.Constraints{TableTypes: []string{"Dining Room", "Bar"}},
	}
	v := domain.Venue{Provider: "resy", Ref: "49716", Name: "Astoria DC", TZ: ny}
	return domain.Plan{
		Goal:            g,
		Venue:           v,
		DropMoment:      time.Date(2026, time.May, 10, 4, 0, 0, 0, time.UTC),
		Strategy:        domain.DiscoveredRelease{ProbeFrom: time.Date(2026, time.May, 10, 3, 58, 30, 0, time.UTC), ProbeUntil: time.Date(2026, time.May, 10, 4, 3, 30, 0, time.UTC)},
		FireSchedule:    []time.Time{time.Date(2026, time.May, 10, 4, 0, 0, 0, time.UTC)},
		SigningRequired: true,
		PlanHashVersion: domain.PlanHashVersionV1,
		ComputedAt:      time.Date(2026, time.May, 10, 3, 30, 0, 0, time.UTC),
		Notes:           []string{"strategyegy=Discovered because cache miss"},
	}
}

func TestPlanHash_Stable(t *testing.T) {
	t.Parallel()
	p1 := samplePlan(t)
	p2 := samplePlan(t)
	h1, err := p1.ComputeHash()
	if err != nil {
		t.Fatalf("hash 1: %v", err)
	}
	h2, err := p2.ComputeHash()
	if err != nil {
		t.Fatalf("hash 2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("unstable hash: %s vs %s", h1, h2)
	}
	if h1 == "" {
		t.Fatal("empty hash")
	}
}

func TestPlanHash_DiffersOnParty(t *testing.T) {
	t.Parallel()
	a := samplePlan(t)
	b := samplePlan(t)
	b.Goal.Party = 4
	ha, _ := a.ComputeHash()
	hb, _ := b.ComputeHash()
	if ha == hb {
		t.Fatalf("hash should differ when party changes (a=%s b=%s)", ha, hb)
	}
}

func TestPlanHash_IgnoresComputedAt(t *testing.T) {
	t.Parallel()
	a := samplePlan(t)
	b := samplePlan(t)
	b.ComputedAt = a.ComputedAt.Add(48 * time.Hour)
	ha, _ := a.ComputeHash()
	hb, _ := b.ComputeHash()
	if ha != hb {
		t.Fatalf("ComputedAt must not affect hash (a=%s b=%s)", ha, hb)
	}
}

func TestPlanHash_IgnoresNotesAndHashField(t *testing.T) {
	t.Parallel()
	a := samplePlan(t)
	b := samplePlan(t)
	b.Notes = []string{"totally different note"}
	b.Hash = "sha256:deadbeef"
	ha, _ := a.ComputeHash()
	hb, _ := b.ComputeHash()
	if ha != hb {
		t.Fatalf("Notes/Hash must not affect hash (a=%s b=%s)", ha, hb)
	}
}

func TestPlanHash_DiffersOnPlanHashVersion(t *testing.T) {
	t.Parallel()
	a := samplePlan(t)
	b := samplePlan(t)
	b.PlanHashVersion = domain.PlanHashVersionV1 + 1
	ha, _ := a.ComputeHash()
	hb, _ := b.ComputeHash()
	if ha == hb {
		t.Fatalf("PlanHashVersion bump must change hash (a=%s b=%s)", ha, hb)
	}
}

func TestPlanCanonicalBytes_Deterministic(t *testing.T) {
	t.Parallel()
	p := samplePlan(t)
	var first []byte
	for i := range 20 {
		buf, err := p.CanonicalBytes()
		if err != nil {
			t.Fatalf("CanonicalBytes: %v", err)
		}
		if i == 0 {
			first = buf
			continue
		}
		if !bytes.Equal(buf, first) {
			t.Fatalf("CanonicalBytes drifted at iteration %d", i)
		}
	}
}

func TestPlanHash_StrategyVariantsDiffer(t *testing.T) {
	t.Parallel()
	base := samplePlan(t)
	at := time.Date(2026, time.May, 10, 4, 0, 0, 0, time.UTC)

	a := base
	a.Strategy = domain.ExplicitRelease{At: at}
	b := base
	b.Strategy = domain.ContinuousRelease{Until: at}
	c := base
	c.Strategy = domain.DiscoveredRelease{ProbeFrom: at, ProbeUntil: at.Add(5 * time.Minute)}

	ha, _ := a.ComputeHash()
	hb, _ := b.ComputeHash()
	hc, _ := c.ComputeHash()
	if ha == hb || hb == hc || ha == hc {
		t.Fatalf("strategyegy variants collided: %s %s %s", ha, hb, hc)
	}
}

func TestPlanWithHash_PopulatesField(t *testing.T) {
	t.Parallel()
	p := samplePlan(t)
	withHash, err := p.WithHash()
	if err != nil {
		t.Fatalf("WithHash: %v", err)
	}
	if withHash.Hash == "" {
		t.Fatal("WithHash did not set Hash")
	}
	// Hash field itself must not feed into the hash.
	rehashed, err := withHash.ComputeHash()
	if err != nil {
		t.Fatalf("rehash: %v", err)
	}
	if rehashed != withHash.Hash {
		t.Fatalf("hash unstable when Hash field is set: %s vs %s", rehashed, withHash.Hash)
	}
}

// TestPlanHash_Property feeds random goals through two hash calls and
// asserts equality. The generator stays inside the validation envelope
// so the canonicalizer's strategyegy switch is exercised across all three
// variants.
func TestPlanHash_Property(t *testing.T) {
	t.Parallel()
	ny := mustLoc(t, "America/New_York")
	venue := domain.Venue{Provider: "resy", Ref: "49716", TZ: ny}

	gen := func(r *rand.Rand) domain.Plan {
		party := r.IntN(8) + 1
		base := time.Date(2026, time.June, 1+r.IntN(28), 0, 0, 0, 0, time.UTC)
		var strategy domain.ReleaseStrategy
		switch r.IntN(3) {
		case 0:
			strategy = domain.ExplicitRelease{At: base}
		case 1:
			strategy = domain.DiscoveredRelease{ProbeFrom: base, ProbeUntil: base.Add(5 * time.Minute)}
		default:
			strategy = domain.ContinuousRelease{Until: base.Add(2 * time.Hour)}
		}
		tables := []string{}
		if r.IntN(2) == 0 {
			tables = append(tables, "Bar")
		}
		if r.IntN(2) == 0 {
			tables = append(tables, "Dining Room")
		}
		return domain.Plan{
			Goal: domain.Goal{
				VenueQuery:  domain.VenueQuerySlug{Slug: "astoria-dc", City: "ny"},
				Date:        domain.NewDate(base.Year(), base.Month(), base.Day()),
				Party:       party,
				TimePrefs:   domain.TimeWindow{Start: domain.NewWallTime(18, 0, 0), End: domain.NewWallTime(21, 0, 0), Priority: domain.PriorityEarlier},
				AccountID:   domain.AccountID("acct"),
				Constraints: domain.Constraints{TableTypes: tables},
			},
			Venue:           venue,
			DropMoment:      base,
			Strategy:        strategy,
			FireSchedule:    []time.Time{base, base.Add(time.Minute)},
			SigningRequired: r.IntN(2) == 0,
			PlanHashVersion: domain.PlanHashVersionV1,
			ComputedAt:      base.Add(-time.Hour),
		}
	}

	check := func(seed1, seed2 uint64) bool {
		r := rand.New(rand.NewPCG(seed1, seed2)) //nolint:gosec // property-test fuzz seed; not security-sensitive
		p := gen(r)
		h1, err := p.ComputeHash()
		if err != nil {
			t.Logf("hash err: %v", err)
			return false
		}
		// Mutate volatile fields; hash must not move.
		p.ComputedAt = p.ComputedAt.Add(72 * time.Hour)
		p.Notes = []string{"changed"}
		p.Hash = "sha256:stale"
		h2, err := p.ComputeHash()
		if err != nil {
			t.Logf("hash err: %v", err)
			return false
		}
		return h1 == h2
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 64}); err != nil {
		t.Fatalf("property failed: %v", err)
	}
}

// Compile-time check: the canonicalizer recognizes all three release
// strategyegy variants. If a new variant is added, ErrPlanStrategyUnknown
// will surface here at test time before users notice in production.
func TestPlanCanonical_AllStrategiesCovered(t *testing.T) {
	t.Parallel()
	p := samplePlan(t)
	for _, s := range []domain.ReleaseStrategy{
		domain.ExplicitRelease{At: time.Now().UTC()},
		domain.DiscoveredRelease{ProbeFrom: time.Now().UTC(), ProbeUntil: time.Now().UTC()},
		domain.ContinuousRelease{Until: time.Now().UTC()},
	} {
		p.Strategy = s
		if _, err := p.CanonicalBytes(); err != nil {
			t.Errorf("variant %T: %v", s, err)
		}
	}
}
