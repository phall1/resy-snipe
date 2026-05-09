package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/engine"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/store"
)

// snipeFakeProvider satisfies providers.Provider AND the SlotPreparer
// shape RunBookingRace asserts on at runtime. It is intentionally
// hard-coded for one slot — the test wants to exercise the wiring
// (engine.Submit → Run → RunBookingRace → notifier surface), not the
// race-and-cancel mechanics already covered in race_test.go.
type snipeFakeProvider struct {
	findCalls    atomic.Int32
	prepareCalls atomic.Int32
	confirmCalls atomic.Int32
	confirmation providers.Confirmation
	failConfirm  bool
}

func (*snipeFakeProvider) ID() domain.ProviderID { return "resy" }

func (*snipeFakeProvider) Login(context.Context, providers.Credentials) (providers.Session, error) {
	return nil, errors.New("snipeFakeProvider: Login unused")
}

func (*snipeFakeProvider) Ping(context.Context, providers.Session) error { return nil }

func (*snipeFakeProvider) SearchVenues(context.Context, providers.Query) ([]domain.Venue, error) {
	return nil, nil
}

func (*snipeFakeProvider) Calendar(context.Context, domain.VenueRef, providers.DateRange) (providers.Calendar, error) {
	return providers.Calendar{}, nil
}

func (*snipeFakeProvider) Book(context.Context, providers.Slot, providers.Session) (providers.Confirmation, error) {
	// Engine never calls Book directly when the provider also implements
	// SlotPreparer; this exists to satisfy providers.Provider.
	return providers.Confirmation{}, errors.New("snipeFakeProvider: Book unused — Prepare/Confirm path expected")
}

func (f *snipeFakeProvider) Find(_ context.Context, req providers.FindRequest) ([]providers.Slot, error) {
	f.findCalls.Add(1)
	return []providers.Slot{{
		Venue:     req.Venue,
		Date:      req.Date,
		Time:      domain.NewWallTime(19, 30, 0),
		PartySize: req.PartySize,
		Payload: domain.ResySlotPayload{
			ConfigID:   "cfg-1",
			TemplateID: "tmpl-1",
		},
	}}, nil
}

// PrepareSlot's error return is always nil in this test fake — the
// happy and failure paths the suite exercises live in ConfirmSlot. We
// keep the (Slot, error) signature so the type satisfies the engine's
// SlotPreparer; a Prepare-side failure path can be added later
// without changing every call site.
func (f *snipeFakeProvider) PrepareSlot(_ context.Context, slot providers.Slot, _ providers.Session) (providers.Slot, error) { //nolint:unparam // signature pinned by the interface
	f.prepareCalls.Add(1)
	p, _ := slot.Payload.(domain.ResySlotPayload)
	p.BookToken = "book-token"
	p.PaymentMethodID = 7
	out := slot
	out.Payload = p
	return out, nil
}

func (f *snipeFakeProvider) ConfirmSlot(_ context.Context, _ providers.Slot, _ providers.Session, _ string) (providers.Confirmation, error) {
	f.confirmCalls.Add(1)
	if f.failConfirm {
		return providers.Confirmation{}, providers.ErrSlotTaken
	}
	return f.confirmation, nil
}

// snipeFakeSession satisfies providers.Session for tests; the resy
// adapter's real Session would carry a JWT, but the engine + the fake
// provider above never inspect it.
type snipeFakeSession struct{}

func (*snipeFakeSession) User() domain.UserID         { return "u@x.io" }
func (*snipeFakeSession) Provider() domain.ProviderID { return "resy" }
func (*snipeFakeSession) ExpiresAt() time.Time {
	return time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)
}

// recordingNotifier captures every Transition / Result call so tests
// can assert the expected notification sequence without parsing
// stdout. It is the equivalent of recordingSubscriber in
// engine/subscribe_test.go but at the Notifier seam — the test
// validates the full bridge.
type recordingNotifier struct {
	mu          sync.Mutex
	transitions []recordedTransition
	results     []recordedResult
}

type recordedTransition struct {
	from, to domain.Status
}

type recordedResult struct {
	success bool
	message string
}

func (r *recordingNotifier) Transition(_ context.Context, _ domain.SnipeID, from, to domain.Status, _ []slog.Attr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transitions = append(r.transitions, recordedTransition{from: from, to: to})
}

func (r *recordingNotifier) Result(_ context.Context, _ domain.SnipeID, success bool, message string, _ []slog.Attr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, recordedResult{success: success, message: message})
}

func (*recordingNotifier) Close() error { return nil }

// snipeStoreFixture builds an on-disk SQLite store the engine can
// write to. Using a temp file (rather than ":memory:") keeps the
// connection pool's reader/writer split realistic — modernc/sqlite
// behaves differently on memory DBs in subtle ways the engine has
// hit before.
func snipeStoreFixture(t *testing.T) store.Store {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "snipe.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return store.NewSQLiteStore(db)
}

func snipeIntent(releaseAt time.Time) domain.Intent {
	return domain.Intent{
		User:      "u@x.io",
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
		SlotPrefs: []domain.SlotPreference{
			{Time: domain.NewWallTime(19, 30, 0)},
		},
		Release: domain.ExplicitRelease{At: releaseAt},
	}
}

