package service_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// TestSubscribeQuestWrongOwnerReturnsNotFound asserts the tenancy
// gate at the SubscribeQuest entry. A caller subscribing to somebody
// else's quest must observe the same opaque ErrNotFound that GetQuest
// produces — the design folds existence and ownership into one signal
// so a tenant cannot probe another tenant's quest IDs.
func TestSubscribeQuestWrongOwnerReturnsNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	err = h.svc.SubscribeQuest(ctx, h.otherID, qid, func(domain.Event) {
		t.Errorf("callback fired for wrong-owner subscribe")
	})
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("SubscribeQuest wrong owner: want ErrNotFound, got %v", err)
	}
}

// TestSubscribeQuestMissingQuestReturnsNotFound covers the
// missing-row case of the tenancy fold. A typo'd quest id surfaces
// the same ErrNotFound as a wrong-tenant subscription.
func TestSubscribeQuestMissingQuestReturnsNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	err := h.svc.SubscribeQuest(t.Context(), h.uid, "q_doesnotexist", func(domain.Event) {
		t.Errorf("callback fired for missing-quest subscribe")
	})
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("SubscribeQuest missing: want ErrNotFound, got %v", err)
	}
}

// TestSubscribeQuestNilCallbackInvalidArgument fails fast at the
// boundary on a nil callback — a nil cb would silently drop events
// and mask the wiring bug.
func TestSubscribeQuestNilCallbackInvalidArgument(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	err = h.svc.SubscribeQuest(ctx, h.uid, qid, nil)
	if !errors.Is(err, service.ErrInvalidArgument) {
		t.Fatalf("SubscribeQuest nil cb: want ErrInvalidArgument, got %v", err)
	}
}

// seedQuestEvent inserts one row directly into the quest_events table.
// M1-11 will replace this side-channel with engine-emitted writes; for
// M1-13 we use SQL to prime the history-replay path independently of
// the writer.
func seedQuestEvent(
	t *testing.T,
	h *harness,
	questID domain.QuestID,
	ev domain.Event,
) {
	t.Helper()
	ctx := t.Context()

	// quest_events.fields_json mirrors the v1 events.fields_json
	// encoding (store.EncodeAttrs). We do not need attrs in the test
	// assertions, so leaving fields_json NULL keeps the seed minimal.
	_ = ev.Attrs
	_, err := h.db.DB().ExecContext(ctx, `
		INSERT INTO quest_events (quest_id, user_id, type, at, fields_json)
		VALUES (?, ?, ?, ?, NULL)`,
		string(questID),
		string(h.uid),
		string(ev.Type),
		ev.At.UnixMilli(),
	)
	if err != nil {
		t.Fatalf("seed quest_events: %v", err)
	}
}

// TestSubscribeQuestReplaysHistory asserts the replay-then-live
// pattern: prior quest_events rows are delivered to cb in
// chronological order before the live subscription is established.
//
// We pre-cancel the context so SubscribeQuest exits immediately
// after the replay (it would otherwise block waiting for live
// emissions). This isolates the replay assertion from the engine
// path.
func TestSubscribeQuestReplaysHistory(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	now := h.clk.Now()
	seedQuestEvent(t, h, qid, domain.Event{Type: domain.EventScheduled, At: now.Add(1 * time.Second)})
	seedQuestEvent(t, h, qid, domain.Event{Type: domain.EventReleased, At: now.Add(2 * time.Second)})
	seedQuestEvent(t, h, qid, domain.Event{Type: domain.EventFound, At: now.Add(3 * time.Second)})

	var mu sync.Mutex
	var got []domain.EventType
	cb := func(ev domain.Event) {
		mu.Lock()
		got = append(got, ev.Type)
		mu.Unlock()
	}

	// Use a short-deadline subscribe so the live path exits quickly
	// after replay drains. Cancel directly after a beat to keep the
	// test fast.
	subCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	if err := h.svc.SubscribeQuest(subCtx, h.uid, qid, cb); err != nil {
		t.Fatalf("SubscribeQuest: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	want := []domain.EventType{
		domain.EventScheduled,
		domain.EventReleased,
		domain.EventFound,
	}
	// CreateQuest (M1-11) writes a "submitted" quest_event before
	// the seeded ones; the replay therefore delivers it first. Skip
	// past any leading non-want events and then assert the seeded
	// suffix matches in chronological order.
	if len(got) < len(want) {
		t.Fatalf("replay events: got %v want at least %v", got, want)
	}
	offset := 0
	for offset < len(got) && got[offset] != want[0] {
		offset++
	}
	if offset+len(want) > len(got) {
		t.Fatalf("replay events: got %v want suffix %v", got, want)
	}
	for i := range want {
		if got[offset+i] != want[i] {
			t.Errorf("replay[%d]: got %s want %s", i, got[offset+i], want[i])
		}
	}
}

// recordingCB is a thread-safe domain.Event collector. The engine
// subscriber runs on its emit goroutine while SubscribeQuest's pump
// runs on the test goroutine, so the captured slice needs a mutex.
type recordingCB struct {
	mu     sync.Mutex
	events []domain.Event
	done   chan struct{}
	// terminalTypes is a closing signal: when the cb sees one of
	// these, it closes done so the test can synchronize without
	// polling.
	terminalTypes map[domain.EventType]struct{}
}

func newRecordingCB(terminal ...domain.EventType) *recordingCB {
	r := &recordingCB{
		done:          make(chan struct{}),
		terminalTypes: map[domain.EventType]struct{}{},
	}
	for _, t := range terminal {
		r.terminalTypes[t] = struct{}{}
	}
	return r
}

func (r *recordingCB) fn(ev domain.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	_, term := r.terminalTypes[ev.Type]
	r.mu.Unlock()
	if term {
		// Closing done is idempotent-by-construction: cb only
		// fires for the matching quest, and a quest reaches a
		// terminal state at most once.
		select {
		case <-r.done:
		default:
			close(r.done)
		}
	}
}

func (r *recordingCB) snapshot() []domain.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.Event, len(r.events))
	copy(out, r.events)
	return out
}

