package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/store"
)

// openWithOperator opens a fresh DB, runs migrations, and seeds an
// operator + a single account row owned by that operator. The
// returned (db, userID, accountID) are ready for quest CRUD tests.
func openWithOperator(t *testing.T) (*sql.DB, domain.UserID, domain.AccountID) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "quests.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	uid, err := store.SeedOperator(ctx, db, store.SeedOpts{
		Email:        "op@example.com",
		PasswordHash: []byte("argon2id$placeholder"),
	})
	if err != nil {
		t.Fatalf("SeedOperator: %v", err)
	}
	// Insert a single account row owned by uid.
	acctID := "acct_test01"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
		VALUES (?, ?, ?, 'test', ?)`,
		acctID, string(uid), "op@example.com", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return db, uid, domain.AccountID(acctID)
}

func sampleQuestRow(uid domain.UserID, aid domain.AccountID, id string, created time.Time) store.QuestRow {
	return store.QuestRow{
		ID:        domain.QuestID(id),
		UserID:    uid,
		AccountID: aid,
		GoalJSON:  `{"date":"2026-06-10"}`,
		PlanHash:  "sha256:deadbeef",
		Status:    domain.StatusSubmitted,
		CreatedAt: created,
		UpdatedAt: created,
	}
}

func TestCreateQuestRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, aid := openWithOperator(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	row := sampleQuestRow(uid, aid, "q_aaaa0001", now)

	if err := store.CreateQuest(ctx, db, row); err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	got, err := store.GetQuest(ctx, db, uid, row.ID)
	if err != nil {
		t.Fatalf("GetQuest: %v", err)
	}
	if got.ID != row.ID || got.UserID != row.UserID || got.AccountID != row.AccountID {
		t.Errorf("row mismatch: got %+v want %+v", got, row)
	}
	if got.PlanHash != row.PlanHash {
		t.Errorf("plan hash: got %q want %q", got.PlanHash, row.PlanHash)
	}
	if got.Status != domain.StatusSubmitted {
		t.Errorf("status: got %v want StatusSubmitted", got.Status)
	}
}

func TestGetQuestReturnsNotFoundForWrongUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, aid := openWithOperator(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	row := sampleQuestRow(uid, aid, "q_aaaa0002", now)
	if err := store.CreateQuest(ctx, db, row); err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	// A different user attempting to read the row must see NotFound,
	// not the row.
	_, err := store.GetQuest(ctx, db, domain.UserID("usr_other"), row.ID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetQuest wrong user: want ErrNotFound, got %v", err)
	}
}

func TestListQuestsScopedToUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, aid := openWithOperator(t)

	// Insert a second user + account so we can verify isolation.
	otherUID := domain.UserID("usr_other2")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES (?, 'b@example.com', x'', 'user', ?, ?)`,
		string(otherUID), string(uid), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert other user: %v", err)
	}
	otherAID := domain.AccountID("acct_other01")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
		VALUES (?, ?, 'b@example.com', 'other', ?)`,
		string(otherAID), string(otherUID), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert other account: %v", err)
	}

	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"q_user_001", "q_user_002"} {
		row := sampleQuestRow(uid, aid, id, now.Add(time.Duration(i)*time.Hour))
		if err := store.CreateQuest(ctx, db, row); err != nil {
			t.Fatalf("CreateQuest %s: %v", id, err)
		}
	}
	otherRow := sampleQuestRow(otherUID, otherAID, "q_other_001", now)
	if err := store.CreateQuest(ctx, db, otherRow); err != nil {
		t.Fatalf("CreateQuest other: %v", err)
	}

	got, err := store.ListQuests(ctx, db, uid, store.QuestListFilter{})
	if err != nil {
		t.Fatalf("ListQuests: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListQuests: got %d rows, want 2", len(got))
	}
	for _, r := range got {
		if r.UserID != uid {
			t.Errorf("ListQuests leaked row from %s: %+v", r.UserID, r)
		}
	}

	// And the other user sees only their row.
	otherGot, err := store.ListQuests(ctx, db, otherUID, store.QuestListFilter{})
	if err != nil {
		t.Fatalf("ListQuests other: %v", err)
	}
	if len(otherGot) != 1 || otherGot[0].ID != otherRow.ID {
		t.Errorf("ListQuests other: got %+v, want [%s]", otherGot, otherRow.ID)
	}
}

func TestUpdateQuestStatusFlipsRowAndStampsCompletedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, aid := openWithOperator(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	row := sampleQuestRow(uid, aid, "q_aaaa0003", now)
	if err := store.CreateQuest(ctx, db, row); err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	later := now.Add(time.Hour)
	if err := store.UpdateQuestStatus(ctx, db, uid, row.ID, domain.StatusCanceled, &later, later); err != nil {
		t.Fatalf("UpdateQuestStatus: %v", err)
	}
	got, err := store.GetQuest(ctx, db, uid, row.ID)
	if err != nil {
		t.Fatalf("GetQuest: %v", err)
	}
	if got.Status != domain.StatusCanceled {
		t.Errorf("status: got %v want Canceled", got.Status)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(later) {
		t.Errorf("completed_at: got %v want %v", got.CompletedAt, later)
	}
}

func TestUpdateQuestStatusWrongUserReturnsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, aid := openWithOperator(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	row := sampleQuestRow(uid, aid, "q_aaaa0004", now)
	if err := store.CreateQuest(ctx, db, row); err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	err := store.UpdateQuestStatus(ctx, db, domain.UserID("usr_other"), row.ID, domain.StatusCanceled, nil, now)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateQuestStatus wrong user: want ErrNotFound, got %v", err)
	}
}

func TestMarshalUnmarshalGoalRoundTrip(t *testing.T) {
	t.Parallel()
	g := domain.Goal{
		VenueQuery: domain.VenueQuerySlug{Slug: "carbone", City: "ny"},
		Date:       domain.NewDate(2026, time.June, 10),
		Party:      2,
		TimePrefs: domain.TimeWindow{
			Start:    domain.WallTime{Hour: 19, Minute: 0},
			End:      domain.WallTime{Hour: 21, Minute: 0},
			Priority: domain.PriorityEarlier,
		},
		AccountID: "acct_x",
		Constraints: domain.Constraints{
			TableTypes: []string{"bar", "dining"},
		},
	}
	buf, err := store.MarshalGoal(g)
	if err != nil {
		t.Fatalf("MarshalGoal: %v", err)
	}
	got, err := store.UnmarshalGoal(buf)
	if err != nil {
		t.Fatalf("UnmarshalGoal: %v", err)
	}
	if got.Date != g.Date || got.Party != g.Party || got.AccountID != g.AccountID {
		t.Errorf("goal roundtrip mismatch: got %+v want %+v", got, g)
	}
	gq, ok := got.VenueQuery.(domain.VenueQuerySlug)
	if !ok || gq.Slug != "carbone" || gq.City != "ny" {
		t.Errorf("venue_query roundtrip: got %+v", got.VenueQuery)
	}
	if got.TimePrefs.Priority != domain.PriorityEarlier {
		t.Errorf("priority roundtrip: got %v", got.TimePrefs.Priority)
	}
}
