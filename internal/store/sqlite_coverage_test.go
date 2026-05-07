package store_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/store"
)

// TestSnipeStateRoundTripsForEveryStatus drives a SnipeState through legal
// transitions to each of the 10 statuses, persists via CreateSnipe +
// UpdateSnipe, reads back with GetSnipe, and asserts the loaded status
// matches. This pins the contract that every persisted Status round-trips
// through the load path's Transition-walking forceStatus helper.
func TestSnipeStateRoundTripsForEveryStatus(t *testing.T) {
	t.Parallel()

	for _, target := range domain.AllStatuses() {
		t.Run(target.String(), func(t *testing.T) {
			t.Parallel()
			// Each sub-test gets its own SQLite file so parallel runs
			// don't contend on the same DB lock.
			s := newStore(t)
			ctx := context.Background()
			now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
			id := domain.SnipeID(fmt.Sprintf("snp_status_%s", target))
			st := domain.NewSnipeState(id, sampleIntent(t), now)
			if err := walkTo(st, target); err != nil {
				t.Fatalf("walk to %s: %v", target, err)
			}
			st.UpdatedAt = now
			if err := s.CreateSnipe(ctx, st); err != nil {
				t.Fatalf("CreateSnipe: %v", err)
			}
			// CreateSnipe was inserted with the post-walk status, so
			// GetSnipe must round-trip it. Update once to force the
			// UPDATE path is also exercised at every status.
			st.UpdatedAt = now.Add(time.Minute)
			if err := s.UpdateSnipe(ctx, st); err != nil {
				t.Fatalf("UpdateSnipe: %v", err)
			}
			got, err := s.GetSnipe(ctx, id)
			if err != nil {
				t.Fatalf("GetSnipe: %v", err)
			}
			if got.Status() != target {
				t.Errorf("status round-trip: got %s, want %s", got.Status(), target)
			}
		})
	}
}

// walkTo drives s through legitimate transitions until its status equals
// target. It mirrors the path table the store's forceStatus uses — we
// re-derive it here to avoid coupling to an unexported function.
func walkTo(s *domain.SnipeState, target domain.Status) error {
	if s.Status() == target {
		return nil
	}
	paths := pathTo(target)
	for _, path := range paths {
		// Find a starting index at which the path matches s's current status.
		start := -1
		for i, st := range path {
			if st == s.Status() {
				start = i
				break
			}
		}
		if start < 0 {
			continue
		}
		ok := true
		for i := start + 1; i < len(path); i++ {
			if err := s.Transition(path[i]); err != nil {
				ok = false
				break
			}
		}
		if ok && s.Status() == target {
			return nil
		}
	}
	return fmt.Errorf("no path from %s to %s", s.Status(), target)
}

// pathTo returns one or more canonical paths from Submitted to target.
// Paths begin with StatusSubmitted (the initial status of every
// SnipeState) so walkTo can match the start position.
func pathTo(target domain.Status) [][]domain.Status {
	switch target {
	case domain.StatusSubmitted:
		return [][]domain.Status{{domain.StatusSubmitted}}
	case domain.StatusScheduled:
		return [][]domain.Status{{domain.StatusSubmitted, domain.StatusScheduled}}
	case domain.StatusDiscovering:
		return [][]domain.Status{{domain.StatusSubmitted, domain.StatusDiscovering}}
	case domain.StatusAwaiting:
		return [][]domain.Status{
			{domain.StatusSubmitted, domain.StatusScheduled, domain.StatusAwaiting},
		}
	case domain.StatusFinding:
		return [][]domain.Status{
			{domain.StatusSubmitted, domain.StatusScheduled, domain.StatusAwaiting, domain.StatusFinding},
		}
	case domain.StatusBooking:
		return [][]domain.Status{
			{domain.StatusSubmitted, domain.StatusScheduled, domain.StatusAwaiting, domain.StatusFinding, domain.StatusBooking},
		}
	case domain.StatusBooked:
		return [][]domain.Status{
			{domain.StatusSubmitted, domain.StatusScheduled, domain.StatusAwaiting, domain.StatusFinding, domain.StatusBooking, domain.StatusBooked},
		}
	case domain.StatusFailed:
		return [][]domain.Status{{domain.StatusSubmitted, domain.StatusFailed}}
	case domain.StatusCanceled:
		return [][]domain.Status{{domain.StatusSubmitted, domain.StatusCanceled}}
	case domain.StatusExpired:
		return [][]domain.Status{
			{domain.StatusSubmitted, domain.StatusScheduled, domain.StatusExpired},
		}
	}
	return nil
}

