package store_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/store"
)

func newStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	db := openMigrated(t)
	return store.NewSQLiteStore(db)
}

func sampleIntent(t *testing.T) domain.Intent {
	t.Helper()
	return domain.Intent{
		User:      "u1",
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
		SlotPrefs: []domain.SlotPreference{
			{Time: domain.NewWallTime(19, 30, 0), TableType: ""},
			{Time: domain.NewWallTime(20, 0, 0), TableType: "Bar"},
		},
		Release: domain.ExplicitRelease{At: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)},
	}
}

func TestCreateAndGetSnipe(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	state := domain.NewSnipeState("snp_1", sampleIntent(t), now)
	state.ScheduledAt = now.Add(time.Hour)
	if err := s.CreateSnipe(ctx, state); err != nil {
		t.Fatalf("CreateSnipe: %v", err)
	}

	got, err := s.GetSnipe(ctx, "snp_1")
	if err != nil {
		t.Fatalf("GetSnipe: %v", err)
	}
	if got.ID != state.ID {
		t.Errorf("ID: %s vs %s", got.ID, state.ID)
	}
	if got.Status() != domain.StatusSubmitted {
		t.Errorf("status: %s", got.Status())
	}
	if got.Intent.Hash() != state.Intent.Hash() {
		t.Errorf("intent hash drifted on round-trip")
	}
	if !got.ScheduledAt.Equal(state.ScheduledAt) {
		t.Errorf("scheduled_at: got %v want %v", got.ScheduledAt, state.ScheduledAt)
	}
}

