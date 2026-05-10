package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
	"resy-snipe/internal/store"
)

// listAuditByAction returns audit_events rows for (uid, action),
// ordered most-recent-first. The helper reads the raw v2 table via
// store.ListAuditEvents — the same package-level seam Service uses
// for its (future) operator-admin reader.
func listAuditByAction(t *testing.T, h *harness, uid domain.UserID, action string) []store.AuditEventRow {
	t.Helper()
	rows, err := store.ListAuditEvents(t.Context(), h.db.DB(), uid, store.AuditEventFilter{
		Action: action,
	})
	if err != nil {
		t.Fatalf("ListAuditEvents(%s): %v", action, err)
	}
	return rows
}

// TestAuditWriteEveryServiceMethod verifies the §audit-log-contract
// invariant: every public Service method writes one audit row before
// returning. We invoke each method once on a clean harness and then
// assert exactly-one matching row exists for the expected action.
func TestAuditWriteEveryServiceMethod(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	// ResolveVenue
	if _, err := h.svc.ResolveVenue(ctx, h.uid,
		domain.VenueQuerySlug{Slug: "carbone", City: "ny"}); err != nil {
		t.Fatalf("ResolveVenue: %v", err)
	}
	if rows := listAuditByAction(t, h, h.uid, "venue.resolve"); len(rows) != 1 || !rows[0].OK {
		t.Errorf("venue.resolve audit: got %+v", rows)
	}

	// PlanQuest
	if _, err := h.svc.PlanQuest(ctx, h.uid, h.sampleGoal()); err != nil {
		t.Fatalf("PlanQuest: %v", err)
	}
	// PlanQuest's audit row count after one direct call equals 1.
	// (Subsequent CreateQuest calls would add more.)
	if rows := listAuditByAction(t, h, h.uid, "quest.plan"); len(rows) != 1 || !rows[0].OK {
		t.Errorf("quest.plan audit: got %d rows %+v", len(rows), rows)
	}

	// CreateQuest. Note: CreateQuest internally invokes PlanQuest,
	// which writes an extra quest.plan row — that is the documented
	// "every Service call writes one row" behavior; the inner call
	// is still a Service-method invocation.
	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	createRows := listAuditByAction(t, h, h.uid, "quest.create")
	if len(createRows) != 1 || !createRows[0].OK || createRows[0].TargetID != string(qid) {
		t.Errorf("quest.create audit: got %+v", createRows)
	}

	// GetQuest
	if _, err := h.svc.GetQuest(ctx, h.uid, qid); err != nil {
		t.Fatalf("GetQuest: %v", err)
	}
	if rows := listAuditByAction(t, h, h.uid, "quest.get"); len(rows) != 1 || !rows[0].OK || rows[0].TargetID != string(qid) {
		t.Errorf("quest.get audit: got %+v", rows)
	}

	// ListQuests
	if _, err := h.svc.ListQuests(ctx, h.uid, service.ListFilter{}); err != nil {
		t.Fatalf("ListQuests: %v", err)
	}
	if rows := listAuditByAction(t, h, h.uid, "quest.list"); len(rows) != 1 || !rows[0].OK {
		t.Errorf("quest.list audit: got %+v", rows)
	}

	// CancelQuest
	if err := h.svc.CancelQuest(ctx, h.uid, qid, service.CancelOpts{Reason: "test"}); err != nil {
		t.Fatalf("CancelQuest: %v", err)
	}
	if rows := listAuditByAction(t, h, h.uid, "quest.cancel"); len(rows) != 1 || !rows[0].OK || rows[0].TargetID != string(qid) {
		t.Errorf("quest.cancel audit: got %+v", rows)
	}

	// ListAccounts
	if _, err := h.svc.ListAccounts(ctx, h.uid); err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if rows := listAuditByAction(t, h, h.uid, "account.list"); len(rows) != 1 || !rows[0].OK {
		t.Errorf("account.list audit: got %+v", rows)
	}

	// Login
	if _, err := h.svc.Login(ctx, h.uid, "audit@example.com", "pw"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if rows := listAuditByAction(t, h, h.uid, "account.login"); len(rows) != 1 || !rows[0].OK {
		t.Errorf("account.login audit: got %+v", rows)
	}

	// Operator-admin stubs — each writes an ok=false row with
	// error_code = not_implemented.
	_, _ = h.svc.InviteUser(ctx, h.uid, "x@y.z", "user")
	_, _ = h.svc.RotateToken(ctx, h.uid, "cli")
	_ = h.svc.RevokeToken(ctx, h.uid, "tok-1")
	_, _ = h.svc.ListUsers(ctx, h.uid)
	for _, a := range []string{"user.invite", "token.rotate", "token.revoke", "user.list"} {
		rows := listAuditByAction(t, h, h.uid, a)
		if len(rows) != 1 || rows[0].OK || rows[0].ErrorCode != "not_implemented" {
			t.Errorf("%s audit: got %+v", a, rows)
		}
	}
}

// TestAuditWriteFailureRecordsErrorCode asserts that a failing
// Service call writes an ok=0 audit row whose error_code matches the
// sentinel name. We trigger ErrVenueNotFound through ResolveVenue.
func TestAuditWriteFailureRecordsErrorCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	_, err := h.svc.ResolveVenue(t.Context(), h.uid,
		domain.VenueQuerySlug{Slug: "missing", City: "??"})
	if !errors.Is(err, service.ErrVenueNotFound) {
		t.Fatalf("ResolveVenue: want ErrVenueNotFound, got %v", err)
	}
	rows := listAuditByAction(t, h, h.uid, "venue.resolve")
	if len(rows) != 1 {
		t.Fatalf("venue.resolve audit: got %d rows want 1", len(rows))
	}
	if rows[0].OK {
		t.Errorf("audit row OK on a failed call: %+v", rows[0])
	}
	if rows[0].ErrorCode != "venue_not_found" {
		t.Errorf("audit error_code: got %q want venue_not_found", rows[0].ErrorCode)
	}
}