// TestSubscribeQuestLiveDeliversEngineTransition wires the full
// replay-then-live path: CreateQuest persists and submits the snipe,
// SubscribeQuest registers, and then an engine.Transition fires a
// Scheduled emission that the subscriber must receive.
//
// The test is intentionally permissive about replay: the legacy v1
// snipe Submitted emission happened during CreateQuest (before
// SubscribeQuest was called) and never landed in quest_events
// (M1-11 territory), so we assert only that the live Scheduled
// reaches cb — not that the prior Submitted does.
func TestSubscribeQuestLiveDeliversEngineTransition(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	rec := newRecordingCB(domain.EventScheduled)

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- h.svc.SubscribeQuest(subCtx, h.uid, qid, rec.fn)
	}()

	// The subscribe goroutine must register with the engine before
	// the Transition fires, otherwise the live notification is
	// missed. There is no public "subscription installed" signal,
	// so we busy-wait for the engine to gain a subscriber by
	// retrying the transition until at least one event lands. In
	// practice the goroutine schedules within microseconds.
	st, err := h.eng.Load(ctx, domain.SnipeID(qid))
	if err != nil {
		t.Fatalf("engine.Load: %v", err)
	}
	// Wait for the SubscribeQuest goroutine to register its engine
	// subscriber. There is no public introspection of the engine's
	// subscriber list, so we use a generous fixed sleep — the
	// SubscribeQuest replay reads an empty quest_events table and
	// then calls engine.Subscribe synchronously, so even under
	// -race + parallel test load 200ms is comfortable. If this
	// flakes, raise the sleep rather than ratcheting down: the
	// scenario under test is the live path, not subscription
	// latency.
	time.Sleep(200 * time.Millisecond)

	if err := st.Transition(ctx, domain.StatusScheduled, domain.EventScheduled,
		slog.String("test", "live")); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	select {
	case <-rec.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive live Scheduled event; got=%v", rec.snapshot())
	}

	// Tear down the subscription. The cb saw a terminal-event
	// match (in this test, Scheduled is the terminal-of-interest
	// for rec, NOT for the snipe), so we have to close ctx to
	// unblock the pump — Scheduled is not in isTerminalEvent.
	cancel()

	if err := <-errCh; err != nil {
		t.Fatalf("SubscribeQuest: %v", err)
	}

	got := rec.snapshot()
	if len(got) == 0 {
		t.Fatalf("no events received")
	}
	if got[len(got)-1].Type != domain.EventScheduled {
		t.Errorf("last event type = %s, want scheduled", got[len(got)-1].Type)
	}
}

// TestSubscribeQuestFiltersByQuestID asserts that a notification for a
// different snipe never reaches the callback. The engine fans out to
// every subscriber regardless of SnipeID; the filter lives inside
// SubscribeQuest.
func TestSubscribeQuestFiltersByQuestID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	// Build a second goal with a different date so its hash differs.
	otherGoal := h.sampleGoal()
	otherGoal.Date = domain.NewDate(2026, time.July, 14)
	otherQID, err := h.svc.CreateQuest(ctx, h.uid, otherGoal, service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest other: %v", err)
	}

	rec := newRecordingCB()

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- h.svc.SubscribeQuest(subCtx, h.uid, qid, rec.fn)
	}()
	// Give the subscribe goroutine a beat to register.
	time.Sleep(20 * time.Millisecond)

	// Transition the OTHER snipe — the subscriber must filter it out.
	other, err := h.eng.Load(ctx, domain.SnipeID(otherQID))
	if err != nil {
		t.Fatalf("engine.Load other: %v", err)
	}
	if err := other.Transition(ctx, domain.StatusScheduled, domain.EventScheduled); err != nil {
		t.Fatalf("Transition other: %v", err)
	}
	// Give the engine a beat to fan out (it is synchronous so this
	// is just for goroutine scheduling).
	time.Sleep(20 * time.Millisecond)

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("SubscribeQuest: %v", err)
	}

	// CreateQuest (M1-11) writes a "submitted" quest_event for qid
	// before SubscribeQuest registers; the replay therefore delivers
	// it. The test's concern is leakage from otherQID, not the
	// presence of qid's own history — assert no event carries
	// otherQID's identity. We do not have direct SnipeID on the
	// callback's domain.Event (it is flattened in by quest_id at the
	// store layer), so we sanity-check by ensuring nothing past the
	// initial submitted slipped through. The engine emit for
	// otherQID's Scheduled transition would surface as an
	// EventScheduled here; its absence proves the filter holds.
	for _, ev := range rec.snapshot() {
		if ev.Type == domain.EventScheduled {
			t.Errorf("Scheduled event leaked from other quest: %+v", ev)
		}
	}
}