// TestEventAppendOrderingPreservedAcrossManyEvents pins the contract that
// ListEventsBySnipe orders by event id (insert order), not by the event's
// At timestamp. We append in a deliberately non-monotonic At order and
// verify the read order is the append order.
func TestEventAppendOrderingPreservedAcrossManyEvents(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	state := domain.NewSnipeState("snp_order", sampleIntent(t), now)
	if err := s.CreateSnipe(ctx, state); err != nil {
		t.Fatalf("CreateSnipe: %v", err)
	}

	// Build 24 events whose At timestamps zig-zag around the base time.
	// If the store sorted by At we'd see a different read order than the
	// append order; we expect them to remain in append order.
	const n = 24
	offsets := []int{
		0, 5, -3, 12, -7, 1, 20, -1, 8, -4,
		15, -10, 2, -6, 11, -2, 9, 17, -8, 3,
		7, -5, 19, -9,
	}
	events := make([]domain.Event, 0, n)
	for i, off := range offsets {
		ev := domain.Event{
			Type:  domain.EventSubmitted,
			At:    now.Add(time.Duration(off) * time.Minute),
			Attrs: []slog.Attr{slog.Int("seq", i)},
		}
		events = append(events, ev)
		if err := s.AppendEvent(ctx, "snp_order", ev); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}

	got, err := s.ListEventsBySnipe(ctx, "snp_order")
	if err != nil {
		t.Fatalf("ListEventsBySnipe: %v", err)
	}
	if len(got) != n {
		t.Fatalf("event count = %d, want %d", len(got), n)
	}
	// Verify the At sequence we read matches the append-order At sequence
	// — and explicitly is NOT monotonically increasing (so we know the
	// test is meaningfully exercising non-time-ordered insertion).
	monotone := true
	for i := range n {
		if !got[i].At.Equal(events[i].At) {
			t.Errorf("event %d At: got %v want %v", i, got[i].At, events[i].At)
		}
		if i > 0 && got[i].At.Before(got[i-1].At) {
			monotone = false
		}
	}
	if monotone {
		t.Fatal("appended Ats came back monotonically increasing — test setup did not exercise the ordering contract")
	}
	// Per-event seq attribute must also match append index.
	for i, e := range got {
		var seq int64 = -1
		for _, a := range e.Attrs {
			if a.Key == "seq" {
				seq = a.Value.Int64()
			}
		}
		if seq != int64(i) {
			t.Errorf("event %d seq attr: got %d, want %d", i, seq, i)
		}
	}
}

