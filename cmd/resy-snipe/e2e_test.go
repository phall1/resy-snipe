package main

// e2e_test.go: end-to-end integration test for milestone M1, A8.
//
// This test wires the full v2 Service pipeline against an httptest
// fake of the Resy backend: Goal → Service.PlanQuest → Service.CreateQuest
// → engine drives the booking pipeline → terminal Booked. Real
// components: SQLite (file-backed under t.TempDir), real engine, real
// resy.Client (against httptest.NewServer), real resolver, real
// planner, real service.Standard. Only the network and the clock are
// stubbed — exactly the seams an integration test is meant to control.
//
// The Service layer in M1 mints the quest row and submits to the engine,
// but it does not yet host a background runner that drives engine.Run
// for newly-submitted quests; that lands with the M2 daemon. The test
// stands in for the daemon by driving engine.RunBookingRace itself
// once the Service has submitted the intent. The Service is still the
// entry point being exercised; the test only fills the M1-shaped runner
// gap so the full Goal → Booked path is observable end-to-end.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	"resy-snipe/internal/resolver"
	"resy-snipe/internal/resy"
	"resy-snipe/internal/service"
	"resy-snipe/internal/store"
)

// ---- fake Resy backend ----------------------------------------------------

// fakeResyServer wraps httptest.Server with per-endpoint counters and
// swappable behaviors. The zero value is not useful; use newFakeResyServer.
type fakeResyServer struct {
	srv *httptest.Server

	// Per-endpoint hit counters, useful for asserting the engine
	// walked the documented pipeline.
	loginHits    atomic.Int32
	venueHits    atomic.Int32
	findHits     atomic.Int32
	detailsHits  atomic.Int32
	bookHits     atomic.Int32
	calendarHits atomic.Int32

	mu sync.Mutex
	// expireTokenOnce makes the next /3/book respond with
	// BOOK_TOKEN_EXPIRED, then resets itself.
	expireTokenOnce bool
	// antiBotOnFind makes /4/find respond with a PerimeterX-shaped
	// 403 challenge until cleared.
	antiBotOnFind bool

	jwt       string // canned /3/auth/password token
	venueBody []byte // canned /3/venue fixture body
}

// newFakeResyServer returns a running httptest server that responds
// like Resy for the endpoints the Service + engine traverse on a
// happy-path snipe. Caller invokes Close (or relies on t.Cleanup
// inside setupE2EBackend).
func newFakeResyServer(t *testing.T) *fakeResyServer {
	t.Helper()

	// Reuse the canonical /3/venue fixture so the resolver decodes a
	// real Resy shape.
	venueBody, err := os.ReadFile(filepath.Join("..", "..", "internal", "resy", "testdata", "venue_astoria.json"))
	if err != nil {
		t.Fatalf("read venue fixture: %v", err)
	}

	// JWT validity well past the test's clock window.
	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	jwt := makeFakeJWT(t, exp)

	f := &fakeResyServer{jwt: jwt, venueBody: venueBody}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	return f
}

// URL returns the server's base URL.
func (f *fakeResyServer) URL() string { return f.srv.URL }

// HTTPClient returns the test client paired with the server.
func (f *fakeResyServer) HTTPClient() *http.Client { return f.srv.Client() }

// Close shuts the server down.
func (f *fakeResyServer) Close() { f.srv.Close() }

// expireBookTokenOnce primes the next /3/book to fail with
// BOOK_TOKEN_EXPIRED. Subsequent calls succeed.
func (f *fakeResyServer) expireBookTokenOnce() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expireTokenOnce = true
}

// enableAntiBotOnFind primes /4/find to respond with a PerimeterX
// anti-bot challenge. The resy classifier maps this to
// providers.ErrAntiBotChallenge.
func (f *fakeResyServer) enableAntiBotOnFind() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.antiBotOnFind = true
}