// TestSubscribeQuestCtxCancelReturnsNil verifies that canceling the
// context exits SubscribeQuest cleanly and promptly — the transport
// layers rely on ctx.Done as the "client disconnected" signal.
func TestSubscribeQuestCtxCancelReturnsNil(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	subCtx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- h.svc.SubscribeQuest(subCtx, h.uid, qid, func(domain.Event) {})
	}()
	// Let the subscribe goroutine settle.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("SubscribeQuest after ctx cancel: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("SubscribeQuest did not return within 500ms of ctx cancel")
	}
}

// TestSubscribeQuestTerminalEventClosesStream asserts the
// terminal-event short-circuit: once a Booked / Failed / Canceled /
// Expired event is delivered, SubscribeQuest returns nil without
// waiting for ctx.Done. This is the path HTTP-SSE and MCP-streaming
// rely on to close the response after the booking outcome lands.
func TestSubscribeQuestTerminalEventClosesStream(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	rec := newRecordingCB(domain.EventCanceled)

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- h.svc.SubscribeQuest(subCtx, h.uid, qid, rec.fn)
	}()
	time.Sleep(20 * time.Millisecond)

	st, err := h.eng.Load(ctx, domain.SnipeID(qid))
	if err != nil {
		t.Fatalf("engine.Load: %v", err)
	}
	if err := st.Transition(ctx, domain.StatusCanceled, domain.EventCanceled); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("SubscribeQuest after terminal event: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("SubscribeQuest did not return after terminal event")
	}

	got := rec.snapshot()
	if len(got) == 0 || got[len(got)-1].Type != domain.EventCanceled {
		t.Errorf("last event = %v, want canceled emission", got)
	}
}

// TestSubscribeQuestAlreadyTerminalShortCircuits asserts that
// subscribing to a quest that is already terminal returns nil
// immediately after replay — no live wait. CancelQuest moves the
// status to Canceled at the quest row level; the live engine path
// is unnecessary for a quest that will never emit again.
func TestSubscribeQuestAlreadyTerminalShortCircuits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	if err := h.svc.CancelQuest(ctx, h.uid, qid, service.CancelOpts{}); err != nil {
		t.Fatalf("CancelQuest: %v", err)
	}

	// Subscribe without canceling ctx — the call must return on
	// its own because the quest is already terminal.
	start := time.Now()
	err = h.svc.SubscribeQuest(ctx, h.uid, qid, func(domain.Event) {})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("SubscribeQuest already-terminal: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("SubscribeQuest blocked for %v on a terminal quest; want immediate return", elapsed)
	}
}

// TestSubscribeQuestUnknownToEngineReturnsAfterCtxDone covers the
// "persisted but not yet scheduled" defensive branch. The quest row
// exists, the engine doesn't have it loaded (we never called
// CreateQuest), and the caller is expected to block on ctx.Done.
//
// We simulate this by inserting a quest row directly via SQL — no
// engine.Submit — and then canceling the context after a beat. The
// call must return nil; an error here would indicate the engine
// subscribe path is rejecting unknown snipe ids, which the spec
// forbids.
func TestSubscribeQuestUnknownToEngineReturnsAfterCtxDone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid := domain.QuestID("q_unsched")
	// Bypass CreateQuest so the engine never learns about this id.
	_, err := h.db.DB().ExecContext(ctx, `
		INSERT INTO quests (id, user_id, account_id, goal_json, plan_hash,
		    status, created_at, updated_at, completed_at)
		VALUES (?, ?, ?, '{}', '', 'pending', ?, ?, NULL)`,
		string(qid), string(h.uid), string(h.acctID),
		h.clk.Now().UnixMilli(), h.clk.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("seed quest row: %v", err)
	}

	subCtx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- h.svc.SubscribeQuest(subCtx, h.uid, qid, func(domain.Event) {})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			// The engine.Subscribe API documented in
			// internal/engine/subscribe.go always succeeds (it's
			// a pure registration), so any error here points at a
			// regression in the bridge layer. Surface the full
			// message for debugging.
			if strings.Contains(err.Error(), "subscribe") {
				t.Errorf("unexpected subscribe error: %v", err)
			} else {
				t.Errorf("SubscribeQuest unknown-to-engine: %v", err)
			}
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("SubscribeQuest did not return after ctx cancel")
	}
}