// TestRunSnipe_EndToEnd_Booked drives runSnipe through the full happy
// path: Submitted → Scheduled → Awaiting → Finding → Booking → Booked.
// Asserts every transition is delivered to the notifier in order and
// the terminal Result line carries the confirmation id.
func TestRunSnipe_EndToEnd_Booked(t *testing.T) {
	t.Parallel()

	// Real clock: the booking-race goroutines + waitWithCtx use the
	// engine's clock for their inter-call PollFloor. Driving them with
	// a fake would require manually advancing through every internal
	// wait, which would obscure what the test actually proves.
	clk := clock.NewReal()
	s := snipeStoreFixture(t)

	prov := &snipeFakeProvider{
		confirmation: providers.Confirmation{
			ID:       "conf-abc-123",
			BookedAt: clk.Now(),
		},
	}
	notifier := &recordingNotifier{}

	// ExplicitRelease in the past: Run computes a non-positive delay
	// and fires the release synchronously, so the test does not have
	// to advance any clock.
	intent := snipeIntent(clk.Now().Add(-time.Second))

	logger := slog.New(slog.DiscardHandler)
	finalStatus, err := runSnipe(context.Background(), intent, &snipeFakeSession{}, s, prov, notifier, logger, clk)
	if err != nil {
		t.Fatalf("runSnipe: %v", err)
	}
	if finalStatus != domain.StatusBooked {
		t.Fatalf("final status = %s, want booked", finalStatus)
	}

	// Provider was actually exercised — guards against a wiring
	// regression where the engine reaches Awaiting but never calls
	// the booking race.
	if got := prov.findCalls.Load(); got != 1 {
		t.Errorf("Find calls = %d, want 1", got)
	}
	if got := prov.prepareCalls.Load(); got != 1 {
		t.Errorf("PrepareSlot calls = %d, want 1", got)
	}
	if got := prov.confirmCalls.Load(); got != 1 {
		t.Errorf("ConfirmSlot calls = %d, want 1", got)
	}

	// Notifier saw the full transition sequence. The bootstrap
	// Submitted notification (From == nil) is filtered by
	// notifierBridge — the test confirms that.
	want := []recordedTransition{
		{from: domain.StatusSubmitted, to: domain.StatusScheduled},
		{from: domain.StatusScheduled, to: domain.StatusAwaiting},
		{from: domain.StatusAwaiting, to: domain.StatusFinding},
		{from: domain.StatusFinding, to: domain.StatusBooking},
		{from: domain.StatusBooking, to: domain.StatusBooked},
	}
	notifier.mu.Lock()
	gotTransitions := notifier.transitions
	gotResults := notifier.results
	notifier.mu.Unlock()
	if len(gotTransitions) != len(want) {
		t.Fatalf("transition count = %d, want %d: %+v", len(gotTransitions), len(want), gotTransitions)
	}
	for i, w := range want {
		if gotTransitions[i] != w {
			t.Errorf("transition[%d] = %+v, want %+v", i, gotTransitions[i], w)
		}
	}

	if len(gotResults) != 1 {
		t.Fatalf("Result calls = %d, want 1", len(gotResults))
	}
	if !gotResults[0].success {
		t.Errorf("Result.success = false, want true")
	}
	if !strings.Contains(gotResults[0].message, "conf-abc-123") {
		t.Errorf("Result.message = %q, want it to contain confirmation id", gotResults[0].message)
	}
}

// TestRunSnipe_EndToEnd_FailedBookingRace verifies the failure path:
// every ConfirmSlot returns ErrSlotTaken so the race reports
// all_attempts_failed. runSnipe must surface that as a non-Booked
// terminal status and emit a failure Result.
func TestRunSnipe_EndToEnd_FailedBookingRace(t *testing.T) {
	t.Parallel()
	clk := clock.NewReal()
	s := snipeStoreFixture(t)

	prov := &snipeFakeProvider{failConfirm: true}
	notifier := &recordingNotifier{}

	intent := snipeIntent(clk.Now().Add(-time.Second))
	logger := slog.New(slog.DiscardHandler)
	finalStatus, err := runSnipe(context.Background(), intent, &snipeFakeSession{}, s, prov, notifier, logger, clk)
	if err == nil {
		t.Fatal("expected non-nil error from failed booking race")
	}
	if finalStatus == domain.StatusBooked {
		t.Errorf("final status = booked despite all confirms failing")
	}

	notifier.mu.Lock()
	results := notifier.results
	notifier.mu.Unlock()
	if len(results) != 1 {
		t.Fatalf("Result calls = %d, want 1", len(results))
	}
	if results[0].success {
		t.Error("Result.success = true on failed race")
	}
}

// TestRunSnipe_BridgeFiltersBootstrap is the unit-level confirmation
// that notifierBridge drops the From == nil bootstrap emission. The
// end-to-end test above relies on this; this one nails it down so a
// regression that introduces a "<unknown> -> submitted" line is
// caught even if the e2e sequence happens to mask it.
func TestRunSnipe_BridgeFiltersBootstrap(t *testing.T) {
	t.Parallel()
	notifier := &recordingNotifier{}
	bridge := notifierBridge(context.Background(), notifier)

	// Bootstrap (From == nil) — must drop, the "<none> -> submitted"
	// line carries no useful information for the user.
	bridge(engine.Notification{
		SnipeID: "snp_test",
		From:    nil,
		To:      domain.StatusSubmitted,
	})
	if got := len(notifier.transitions); got != 0 {
		t.Errorf("bootstrap notification produced %d transitions, want 0", got)
	}

	// Real transition — must forward.
	from := domain.StatusSubmitted
	bridge(engine.Notification{
		SnipeID: "snp_test",
		From:    &from,
		To:      domain.StatusScheduled,
	})
	if got := len(notifier.transitions); got != 1 {
		t.Fatalf("post-bootstrap notification produced %d transitions, want 1", got)
	}
}