// serveHTTP is the multiplexer. Each branch produces deterministic
// JSON shaped like the real Resy response so the per-endpoint parsers
// in internal/resy decode it without complaint.
func (f *fakeResyServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/3/auth/password":
		f.loginHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":1,"em_address":"e2e@example.com","token":%q,"payment_method":{"id":42}}`, f.jwt)

	case "/3/venue":
		f.venueHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(f.venueBody)

	case "/4/find":
		f.findHits.Add(1)
		f.mu.Lock()
		anti := f.antiBotOnFind
		f.mu.Unlock()
		if anti {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"px-captcha challenge required"}`)
			return
		}
		// One slot at the user's preferred window start (19:00). The
		// slot's config.token round-trips to /3/details as config_id.
		_, _ = io.WriteString(w, `{
			"results":{"venues":[{
				"venue":{"id":49716,"name":"Astoria DC"},
				"slots":[
					{"config":{"id":777,"token":"tok-19","type":"Dining Room"},
					 "date":{"start":"2026-06-10 19:00:00","end":"2026-06-10 21:00:00"}}
				]
			}]}
		}`)

	case "/3/details":
		f.detailsHits.Add(1)
		_, _ = io.WriteString(w, `{
			"book_token":{"value":"e2e-book-tok"},
			"user":{"payment_methods":[{"id":42,"display":"Visa ending 0000"}]}
		}`)

	case "/3/book":
		f.bookHits.Add(1)
		f.mu.Lock()
		expire := f.expireTokenOnce
		if expire {
			f.expireTokenOnce = false
		}
		f.mu.Unlock()
		if expire {
			w.WriteHeader(http.StatusGone)
			_, _ = io.WriteString(w, `{"specific_code":"BOOK_TOKEN_EXPIRED","message":"book token expired"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"resy_token":"e2e-confirmation-xyz","reservation_id":12345}`)

	case "/4/venue/calendar":
		f.calendarHits.Add(1)
		// Empty calendar — the planner falls through to Explicit or
		// Discovered. The test's harness forces the snipe into
		// Awaiting via forceToAwaiting; the strategy choice does not
		// matter for the assertions.
		_, _ = io.WriteString(w, `{"scheduled":[]}`)

	default:
		w.WriteHeader(http.StatusNotFound)
		// Don't echo the request path into the body — it lights up
		// gosec G705 (taint flow) and the test does not need it.
		_, _ = io.WriteString(w, `{"message":"unhandled path"}`)
	}
}

// makeFakeJWT is a local copy of the helper from
// internal/resy/integration_test.go. The adapter only parses the
// payload's exp claim; the signature is ignored. Three base64url
// segments separated by '.' are the minimum the parser accepts.
func makeFakeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(struct {
		Exp int64 `json:"exp"`
	}{Exp: exp.Unix()})
	if err != nil {
		t.Fatalf("marshal jwt: %v", err)
	}
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(payload) + ".sig"
}

// ---- harness --------------------------------------------------------------

// e2eHarness is the wired-up stack the integration tests share: a
// Service, an Engine (for the daemon-runner stand-in), a fake clock,
// the fake Resy server, the SQLite store (so tests can read sessions
// out of band), and the homelab user + resy account identities.
type e2eHarness struct {
	svc        service.Service
	engine     *engine.Engine
	fakeSrv    *fakeResyServer
	clock      *clock.Fake
	sqlStore   *store.SQLiteStore
	resyClient *resy.Client
	userID     domain.UserID
	resyEmail  string
}

// setupE2EBackend builds the harness. Mirrors newStandardService from
// service_wiring.go but with the *resy.Client pointed at the test's
// httptest server and the clock pinned to a fake. Operator + resy
// account are seeded so CreateQuest has an AccountID to consume.
//
// t.Cleanup closes the DB and the httptest server.
func setupE2EBackend(t *testing.T) *e2eHarness {
	t.Helper()
	ctx := context.Background()

	fakeSrv := newFakeResyServer(t)

	// File-backed SQLite under TempDir keeps the test isolated from
	// any developer DB.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "e2e.db")
	rawDB, err := store.Open(ctx, dbPath)
	if err != nil {
		fakeSrv.Close()
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(ctx, rawDB); err != nil {
		_ = rawDB.Close()
		fakeSrv.Close()
		t.Fatalf("store.Migrate: %v", err)
	}

	userID, err := store.SeedOperator(ctx, rawDB, store.SeedOpts{Email: "operator@example.com"})
	if err != nil {
		_ = rawDB.Close()
		fakeSrv.Close()
		t.Fatalf("SeedOperator: %v", err)
	}

	startNow := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(startNow)

	sqlStore := store.NewSQLiteStore(rawDB)
	logger := slog.New(slog.DiscardHandler)

	resyClient := resy.NewClient(logger, clk,
		resy.WithBaseURL(fakeSrv.URL()),
		resy.WithAPIKey("test-key"),
		resy.WithHTTPClient(fakeSrv.HTTPClient()),
		resy.WithUserAgent("e2e-test"),
		resy.WithStore(newSessionStoreAdapter(sqlStore)),
	)

	provider := &providerAdapter{Client: resyClient}
	res := resolver.New(provider, newResolverCacheAdapter(sqlStore), clk, resolver.WithLogger(logger))
	eng := engine.New(sqlStore, clk, logger, engine.WithProvider(provider))
	auth := newResyAuthAdapter(resyClient)
	storeBackend := newServiceStoreAdapter(sqlStore)

	svc, err := service.New(res, eng, storeBackend, clk,
		service.WithLogger(logger),
		service.WithProvider(provider),
		service.WithAuth(auth),
	)
	if err != nil {
		_ = rawDB.Close()
		fakeSrv.Close()
		t.Fatalf("service.New: %v", err)
	}

	// Login through the Service so the resy_sessions row and the v2
	// accounts row both exist for the operator. The fake server's
	// /3/auth/password handler answers any (email, password).
	resyEmail := "e2e@example.com"
	if _, err := svc.Login(ctx, userID, resyEmail, "pw"); err != nil {
		_ = rawDB.Close()
		fakeSrv.Close()
		t.Fatalf("svc.Login: %v", err)
	}

	t.Cleanup(func() {
		_ = rawDB.Close()
		fakeSrv.Close()
	})

	return &e2eHarness{
		svc:        svc,
		engine:     eng,
		fakeSrv:    fakeSrv,
		clock:      clk,
		sqlStore:   sqlStore,
		resyClient: resyClient,
		userID:     userID,
		resyEmail:  resyEmail,
	}
}

// sampleGoal returns a Goal valid for the harness's Astoria DC fixture.
// Date is far enough out that goal.Validate passes against the fake
// clock's "now".
func (h *e2eHarness) sampleGoal() domain.Goal {
	return domain.Goal{
		VenueQuery: domain.VenueQuerySlug{Slug: "astoria-dc", City: "washington-dc"},
		Date:       domain.NewDate(2026, time.June, 10),
		Party:      2,
		TimePrefs: domain.TimeWindow{
			Start:    domain.WallTime{Hour: 19, Minute: 0},
			End:      domain.WallTime{Hour: 21, Minute: 0},
			Priority: domain.PriorityEarlier,
		},
		AccountID: domain.AccountID(h.resyEmail),
	}
}

// session returns the persisted Resy session for the harness's
// operator. RunBookingRace's PrepareSlot / ConfirmSlot need it.
func (h *e2eHarness) session(t *testing.T) providers.Session {
	t.Helper()
	sess, err := h.resyClient.LoadSession(context.Background(), domain.UserID(h.resyEmail))
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	return sess
}

// driveQuestToBooking stands in for the M2 daemon runner. The Service
// has already submitted the engine intent inside CreateQuest; this
// helper jumps the snipe to Awaiting (skipping the release-strategy
// poll loop, since our empty-calendar fake would not converge
// Discovered, and the Service has no release-config knob to force
// Explicit in M1) and runs the booking race. The race issues /4/find,
// PrepareSlot (/3/details), ConfirmSlot (/3/book) — the same pipeline
// A8 cares about.
//
// Returns the persisted terminal status and any race error.
func (h *e2eHarness) driveQuestToBooking(
	ctx context.Context,
	t *testing.T,
	questID domain.QuestID,
	sess providers.Session,
) (domain.Status, error) {
	t.Helper()
	sid := domain.SnipeID(questID)

	// Walk Submitted → Scheduled → Awaiting via the legal transition
	// path. We use the engine's own Transition gate so the events
	// emitted look exactly like a real release would have produced.
	if err := forceToAwaiting(ctx, h.engine, sid); err != nil {
		return domain.StatusFailed, fmt.Errorf("forceToAwaiting: %w", err)
	}

	state, err := h.engine.Load(ctx, sid)
	if err != nil {
		return domain.StatusFailed, fmt.Errorf("reload: %w", err)
	}

	raceErr := h.engine.RunBookingRace(ctx, state, sess)
	final, loadErr := h.engine.Load(ctx, sid)
	if loadErr != nil {
		return domain.StatusFailed, fmt.Errorf("reload after race: %w", loadErr)
	}
	if raceErr != nil {
		return final.Status(), raceErr
	}
	return final.Status(), nil
}

// forceToAwaiting drives a snipe through the legal Submitted →
// Scheduled → Awaiting walk via the engine's Transition gate. It is
// the M1 test stand-in for an Explicit release that has already fired
// — RunBookingRace expects a snipe in Awaiting.
//
// Each transition uses the same EventType the real release path would
// emit, so subscribers observe the same sequence they would have on a
// production snipe.
func forceToAwaiting(ctx context.Context, eng *engine.Engine, id domain.SnipeID) error {
	for range 6 {
		state, err := eng.Load(ctx, id)
		if err != nil {
			return err
		}
		switch state.Status() {
		case domain.StatusAwaiting:
			return nil
		case domain.StatusSubmitted:
			if err := state.Transition(ctx, domain.StatusScheduled, domain.EventScheduled,
				slog.String("forced_by", "e2e_test"),
			); err != nil {
				return err
			}
		case domain.StatusScheduled:
			if err := state.Transition(ctx, domain.StatusAwaiting, domain.EventReleased,
				slog.String("forced_by", "e2e_test"),
			); err != nil {
				return err
			}
		case domain.StatusDiscovering:
			if err := state.Transition(ctx, domain.StatusAwaiting, domain.EventReleased,
				slog.String("forced_by", "e2e_test"),
			); err != nil {
				return err
			}
		default:
			return fmt.Errorf("forceToAwaiting: cannot route from %s", state.Status())
		}
	}
	return errors.New("forceToAwaiting: too many hops")
}

// ---- tests ----------------------------------------------------------------

// TestE2EGoalToBooked is the keystone test for milestone acceptance
// criterion A8: Goal → Service.PlanQuest → Service.CreateQuest →
// engine drives /4/find → /3/details → /3/book → terminal Booked,
// against an httptest fixture. Every layer is the real production
// wiring; only the network and clock are stubbed.
func TestE2EGoalToBooked(t *testing.T) {
	t.Parallel()

	h := setupE2EBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	goal := h.sampleGoal()

	// PlanQuest is the pure-computation seam: it resolves the venue
	// (hitting /3/venue once via the resolver) and returns a Plan with
	// a non-empty hash.
	plan, err := h.svc.PlanQuest(ctx, h.userID, goal)
	if err != nil {
		t.Fatalf("PlanQuest: %v", err)
	}
	if plan.Hash == "" {
		t.Fatal("PlanQuest returned empty Hash")
	}
	if plan.Venue.Ref != "49716" {
		t.Errorf("PlanQuest venue ref: got %q want 49716 (Astoria DC fixture)", plan.Venue.Ref)
	}

	// CreateQuest persists the quest, submits to the engine, and
	// returns the v2 QuestID. The PlanHash pin guarantees no plan
	// drift between preview and commit (ADR-0012).
	hash := plan.Hash
	questID, err := h.svc.CreateQuest(ctx, h.userID, goal, service.CreateOpts{PlanHash: &hash})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	if questID == "" {
		t.Fatal("CreateQuest returned empty QuestID")
	}

	// Read the quest back via the Service — verifies the v2 quests row
	// was persisted under the operator's userID with the right Goal
	// and AccountID linkage.
	state, err := h.svc.GetQuest(ctx, h.userID, questID)
	if err != nil {
		t.Fatalf("GetQuest: %v", err)
	}
	if state.Summary.UserID != h.userID {
		t.Errorf("GetQuest UserID: got %q want %q", state.Summary.UserID, h.userID)
	}
	if state.Summary.PlanHash != plan.Hash {
		t.Errorf("GetQuest PlanHash: got %q want %q", state.Summary.PlanHash, plan.Hash)
	}

	// Drive the engine through release → /4/find → /3/details → /3/book.
	sess := h.session(t)
	finalStatus, raceErr := h.driveQuestToBooking(ctx, t, questID, sess)
	if raceErr != nil {
		t.Fatalf("driveQuestToBooking: %v (status=%s)", raceErr, finalStatus)
	}
	if finalStatus != domain.StatusBooked {
		t.Fatalf("final status: got %s want %s", finalStatus, domain.StatusBooked)
	}

	// Verify the engine hit each endpoint exactly once on the happy
	// path. The exact counts assert the engine walked the documented
	// pipeline with no spurious retries.
	if got := h.fakeSrv.findHits.Load(); got != 1 {
		t.Errorf("/4/find hits: got %d want 1", got)
	}
	if got := h.fakeSrv.detailsHits.Load(); got != 1 {
		t.Errorf("/3/details hits: got %d want 1", got)
	}
	if got := h.fakeSrv.bookHits.Load(); got != 1 {
		t.Errorf("/3/book hits: got %d want 1", got)
	}

	// Verify the v1 snipe state carries the confirmation matching the
	// fake server's response. This is the proof the booking pipeline
	// round-tripped the resy_token end-to-end.
	loaded, err := h.engine.Load(ctx, domain.SnipeID(questID))
	if err != nil {
		t.Fatalf("engine.Load: %v", err)
	}
	if loaded.Inner().Result == nil {
		t.Fatal("engine state has no Result after Booked")
	}
	if loaded.Inner().Result.ConfirmationID != "e2e-confirmation-xyz" {
		t.Errorf("ConfirmationID: got %q want e2e-confirmation-xyz",
			loaded.Inner().Result.ConfirmationID)
	}
}

// TestE2EGoalToBookedSubscribeEvents covers the same happy path but
// with a parallel SubscribeQuest goroutine. It asserts the subscriber
// observes the engine's booking-phase transitions (Found,
// BookAttempted, Booked).
//
// SubscribeQuest's replay path (ListQuestEvents) is usually empty in
// M1 because the quest_events writer lands in a sibling agent's work
// (M1-11). We assert on the live engine.Subscribe stream only.
func TestE2EGoalToBookedSubscribeEvents(t *testing.T) {
	t.Parallel()

	h := setupE2EBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	goal := h.sampleGoal()
	plan, err := h.svc.PlanQuest(ctx, h.userID, goal)
	if err != nil {
		t.Fatalf("PlanQuest: %v", err)
	}
	hash := plan.Hash
	questID, err := h.svc.CreateQuest(ctx, h.userID, goal, service.CreateOpts{PlanHash: &hash})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	// Subscribe in a background goroutine. Events land on a channel
	// the test drains after the race completes.
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	events := make(chan domain.Event, 32)
	subDone := make(chan error, 1)
	go func() {
		subDone <- h.svc.SubscribeQuest(subCtx, h.userID, questID, func(ev domain.Event) {
			select {
			case events <- ev:
			default:
			}
		})
	}()
	// Give the subscriber a moment to register before transitions
	// fire. A small real-time sleep is acceptable: the engine path
	// dominates wall time and we only need the goroutine scheduled.
	time.Sleep(10 * time.Millisecond)

	sess := h.session(t)
	finalStatus, raceErr := h.driveQuestToBooking(ctx, t, questID, sess)
	if raceErr != nil {
		t.Fatalf("driveQuestToBooking: %v (status=%s)", raceErr, finalStatus)
	}
	if finalStatus != domain.StatusBooked {
		t.Fatalf("final status: got %s want Booked", finalStatus)
	}

	// SubscribeQuest exits cleanly when a terminal event arrives.
	select {
	case sErr := <-subDone:
		if sErr != nil {
			t.Fatalf("SubscribeQuest returned %v", sErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SubscribeQuest did not return after terminal event")
	}
	close(events)

	// Collect every observed event type and assert the booking-phase
	// transitions are present.
	seen := map[domain.EventType]bool{}
	for ev := range events {
		seen[ev.Type] = true
	}
	mustSee := []domain.EventType{
		domain.EventFound,
		domain.EventBookAttempted,
		domain.EventBooked,
	}
	for _, et := range mustSee {
		if !seen[et] {
			t.Errorf("subscriber missed %s; seen=%v", et, seen)
		}
	}
}

// TestE2EAntiBotChallengeFails covers the failure path where /4/find
// returns a PerimeterX anti-bot challenge. With no signer wired (the
// test uses the default Noop), the engine cannot recover and the
// snipe lands on Failed. This exercises the error mapping from
// providers.ErrAntiBotChallenge through the engine's prepare_blocked
// branch.
func TestE2EAntiBotChallengeFails(t *testing.T) {
	t.Parallel()

	h := setupE2EBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.fakeSrv.enableAntiBotOnFind()

	goal := h.sampleGoal()
	plan, err := h.svc.PlanQuest(ctx, h.userID, goal)
	if err != nil {
		t.Fatalf("PlanQuest: %v", err)
	}
	hash := plan.Hash
	questID, err := h.svc.CreateQuest(ctx, h.userID, goal, service.CreateOpts{PlanHash: &hash})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	sess := h.session(t)
	finalStatus, raceErr := h.driveQuestToBooking(ctx, t, questID, sess)
	if raceErr == nil {
		t.Fatalf("expected RunBookingRace error, got nil (status=%s)", finalStatus)
	}
	if !errors.Is(raceErr, providers.ErrAntiBotChallenge) &&
		!strings.Contains(raceErr.Error(), "anti-bot") &&
		!strings.Contains(raceErr.Error(), "anti_bot") {
		t.Errorf("error not anti-bot-shaped: %v", raceErr)
	}
	if finalStatus != domain.StatusFailed {
		t.Errorf("final status: got %s want Failed", finalStatus)
	}
}

// TestE2EBookTokenExpiredFails covers the failure path where /3/book
// rejects the token. With one slot and no retry path, the snipe ends
// in Failed and the bookHits counter is exactly 1.
func TestE2EBookTokenExpiredFails(t *testing.T) {
	t.Parallel()

	h := setupE2EBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.fakeSrv.expireBookTokenOnce()

	goal := h.sampleGoal()
	plan, err := h.svc.PlanQuest(ctx, h.userID, goal)
	if err != nil {
		t.Fatalf("PlanQuest: %v", err)
	}
	hash := plan.Hash
	questID, err := h.svc.CreateQuest(ctx, h.userID, goal, service.CreateOpts{PlanHash: &hash})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	sess := h.session(t)
	finalStatus, raceErr := h.driveQuestToBooking(ctx, t, questID, sess)
	if raceErr == nil {
		t.Fatalf("expected RunBookingRace error, got nil (status=%s)", finalStatus)
	}
	if finalStatus != domain.StatusFailed {
		t.Errorf("final status: got %s want Failed", finalStatus)
	}
	if got := h.fakeSrv.bookHits.Load(); got != 1 {
		t.Errorf("/3/book hits: got %d want 1 (no retry path on this failure)", got)
	}
}