// TestEventAttrsRoundTripAcrossKinds appends one event whose Attrs cover
// every slog kind the codec preserves: String, Int64, Uint64, Float64,
// Bool, Duration, Time. Every attr must round-trip with its kind and
// value intact.
func TestEventAttrsRoundTripAcrossKinds(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	state := domain.NewSnipeState("snp_attrs", sampleIntent(t), now)
	if err := s.CreateSnipe(ctx, state); err != nil {
		t.Fatalf("CreateSnipe: %v", err)
	}

	whenT := time.Date(2026, 5, 1, 12, 30, 45, 0, time.UTC)
	dur := 750 * time.Millisecond
	ev := domain.Event{
		Type: domain.EventBookAttempted,
		At:   now,
		Attrs: []slog.Attr{
			slog.String("k_string", "hello"),
			slog.Int64("k_int", -42),
			slog.Uint64("k_uint", 18446744073709551610),
			slog.Float64("k_float", 3.14159),
			slog.Bool("k_bool", true),
			slog.Duration("k_dur", dur),
			slog.Time("k_time", whenT),
		},
	}
	if err := s.AppendEvent(ctx, "snp_attrs", ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	got, err := s.ListEventsBySnipe(ctx, "snp_attrs")
	if err != nil {
		t.Fatalf("ListEventsBySnipe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("event count = %d, want 1", len(got))
	}
	byKey := map[string]slog.Attr{}
	for _, a := range got[0].Attrs {
		byKey[a.Key] = a
	}

	checkKind := func(key string, want slog.Kind) slog.Attr {
		t.Helper()
		a, ok := byKey[key]
		if !ok {
			t.Fatalf("attr %q missing", key)
		}
		if a.Value.Kind() != want {
			t.Errorf("attr %q: got kind %v want %v", key, a.Value.Kind(), want)
		}
		return a
	}

	if a := checkKind("k_string", slog.KindString); a.Value.String() != "hello" {
		t.Errorf("k_string: %v", a.Value)
	}
	if a := checkKind("k_int", slog.KindInt64); a.Value.Int64() != -42 {
		t.Errorf("k_int: %v", a.Value)
	}
	if a := checkKind("k_uint", slog.KindUint64); a.Value.Uint64() != 18446744073709551610 {
		t.Errorf("k_uint: %v", a.Value)
	}
	if a := checkKind("k_float", slog.KindFloat64); a.Value.Float64() != 3.14159 {
		t.Errorf("k_float: %v", a.Value)
	}
	if a := checkKind("k_bool", slog.KindBool); !a.Value.Bool() {
		t.Errorf("k_bool: %v", a.Value)
	}
	if a := checkKind("k_dur", slog.KindDuration); a.Value.Duration() != dur {
		t.Errorf("k_dur: %v", a.Value)
	}
	if a := checkKind("k_time", slog.KindTime); !a.Value.Time().Equal(whenT) {
		t.Errorf("k_time: got %v want %v", a.Value.Time(), whenT)
	}
}

// TestSessionUpsertReplacesAndExpReExpires walks through the full
// upsert/expire/re-upsert cycle for one (user, provider) row. We check:
//  1. immediately after upsert with future exp, GetSession at now-before
//     -exp returns the row.
//  2. when 'now' moves past exp, GetSession returns ErrSessionExpired.
//  3. a fresh upsert with a later exp re-validates the row.
func TestSessionUpsertReplacesAndExpReExpires(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	if err := storeUser(s, "u_cycle"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	row := store.SessionRow{
		UserID:    "u_cycle",
		Provider:  "resy",
		JWT:       "jwt_v1",
		ExpiresAt: now.Add(time.Hour),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.UpsertSession(ctx, row); err != nil {
		t.Fatalf("upsert v1: %v", err)
	}

	got, err := s.GetSession(ctx, "u_cycle", "resy", now)
	if err != nil {
		t.Fatalf("get pre-exp: %v", err)
	}
	if got.JWT != "jwt_v1" {
		t.Errorf("jwt v1: %s", got.JWT)
	}

	// Advance now past exp.
	if _, err := s.GetSession(ctx, "u_cycle", "resy", now.Add(2*time.Hour)); !errors.Is(err, store.ErrSessionExpired) {
		t.Fatalf("post-exp: err = %v, want ErrSessionExpired", err)
	}

	// Re-upsert with a fresh exp; a get at now-before-new-exp must succeed.
	row.JWT = "jwt_v2"
	row.ExpiresAt = now.Add(4 * time.Hour)
	row.UpdatedAt = now.Add(time.Minute)
	if err := s.UpsertSession(ctx, row); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	got, err = s.GetSession(ctx, "u_cycle", "resy", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("get post-refresh: %v", err)
	}
	if got.JWT != "jwt_v2" {
		t.Errorf("jwt v2: %s", got.JWT)
	}
	if !got.ExpiresAt.Equal(row.ExpiresAt) {
		t.Errorf("exp v2: got %v want %v", got.ExpiresAt, row.ExpiresAt)
	}
}

// TestVenueUpsertRefreshesLastSeenAt verifies that a second UpsertVenue
// call updates the last_seen_at column (driven by the SQL DEFAULT
// strftime('now')). The Store interface does not expose last_seen_at, so
// we read it directly via the DB() accessor.
//
// strftime('now') has millisecond resolution; we sleep ~2ms between calls
// to guarantee the SQL clock advances. This is the only test in the
// package where time.Sleep is acceptable — we are exercising a SQL
// DEFAULT, not engine timing.
func TestVenueUpsertRefreshesLastSeenAt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("LoadLocation: %v", err)
	}
	v := domain.Venue{Provider: "resy", Ref: "v_lastseen", Name: "First", TZ: ny}
	if err := s.UpsertVenue(ctx, v); err != nil {
		t.Fatalf("UpsertVenue 1: %v", err)
	}
	first := readLastSeenAt(t, s, v.AsRef())

	// Sleep enough for strftime('%f') to tick. Modernc clock granularity
	// is OS-level; 2ms is safe on darwin/linux.
	time.Sleep(2 * time.Millisecond)

	v.Name = "Second"
	if err := s.UpsertVenue(ctx, v); err != nil {
		t.Fatalf("UpsertVenue 2: %v", err)
	}
	second := readLastSeenAt(t, s, v.AsRef())

	if !second.After(first) {
		t.Errorf("last_seen_at not refreshed: first=%s second=%s", first, second)
	}
	// Sanity: the updated name landed too.
	got, err := s.GetVenue(ctx, v.AsRef())
	if err != nil {
		t.Fatalf("GetVenue: %v", err)
	}
	if got.Name != "Second" {
		t.Errorf("name: got %q want Second", got.Name)
	}
}

func readLastSeenAt(t *testing.T, s *store.SQLiteStore, ref domain.VenueRef) time.Time {
	t.Helper()
	var raw string
	err := s.DB().QueryRowContext(context.Background(),
		`SELECT last_seen_at FROM venues WHERE provider = ? AND ref = ?`,
		string(ref.Provider), ref.Ref).Scan(&raw)
	if err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	// last_seen_at format is the schema's strftime('%Y-%m-%dT%H:%M:%fZ', 'now').
	// Try RFC3339Nano first, then the strftime layout (millis only).
	if parsed, perr := time.Parse(time.RFC3339Nano, raw); perr == nil {
		return parsed
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", raw)
	if err != nil {
		t.Fatalf("parse last_seen_at %q: %v", raw, err)
	}
	return parsed
}

// TestObservedReleaseUpsertChangesAllFields covers the full upsert
// replacement: ObservedLocalTime, DaysOffset, and ObservedAt must all
// reflect the second upsert's values.
func TestObservedReleaseUpsertChangesAllFields(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	ref := domain.VenueRef{Provider: "resy", Ref: "v_observed_all"}
	v1 := store.ObservedRelease{
		Venue:             ref,
		ObservedLocalTime: domain.NewWallTime(9, 0, 0),
		DaysOffset:        14,
		ObservedAt:        time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
	}
	if err := s.UpsertObserved(ctx, v1); err != nil {
		t.Fatalf("UpsertObserved 1: %v", err)
	}

	v2 := store.ObservedRelease{
		Venue:             ref,
		ObservedLocalTime: domain.NewWallTime(11, 30, 15),
		DaysOffset:        30,
		ObservedAt:        time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC),
	}
	if err := s.UpsertObserved(ctx, v2); err != nil {
		t.Fatalf("UpsertObserved 2: %v", err)
	}

	got, err := s.GetObserved(ctx, ref)
	if err != nil {
		t.Fatalf("GetObserved: %v", err)
	}
	if got.ObservedLocalTime != v2.ObservedLocalTime {
		t.Errorf("ObservedLocalTime: got %v want %v", got.ObservedLocalTime, v2.ObservedLocalTime)
	}
	if got.DaysOffset != v2.DaysOffset {
		t.Errorf("DaysOffset: got %d want %d", got.DaysOffset, v2.DaysOffset)
	}
	if !got.ObservedAt.Equal(v2.ObservedAt) {
		t.Errorf("ObservedAt: got %v want %v", got.ObservedAt, v2.ObservedAt)
	}
}

// TestTransitionSnipeAtomicityOnInvalidId calls TransitionSnipe with a
// SnipeState whose id has not been inserted. It must return ErrNotFound
// AND leave the events table empty — the row update is the gate that
// fails first, so the event insert must never run.
func TestTransitionSnipeAtomicityOnInvalidId(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	st := domain.NewSnipeState("snp_ghost", sampleIntent(t), now)
	if err := st.Transition(domain.StatusScheduled); err != nil {
		t.Fatalf("transition: %v", err)
	}
	st.UpdatedAt = now.Add(time.Minute)

	ev := domain.Event{
		Type:  domain.EventScheduled,
		At:    now.Add(time.Minute),
		Attrs: []slog.Attr{slog.String("note", "must not land")},
	}
	err := s.TransitionSnipe(ctx, st, ev)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	// No events row may have been written for this snipe id.
	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE snipe_id = ?`, "snp_ghost").Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 0 {
		t.Errorf("event count for ghost snipe = %d, want 0 (transaction rolled back)", n)
	}
}

// TestTransitionSnipeHappyPath gets the TransitionSnipe success path
// covered too, since the existing suite did not exercise it. Atomic
// update + event append in one call.
func TestTransitionSnipeHappyPath(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	st := domain.NewSnipeState("snp_tx", sampleIntent(t), now)
	if err := s.CreateSnipe(ctx, st); err != nil {
		t.Fatalf("CreateSnipe: %v", err)
	}
	if err := st.Transition(domain.StatusScheduled); err != nil {
		t.Fatalf("transition: %v", err)
	}
	st.UpdatedAt = now.Add(time.Minute)

	ev := domain.Event{
		Type:  domain.EventScheduled,
		At:    now.Add(time.Minute),
		Attrs: []slog.Attr{slog.String("reason", "release_at_set")},
	}
	if err := s.TransitionSnipe(ctx, st, ev); err != nil {
		t.Fatalf("TransitionSnipe: %v", err)
	}

	got, err := s.GetSnipe(ctx, "snp_tx")
	if err != nil {
		t.Fatalf("GetSnipe: %v", err)
	}
	if got.Status() != domain.StatusScheduled {
		t.Errorf("status: %s", got.Status())
	}
	events, err := s.ListEventsBySnipe(ctx, "snp_tx")
	if err != nil {
		t.Fatalf("ListEventsBySnipe: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count: %d, want 1", len(events))
	}
	if events[0].Type != domain.EventScheduled {
		t.Errorf("event type: %s", events[0].Type)
	}
}

// TestListEventsBySnipeEmptyForUnknownSnipe asserts that asking for
// events of a snipe that has none (or doesn't exist) returns an empty
// slice with nil error — no sentinel for "no rows" on lists.
func TestListEventsBySnipeEmptyForUnknownSnipe(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	got, err := s.ListEventsBySnipe(ctx, "nope")
	if err != nil {
		t.Fatalf("ListEventsBySnipe: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d events, want 0", len(got))
	}
}

// TestListSnipesByStatusEmpty ensures the empty-result path on
// ListSnipesByStatus returns an empty slice and no error.
func TestListSnipesByStatusEmpty(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	got, err := s.ListSnipesByStatus(context.Background(), domain.StatusBooked)
	if err != nil {
		t.Fatalf("ListSnipesByStatus: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d snipes, want 0", len(got))
	}
}

// TestIntentReleaseVariantsRoundTripThroughSnipe rounds an intent through
// CreateSnipe + GetSnipe with each ReleaseStrategy variant (Explicit,
// Discovered, Continuous, and the nil sentinel) so the encode/decode
// switch cases all run.
func TestIntentReleaseVariantsRoundTripThroughSnipe(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		release domain.ReleaseStrategy
	}{
		{"explicit", domain.ExplicitRelease{At: base}},
		{"discovered", domain.DiscoveredRelease{ProbeFrom: base, ProbeUntil: base.Add(time.Hour)}},
		{"continuous", domain.ContinuousRelease{Until: base.Add(2 * time.Hour)}},
		{"nil", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			ctx := context.Background()
			intent := sampleIntent(t)
			intent.Release = tc.release
			id := domain.SnipeID("snp_rel_" + tc.name)
			st := domain.NewSnipeState(id, intent, base)
			if err := s.CreateSnipe(ctx, st); err != nil {
				t.Fatalf("CreateSnipe: %v", err)
			}
			got, err := s.GetSnipe(ctx, id)
			if err != nil {
				t.Fatalf("GetSnipe: %v", err)
			}
			if got.Intent.Release == nil && tc.release != nil {
				t.Fatalf("release dropped on round-trip: want %T", tc.release)
			}
			if got.Intent.Release != nil && tc.release == nil {
				t.Fatalf("release synthesized on round-trip: got %T", got.Intent.Release)
			}
			if got.Intent.Hash() != intent.Hash() {
				t.Errorf("intent hash drifted on %s release", tc.name)
			}
		})
	}
}

// TestResultRoundTripsThroughSnipe walks a snipe to Booked and persists a
// Result; reading back must yield the same Result fields. This is what
// covers UnmarshalResult, which the existing suite does not exercise.
func TestResultRoundTripsThroughSnipe(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	st := domain.NewSnipeState("snp_result", sampleIntent(t), now)
	if err := walkTo(st, domain.StatusBooked); err != nil {
		t.Fatalf("walk to booked: %v", err)
	}
	st.Result = &domain.Result{
		BookedAt:       now.Add(time.Hour).UTC(),
		ConfirmationID: "RES-12345",
	}
	st.UpdatedAt = now.Add(time.Hour)
	if err := s.CreateSnipe(ctx, st); err != nil {
		t.Fatalf("CreateSnipe: %v", err)
	}

	got, err := s.GetSnipe(ctx, "snp_result")
	if err != nil {
		t.Fatalf("GetSnipe: %v", err)
	}
	if got.Result == nil {
		t.Fatal("result dropped on round-trip")
	}
	if got.Result.ConfirmationID != st.Result.ConfirmationID {
		t.Errorf("confirmation: got %q want %q", got.Result.ConfirmationID, st.Result.ConfirmationID)
	}
	if !got.Result.BookedAt.Equal(st.Result.BookedAt) {
		t.Errorf("booked_at: got %v want %v", got.Result.BookedAt, st.Result.BookedAt)
	}
}

// TestPayloadCodecRoundTripPreservesEveryField builds a fully populated
// ResySlotPayload, marshals through the store envelope, unmarshals, and
// asserts every field round-trips. (The existing payload_codec_test.go
// covers ResySlotPayload but not all fields populated.)
func TestPayloadCodecRoundTripPreservesEveryField(t *testing.T) {
	t.Parallel()
	original := domain.ResySlotPayload{
		ConfigID:    "rgs://resy/cal/v2/abc/2/2026-06-01/2026-06-01/19:30:00/2/Bar",
		TemplateID:  "tmpl_xyz",
		SeatingType: "Bar",
	}
	data, err := store.MarshalSlotPayload(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := store.UnmarshalSlotPayload(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	resy, ok := got.(domain.ResySlotPayload)
	if !ok {
		t.Fatalf("type lost on round-trip: %T", got)
	}
	if resy.ConfigID != original.ConfigID {
		t.Errorf("ConfigID: got %q want %q", resy.ConfigID, original.ConfigID)
	}
	if resy.TemplateID != original.TemplateID {
		t.Errorf("TemplateID: got %q want %q", resy.TemplateID, original.TemplateID)
	}
	if resy.SeatingType != original.SeatingType {
		t.Errorf("SeatingType: got %q want %q", resy.SeatingType, original.SeatingType)
	}
}

// TestEncodeAttrsEmptyReturnsNil and TestDecodeAttrsEmptyReturnsNil pin
// the codec's empty-input contract. Empty input means "no attrs", not an
// error — the engine relies on this for no-attrs events.
func TestEncodeAttrsEmptyReturnsNil(t *testing.T) {
	t.Parallel()
	b, err := store.EncodeAttrs(nil)
	if err != nil {
		t.Fatalf("EncodeAttrs(nil): %v", err)
	}
	if b != nil {
		t.Errorf("EncodeAttrs(nil): got %q, want nil", b)
	}
}

func TestDecodeAttrsEmptyReturnsNil(t *testing.T) {
	t.Parallel()
	got, err := store.DecodeAttrs(nil)
	if err != nil {
		t.Fatalf("DecodeAttrs(nil): %v", err)
	}
	if got != nil {
		t.Errorf("DecodeAttrs(nil): got %v, want nil", got)
	}
}

// TestDecodeAttrsRejectsGarbage covers the JSON-unmarshal error branch.
func TestDecodeAttrsRejectsGarbage(t *testing.T) {
	t.Parallel()
	_, err := store.DecodeAttrs([]byte("not json"))
	if err == nil {
		t.Fatal("garbage bytes must error")
	}
}

// TestUnmarshalIntentRejectsGarbage covers the codec's error path on
// malformed wire bytes — the engine relies on this surfacing as an
// error rather than a partial Intent.
func TestUnmarshalIntentRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := store.UnmarshalIntent([]byte("not json")); err == nil {
		t.Fatal("garbage bytes must error")
	}
}

// TestUnmarshalIntentRejectsBadDate covers the date-parse error path.
func TestUnmarshalIntentRejectsBadDate(t *testing.T) {
	t.Parallel()
	bad := `{"user":"u","venue":{"Provider":"resy","Ref":"v"},"date":"not-a-date","party_size":2}`
	if _, err := store.UnmarshalIntent([]byte(bad)); err == nil {
		t.Fatal("bad date must error")
	}
}

// TestUnmarshalResultEmptyReturnsNil pins the no-result-yet contract.
func TestUnmarshalResultEmptyReturnsNil(t *testing.T) {
	t.Parallel()
	got, err := store.UnmarshalResult(nil)
	if err != nil {
		t.Fatalf("UnmarshalResult(nil): %v", err)
	}
	if got != nil {
		t.Errorf("UnmarshalResult(nil): got %+v, want nil", got)
	}
}

// TestUnmarshalResultRejectsGarbage covers the JSON-unmarshal error path.
func TestUnmarshalResultRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := store.UnmarshalResult([]byte("not json")); err == nil {
		t.Fatal("garbage bytes must error")
	}
}

// TestGetVenueRejectsBadTZ covers the time.LoadLocation error branch in
// GetVenue. We insert a row with an invalid IANA zone name via raw SQL,
// then expect GetVenue to surface the LoadLocation failure.
func TestGetVenueRejectsBadTZ(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	// Bypass UpsertVenue (which validates) and inject a corrupt row.
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO venues (provider, ref, name, tz) VALUES (?, ?, ?, ?)`,
		"resy", "v_badtz", "Bad", "Not/AReal/Zone")
	if err != nil {
		t.Fatalf("seed venue: %v", err)
	}

	_, err = s.GetVenue(ctx, domain.VenueRef{Provider: "resy", Ref: "v_badtz"})
	if err == nil {
		t.Fatal("GetVenue should error on bad tz")
	}
}

// TestGetSnipeRejectsCorruptIntentJSON covers scanSnipeRow's intent
// decode-error branch. We insert a snipes row directly with bad JSON,
// then expect GetSnipe to surface the decode failure.
func TestGetSnipeRejectsCorruptIntentJSON(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO snipes (id, intent_hash, intent_json, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"snp_bad_intent", "ihash", "not-json", "submitted",
		"2026-05-01T09:00:00Z", "2026-05-01T09:00:00Z")
	if err != nil {
		t.Fatalf("seed snipe: %v", err)
	}

	_, err = s.GetSnipe(ctx, "snp_bad_intent")
	if err == nil {
		t.Fatal("GetSnipe should error on corrupt intent_json")
	}
}

// TestGetSnipeRejectsUnknownStatus covers the parseStatus error branch
// hit when scanning a row whose status column is not one of the known
// strings. The store should not silently coerce — it must error.
func TestGetSnipeRejectsUnknownStatus(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	intentJSON := `{"user":"u","venue":{"Provider":"resy","Ref":"v"},"date":"2026-06-01","party_size":2}`
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO snipes (id, intent_hash, intent_json, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"snp_bad_status", "ihash", intentJSON, "in-some-future-state",
		"2026-05-01T09:00:00Z", "2026-05-01T09:00:00Z")
	if err != nil {
		t.Fatalf("seed snipe: %v", err)
	}

	_, err = s.GetSnipe(ctx, "snp_bad_status")
	if err == nil {
		t.Fatal("GetSnipe should error on unknown status")
	}
}

// TestListSnipesByStatusRejectsCorruptRow exercises the scan-error path
// inside ListSnipesByStatus's iteration loop.
func TestListSnipesByStatusRejectsCorruptRow(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO snipes (id, intent_hash, intent_json, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"snp_bad_list", "ihash", "not-json", "submitted",
		"2026-05-01T09:00:00Z", "2026-05-01T09:00:00Z")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := s.ListSnipesByStatus(ctx, domain.StatusSubmitted); err == nil {
		t.Fatal("ListSnipesByStatus should error on corrupt row")
	}
}

// TestGetSnipeRejectsBadCreatedAt covers the scanSnipeRow created_at
// parse-error branch.
func TestGetSnipeRejectsBadCreatedAt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	intentJSON := `{"user":"u","venue":{"Provider":"resy","Ref":"v"},"date":"2026-06-01","party_size":2}`
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO snipes (id, intent_hash, intent_json, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"snp_bad_created", "ihash", intentJSON, "submitted",
		"NOT-A-TIMESTAMP", "2026-05-01T09:00:00Z")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.GetSnipe(ctx, "snp_bad_created"); err == nil {
		t.Fatal("GetSnipe should error on bad created_at")
	}
}