// failingAuditBackend wraps testStoreBackend but forces every audit
// write to fail. Used to assert the audit-write failure-mode policy
// (log WARN, never fail the parent call).
type failingAuditBackend struct {
	*testStoreBackend

	auditErr error
}

func (f *failingAuditBackend) WriteAuditEvent(_ context.Context, _ service.AuditWrite) error {
	return f.auditErr
}

// TestAuditWriteFailureDoesNotFailParentCall asserts that a Service
// call whose audit write fails still returns its normal result. The
// failure is observable only through the WARN log (which we discard).
func TestAuditWriteFailureDoesNotFailParentCall(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// Swap in a backend whose WriteAuditEvent always fails.
	fb := &failingAuditBackend{
		testStoreBackend: newTestStoreBackend(h.db),
		auditErr:         errors.New("forced audit-write failure"),
	}
	// Rebuild the Service with the failing backend; reuse the
	// engine/resolver/auth/clock from the harness.
	svc, err := service.New(&fakeResolver{venues: map[string]domain.Venue{
		domain.VenueQuerySlug{Slug: "carbone", City: "ny"}.String(): h.venue,
	}}, h.eng, fb, h.clk,
		service.WithProvider(h.prov),
		service.WithAuth(h.auth),
	)
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}

	// ResolveVenue must still succeed even though every WriteAuditEvent
	// call fails. The audit failure surfaces as a WARN line in the
	// service logger, which the harness discards.
	v, err := svc.ResolveVenue(t.Context(), h.uid,
		domain.VenueQuerySlug{Slug: "carbone", City: "ny"})
	if err != nil {
		t.Fatalf("ResolveVenue: %v", err)
	}
	if v.Ref != h.venue.Ref {
		t.Errorf("ResolveVenue ref: got %q want %q", v.Ref, h.venue.Ref)
	}
}

// TestCreateQuestWritesSubmittedQuestEvent asserts CreateQuest
// persists a "submitted" row into quest_events so subsequent
// GetQuest / SubscribeQuest replays surface the lifecycle's first
// beat.
func TestCreateQuestWritesSubmittedQuestEvent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	events, err := store.ListQuestEvents(ctx, h.db.DB(), h.uid, qid, 0)
	if err != nil {
		t.Fatalf("ListQuestEvents: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == domain.EventSubmitted {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("submitted quest_event not found; got %+v", events)
	}
}

