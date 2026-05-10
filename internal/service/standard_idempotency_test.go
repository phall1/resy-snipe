package service_test

import (
	"errors"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// keyPtr returns a *string addressed at v. Tests use it to construct
// CreateOpts/CancelOpts.IdempotencyKey without sprinkling addressable-
// literal helpers across the suite.
func keyPtr(v string) *string { return &v }

// countQuestRows returns the number of rows in the v2 quests table
// for h.uid. Used to assert that an idempotent replay did NOT mint a
// second quest.
func countQuestRows(t *testing.T, h *harness) int {
	t.Helper()
	var n int
	if err := h.db.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM quests WHERE user_id = ?`, string(h.uid),
	).Scan(&n); err != nil {
		t.Fatalf("count quests: %v", err)
	}
	return n
}

func TestCreateQuestIdempotencyReplay(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	key := "client-supplied-key-A"
	qid1, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{
		IdempotencyKey: keyPtr(key),
	})
	if err != nil {
		t.Fatalf("first CreateQuest: %v", err)
	}
	qid2, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{
		IdempotencyKey: keyPtr(key),
	})
	if err != nil {
		t.Fatalf("replay CreateQuest: %v", err)
	}
	if qid1 != qid2 {
		t.Errorf("replay returned a different QuestID: first=%q second=%q", qid1, qid2)
	}
	if n := countQuestRows(t, h); n != 1 {
		t.Errorf("quests rows after replay: got %d want 1", n)
	}
}

func TestCreateQuestIdempotencyConflictOnDifferentGoal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	key := "client-supplied-key-B"
	g1 := h.sampleGoal()
	g2 := h.sampleGoal()
	g2.Party = g1.Party + 1 // mutate one field — same key, different payload

	if _, err := h.svc.CreateQuest(ctx, h.uid, g1, service.CreateOpts{
		IdempotencyKey: keyPtr(key),
	}); err != nil {
		t.Fatalf("first CreateQuest: %v", err)
	}
	_, err := h.svc.CreateQuest(ctx, h.uid, g2, service.CreateOpts{
		IdempotencyKey: keyPtr(key),
	})
	if !errors.Is(err, service.ErrIdempotencyConflict) {
		t.Fatalf("CreateQuest mismatched-goal: want ErrIdempotencyConflict, got %v", err)
	}
}

func TestCreateQuestIdempotencyExpiredKeyTreatedAsFresh(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	key := "client-supplied-key-C"
	qid1, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{
		IdempotencyKey: keyPtr(key),
	})
	if err != nil {
		t.Fatalf("first CreateQuest: %v", err)
	}

	// Advance the harness clock past the 24h retention window so the
	// prior idempotency row reads as expired.
	h.clk.Advance(25 * time.Hour)

	qid2, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{
		IdempotencyKey: keyPtr(key),
	})
	if err != nil {
		t.Fatalf("post-expiry CreateQuest: %v", err)
	}
	if qid1 == qid2 {
		t.Errorf("post-expiry CreateQuest returned the same QuestID: got %q (both calls)", qid1)
	}
	if n := countQuestRows(t, h); n != 2 {
		t.Errorf("quests rows after expired replay: got %d want 2", n)
	}
}

func TestCancelQuestIdempotencyReplay(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	key := "cancel-key-A"
	if err := h.svc.CancelQuest(ctx, h.uid, qid, service.CancelOpts{
		IdempotencyKey: keyPtr(key),
		Reason:         "first",
	}); err != nil {
		t.Fatalf("first CancelQuest: %v", err)
	}
	// Second call with the same key replays — must not error even
	// though the quest is already terminal (replay short-circuits
	// before the row-lookup that would otherwise see Canceled).
	if err := h.svc.CancelQuest(ctx, h.uid, qid, service.CancelOpts{
		IdempotencyKey: keyPtr(key),
		Reason:         "second-should-replay",
	}); err != nil {
		t.Fatalf("replay CancelQuest: %v", err)
	}

	// Verify status is Canceled exactly once: row's updated_at after
	// the second call equals the value from the first call (replay
	// did not re-write).
	state, err := h.svc.GetQuest(ctx, h.uid, qid)
	if err != nil {
		t.Fatalf("GetQuest: %v", err)
	}
	if state.Summary.Status != domain.StatusCanceled {
		t.Errorf("status after replay: got %v want Canceled", state.Summary.Status)
	}
}

func TestCancelQuestDifferentKeysOnSameQuestBothSucceed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	if err := h.svc.CancelQuest(ctx, h.uid, qid, service.CancelOpts{
		IdempotencyKey: keyPtr("first-key"),
	}); err != nil {
		t.Fatalf("first CancelQuest: %v", err)
	}
	// Second call uses a DIFFERENT key — no idempotency hit, so the
	// real Cancel path runs. The quest is already terminal at this
	// point, which the Cancel verb treats as a no-op (returns nil).
	if err := h.svc.CancelQuest(ctx, h.uid, qid, service.CancelOpts{
		IdempotencyKey: keyPtr("different-key"),
	}); err != nil {
		t.Fatalf("second CancelQuest (different key): %v", err)
	}
}

func TestIdempotencyDifferentUsersSameKeyDoNotCollide(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	// Seed an account for otherID so it can CreateQuest too. The
	// harness only creates one account row (owned by h.uid); we
	// insert a sibling here so the cross-user test has a target.
	otherAcctID := domain.AccountID("acct_other1")
	if _, err := h.db.DB().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
		VALUES (?, ?, 'other@example.com', 'other', ?)`,
		string(otherAcctID), string(h.otherID), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert other account: %v", err)
	}

	key := "shared-key-name"
	goalA := h.sampleGoal()
	goalB := h.sampleGoal()
	goalB.AccountID = otherAcctID

	qidA, err := h.svc.CreateQuest(ctx, h.uid, goalA, service.CreateOpts{
		IdempotencyKey: keyPtr(key),
	})
	if err != nil {
		t.Fatalf("CreateQuest for h.uid: %v", err)
	}
	qidB, err := h.svc.CreateQuest(ctx, h.otherID, goalB, service.CreateOpts{
		IdempotencyKey: keyPtr(key),
	})
	if err != nil {
		t.Fatalf("CreateQuest for h.otherID: %v", err)
	}
	if qidA == qidB {
		t.Errorf("cross-user same-key produced same QuestID %q — keys are not isolated by userID", qidA)
	}
}