// TestGetSnipeRejectsBadScheduledAt covers the scanSnipeRow
// scheduled_at parse-error branch (taken only when scheduled_at is
// non-NULL but malformed).
func TestGetSnipeRejectsBadScheduledAt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	intentJSON := `{"user":"u","venue":{"Provider":"resy","Ref":"v"},"date":"2026-06-01","party_size":2}`
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO snipes (id, intent_hash, intent_json, status, scheduled_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"snp_bad_sched", "ihash", intentJSON, "submitted",
		"NOT-A-TIMESTAMP",
		"2026-05-01T09:00:00Z", "2026-05-01T09:00:00Z")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.GetSnipe(ctx, "snp_bad_sched"); err == nil {
		t.Fatal("GetSnipe should error on bad scheduled_at")
	}
}

// TestGetSnipeRejectsBadResultJSON covers the scanSnipeRow result-decode
// error branch.
func TestGetSnipeRejectsBadResultJSON(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	intentJSON := `{"user":"u","venue":{"Provider":"resy","Ref":"v"},"date":"2026-06-01","party_size":2}`
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO snipes (id, intent_hash, intent_json, status, result_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"snp_bad_result", "ihash", intentJSON, "submitted",
		"not-json",
		"2026-05-01T09:00:00Z", "2026-05-01T09:00:00Z")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.GetSnipe(ctx, "snp_bad_result"); err == nil {
		t.Fatal("GetSnipe should error on bad result_json")
	}
}