func TestGetSnipeNotFound(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.GetSnipe(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateSnipe(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	state := domain.NewSnipeState("snp_2", sampleIntent(t), now)
	if err := s.CreateSnipe(ctx, state); err != nil {
		t.Fatalf("CreateSnipe: %v", err)
	}

	if err := state.Transition(domain.StatusScheduled); err != nil {
		t.Fatalf("transition: %v", err)
	}
	state.UpdatedAt = now.Add(time.Minute)
	if err := s.UpdateSnipe(ctx, state); err != nil {
		t.Fatalf("UpdateSnipe: %v", err)
	}

	got, err := s.GetSnipe(ctx, "snp_2")
	if err != nil {
		t.Fatalf("GetSnipe after update: %v", err)
	}
	if got.Status() != domain.StatusScheduled {
		t.Errorf("status after update: %s", got.Status())
	}
}

func TestUpdateSnipeMissingReturnsNotFound(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	state := domain.NewSnipeState("ghost", sampleIntent(t), now)
	err := s.UpdateSnipe(context.Background(), state)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestListSnipesByStatus(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

	a := domain.NewSnipeState("snp_a", sampleIntent(t), now)
	b := domain.NewSnipeState("snp_b", sampleIntent(t), now.Add(time.Second))
	if err := b.Transition(domain.StatusScheduled); err != nil {
		t.Fatal(err)
	}
	c := domain.NewSnipeState("snp_c", sampleIntent(t), now.Add(2*time.Second))
	if err := c.Transition(domain.StatusScheduled); err != nil {
		t.Fatal(err)
	}

	for _, st := range []*domain.SnipeState{a, b, c} {
		if err := s.CreateSnipe(ctx, st); err != nil {
			t.Fatalf("CreateSnipe: %v", err)
		}
	}
	// Need to push b/c to scheduled in DB.
	if err := s.UpdateSnipe(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSnipe(ctx, c); err != nil {
		t.Fatal(err)
	}

	scheduled, err := s.ListSnipesByStatus(ctx, domain.StatusScheduled)
	if err != nil {
		t.Fatalf("ListSnipesByStatus: %v", err)
	}
	if len(scheduled) != 2 {
		t.Fatalf("expected 2 scheduled, got %d", len(scheduled))
	}
	// Order is by created_at asc; b was created before c.
	if scheduled[0].ID != "snp_b" || scheduled[1].ID != "snp_c" {
		t.Errorf("ordering off: %s, %s", scheduled[0].ID, scheduled[1].ID)
	}

	submitted, err := s.ListSnipesByStatus(ctx, domain.StatusSubmitted)
	if err != nil {
		t.Fatal(err)
	}
	if len(submitted) != 1 || submitted[0].ID != "snp_a" {
		t.Errorf("expected only snp_a submitted, got %v", submitted)
	}
}

func TestEventsRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	state := domain.NewSnipeState("snp_evt", sampleIntent(t), now)
	if err := s.CreateSnipe(ctx, state); err != nil {
		t.Fatal(err)
	}

	at := now.Add(time.Minute)
	events := []domain.Event{
		{
			Type: domain.EventSubmitted,
			At:   at,
			Attrs: []slog.Attr{
				slog.String(domain.LogKeySnipeID, "snp_evt"),
				slog.Int(domain.LogKeyAttempt, 1),
				slog.Bool("dry_run", false),
				slog.Time("scheduled_at", now.Add(time.Hour).UTC()),
			},
		},
		{
			Type:  domain.EventBookAttempted,
			At:    at.Add(time.Second),
			Attrs: []slog.Attr{slog.String(domain.LogKeyResyRequestID, "req_xyz")},
		},
	}
	for _, e := range events {
		if err := s.AppendEvent(ctx, "snp_evt", e); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	got, err := s.ListEventsBySnipe(ctx, "snp_evt")
	if err != nil {
		t.Fatalf("ListEventsBySnipe: %v", err)
	}
	if len(got) != len(events) {
		t.Fatalf("expected %d events, got %d", len(events), len(got))
	}

	// Type and time must round-trip exactly.
	for i, want := range events {
		if got[i].Type != want.Type {
			t.Errorf("event %d type: %s vs %s", i, got[i].Type, want.Type)
		}
		if !got[i].At.Equal(want.At) {
			t.Errorf("event %d at: %v vs %v", i, got[i].At, want.At)
		}
		if len(got[i].Attrs) != len(want.Attrs) {
			t.Errorf("event %d attr count: %d vs %d", i, len(got[i].Attrs), len(want.Attrs))
		}
	}

	// Spot-check that a typed Attr round-trips with its kind preserved
	// for the kinds we encode explicitly.
	first := got[0]
	for _, a := range first.Attrs {
		switch a.Key {
		case domain.LogKeyAttempt:
			if a.Value.Kind() != slog.KindInt64 || a.Value.Int64() != 1 {
				t.Errorf("attempt attr lost type/value: kind=%v val=%v", a.Value.Kind(), a.Value)
			}
		case "dry_run":
			if a.Value.Kind() != slog.KindBool || a.Value.Bool() {
				t.Errorf("dry_run attr lost type/value")
			}
		case "scheduled_at":
			if a.Value.Kind() != slog.KindTime {
				t.Errorf("scheduled_at attr lost time kind: %v", a.Value.Kind())
			}
		}
	}
}

func TestSessionsHappyPath(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	// Create the user first to satisfy the FK.
	db := openMigrated(t)
	_ = db // already opened by newStore via openMigrated
	if err := storeUser(s, "u1"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	row := store.SessionRow{
		UserID:    "u1",
		Provider:  "resy",
		JWT:       "jwt-token",
		ExpiresAt: now.Add(time.Hour),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.UpsertSession(ctx, row); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	got, err := s.GetSession(ctx, "u1", "resy", now)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.JWT != "jwt-token" {
		t.Errorf("jwt drifted: %s", got.JWT)
	}
	if !got.ExpiresAt.Equal(row.ExpiresAt) {
		t.Errorf("exp drifted: %v vs %v", got.ExpiresAt, row.ExpiresAt)
	}

	// Idempotent upsert with new jwt.
	row.JWT = "new-jwt"
	row.UpdatedAt = now.Add(time.Minute)
	if err := s.UpsertSession(ctx, row); err != nil {
		t.Fatalf("UpsertSession again: %v", err)
	}
	got, err = s.GetSession(ctx, "u1", "resy", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.JWT != "new-jwt" {
		t.Errorf("upsert did not refresh jwt: %s", got.JWT)
	}
}

func TestGetSessionExpired(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	if err := storeUser(s, "u_exp"); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	if err := s.UpsertSession(ctx, store.SessionRow{
		UserID:    "u_exp",
		Provider:  "resy",
		JWT:       "old",
		ExpiresAt: now.Add(-time.Hour),
		CreatedAt: now.Add(-2 * time.Hour),
		UpdatedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := s.GetSession(ctx, "u_exp", "resy", now)
	if !errors.Is(err, store.ErrSessionExpired) {
		t.Fatalf("err = %v, want ErrSessionExpired", err)
	}
}

func TestGetSessionMissing(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.GetSession(context.Background(), "ghost", "resy", time.Now().UTC())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDeleteSession(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	if err := storeUser(s, "u_del"); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	if err := s.UpsertSession(ctx, store.SessionRow{
		UserID:    "u_del",
		Provider:  "resy",
		JWT:       "x",
		ExpiresAt: now.Add(time.Hour),
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteSession(ctx, "u_del", "resy"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	_, err := s.GetSession(ctx, "u_del", "resy", now)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
}

func TestVenuesRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("LoadLocation: %v", err)
	}
	v := domain.Venue{Provider: "resy", Ref: "38660", Name: "Carbone", TZ: ny}
	if err := s.UpsertVenue(ctx, v); err != nil {
		t.Fatalf("UpsertVenue: %v", err)
	}

	got, err := s.GetVenue(ctx, v.AsRef())
	if err != nil {
		t.Fatalf("GetVenue: %v", err)
	}
	if got.Name != v.Name || got.TZ.String() != v.TZ.String() {
		t.Errorf("venue drifted: %+v", got)
	}
}

func TestUpsertVenueRejectsMissingTZ(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	err := s.UpsertVenue(context.Background(), domain.Venue{Provider: "resy", Ref: "x"})
	if err == nil {
		t.Fatal("missing TZ must error")
	}
}

func TestObservedRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	o := store.ObservedRelease{
		Venue:             domain.VenueRef{Provider: "resy", Ref: "38660"},
		ObservedLocalTime: domain.NewWallTime(10, 0, 0),
		DaysOffset:        14,
		ObservedAt:        now,
	}
	if err := s.UpsertObserved(ctx, o); err != nil {
		t.Fatalf("UpsertObserved: %v", err)
	}

	got, err := s.GetObserved(ctx, o.Venue)
	if err != nil {
		t.Fatalf("GetObserved: %v", err)
	}
	if got.ObservedLocalTime != o.ObservedLocalTime {
		t.Errorf("local time: %v vs %v", got.ObservedLocalTime, o.ObservedLocalTime)
	}
	if got.DaysOffset != o.DaysOffset {
		t.Errorf("days_offset: %d vs %d", got.DaysOffset, o.DaysOffset)
	}
	if !got.ObservedAt.Equal(o.ObservedAt) {
		t.Errorf("observed_at: %v vs %v", got.ObservedAt, o.ObservedAt)
	}

	// Upsert refreshes.
	o.DaysOffset = 30
	o.ObservedAt = now.Add(time.Hour)
	if err := s.UpsertObserved(ctx, o); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetObserved(ctx, o.Venue)
	if err != nil {
		t.Fatal(err)
	}
	if got.DaysOffset != 30 {
		t.Errorf("upsert did not refresh days_offset: %d", got.DaysOffset)
	}
}

func TestObservedNotFound(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.GetObserved(context.Background(),
		domain.VenueRef{Provider: "resy", Ref: "missing"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// storeUser writes a minimal users row directly via the DB accessor
// so session FK constraints are satisfied. Users management isn't
// surfaced by the Store interface in Phase 1.
func storeUser(s *store.SQLiteStore, id string) error {
	_, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO users (id) VALUES (?)`, id)
	return err
}