// TestCancelQuestWritesCanceledQuestEvent asserts CancelQuest
// persists a "canceled" row into quest_events.
func TestCancelQuestWritesCanceledQuestEvent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	if err := h.svc.CancelQuest(ctx, h.uid, qid, service.CancelOpts{Reason: "test"}); err != nil {
		t.Fatalf("CancelQuest: %v", err)
	}
	events, err := store.ListQuestEvents(ctx, h.db.DB(), h.uid, qid, 0)
	if err != nil {
		t.Fatalf("ListQuestEvents: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == domain.EventCanceled {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("canceled quest_event not found; got %+v", events)
	}
}

// TestGetQuestPopulatesEvents asserts the M1-11 wiring of
// GetQuest.QuestState.Events: the events list is non-empty for a
// quest that has progressed (submitted + scheduled).
func TestGetQuestPopulatesEvents(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	state, err := h.svc.GetQuest(ctx, h.uid, qid)
	if err != nil {
		t.Fatalf("GetQuest: %v", err)
	}
	if len(state.Events) == 0 {
		t.Fatalf("QuestState.Events empty after CreateQuest")
	}
	// The first persisted event is "submitted" (chronological
	// order, oldest first).
	if state.Events[0].Type != domain.EventSubmitted {
		t.Errorf("Events[0].Type: got %s want submitted", state.Events[0].Type)
	}
}

// TestSubscribeQuestPersistsLiveEngineTransition asserts the engine
// → quest_events bridge: a live engine.Transition that fires while a
// SubscribeQuest call is active produces a quest_events row.
func TestSubscribeQuestPersistsLiveEngineTransition(t *testing.T) {
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
	// Give the subscribe goroutine a beat to register; mirrors
	// TestSubscribeQuestLiveDeliversEngineTransition.
	time.Sleep(200 * time.Millisecond)

	st, err := h.eng.Load(ctx, domain.SnipeID(qid))
	if err != nil {
		t.Fatalf("engine.Load: %v", err)
	}
	if err := st.Transition(ctx, domain.StatusScheduled, domain.EventScheduled); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	select {
	case <-rec.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("live Scheduled not delivered to cb; got=%v", rec.snapshot())
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("SubscribeQuest: %v", err)
	}

	// Assert the Scheduled event landed in quest_events.
	events, err := store.ListQuestEvents(ctx, h.db.DB(), h.uid, qid, 0)
	if err != nil {
		t.Fatalf("ListQuestEvents: %v", err)
	}
	foundScheduled := false
	for _, ev := range events {
		if ev.Type == domain.EventScheduled {
			foundScheduled = true
			break
		}
	}
	if !foundScheduled {
		t.Errorf("scheduled quest_event not persisted; got %+v", events)
	}
}

// TestSubscribeQuestAuditLoggedOnceAtStart asserts SubscribeQuest
// writes exactly one audit row at subscription start — not one per
// delivered event.
func TestSubscribeQuestAuditLoggedOnceAtStart(t *testing.T) {
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
	time.Sleep(50 * time.Millisecond)

	// Drive several engine transitions through the live path.
	st, err := h.eng.Load(ctx, domain.SnipeID(qid))
	if err != nil {
		t.Fatalf("engine.Load: %v", err)
	}
	if err := st.Transition(ctx, domain.StatusScheduled, domain.EventScheduled); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("SubscribeQuest: %v", err)
	}

	rows := listAuditByAction(t, h, h.uid, "quest.subscribe")
	if len(rows) != 1 {
		t.Errorf("quest.subscribe audit rows: got %d want 1 (one per subscription, not per event)", len(rows))
	}
}

// TestSubscribeQuestWrongOwnerAuditsFailure asserts the
// failed-tenancy-gate audit row: a wrong-owner SubscribeQuest still
// writes one audit_events row (ok=0).
func TestSubscribeQuestWrongOwnerAuditsFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	err = h.svc.SubscribeQuest(ctx, h.otherID, qid, func(domain.Event) {})
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("SubscribeQuest wrong owner: %v", err)
	}
	rows := listAuditByAction(t, h, h.otherID, "quest.subscribe")
	if len(rows) != 1 || rows[0].OK || rows[0].ErrorCode != "not_found" {
		t.Errorf("wrong-owner quest.subscribe audit: got %+v", rows)
	}
}