// TestUnmarshalIntentRejectsBadSlotTime covers the slot-time parse-error
// branch in UnmarshalIntent.
func TestUnmarshalIntentRejectsBadSlotTime(t *testing.T) {
	t.Parallel()
	bad := `{
		"user":"u","venue":{"Provider":"resy","Ref":"v"},
		"date":"2026-06-01","party_size":2,
		"slot_prefs":[{"time":"not-a-time"}]
	}`
	if _, err := store.UnmarshalIntent([]byte(bad)); err == nil {
		t.Fatal("bad slot time must error")
	}
}

// TestUnmarshalIntentRejectsUnknownReleaseKind covers decodeRelease's
// unknown-discriminator branch.
func TestUnmarshalIntentRejectsUnknownReleaseKind(t *testing.T) {
	t.Parallel()
	bad := `{
		"user":"u","venue":{"Provider":"resy","Ref":"v"},
		"date":"2026-06-01","party_size":2,
		"release":{"kind":"telepathic"}
	}`
	if _, err := store.UnmarshalIntent([]byte(bad)); err == nil {
		t.Fatal("unknown release kind must error")
	}
}

// TestGetObservedRejectsBadObservedAt covers GetObserved's observed_at
// parse-error branch.
func TestGetObservedRejectsBadObservedAt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO observed_release_times (provider, venue_ref, observed_local_time, days_offset, observed_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"resy", "v_bad_at", "10:00:00", 14, "NOT-A-TIMESTAMP")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.GetObserved(ctx, domain.VenueRef{Provider: "resy", Ref: "v_bad_at"}); err == nil {
		t.Fatal("GetObserved should error on bad observed_at")
	}
}

// TestGetObservedRejectsBadLocalTime covers GetObserved's wall-time
// parse-error branch.
func TestGetObservedRejectsBadLocalTime(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO observed_release_times (provider, venue_ref, observed_local_time, days_offset, observed_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"resy", "v_bad_wt", "twenty-six o'clock", 14, "2026-05-01T09:00:00Z")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.GetObserved(ctx, domain.VenueRef{Provider: "resy", Ref: "v_bad_wt"}); err == nil {
		t.Fatal("GetObserved should error on bad local time")
	}
}

// TestGetSessionRejectsBadExp covers GetSession's exp parse-error
// branch.
func TestGetSessionRejectsBadExp(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	if err := storeUser(s, "u_badexp"); err != nil {
		t.Fatal(err)
	}
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO sessions (user_id, provider, jwt, exp, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"u_badexp", "resy", "jwt", "NOT-A-TIMESTAMP",
		"2026-05-01T09:00:00Z", "2026-05-01T09:00:00Z")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.GetSession(ctx, "u_badexp", "resy", time.Now()); err == nil {
		t.Fatal("GetSession should error on bad exp")
	}
}
