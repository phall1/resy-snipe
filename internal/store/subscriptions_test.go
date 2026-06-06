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

func openTestDB(t *testing.T) (*sql.DB, domain.UserID, domain.AccountID) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "test.db"))
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

func sampleSubscriptionRow(uid domain.UserID, id string, status domain.SubscriptionStatus, created time.Time) store.SubscriptionRow {
	expires := created.Add(24 * time.Hour)
	return store.SubscriptionRow{
		ID:             domain.SubscriptionID(id),
		UserID:         uid,
		GoalJSON:       `{"venue_query":{"kind":"name","name":"Carbone"},"date":"2026-06-12","party":2,"time_prefs":{"start":"19:00:00","end":"21:00:00","priority":0},"account_id":"acct_test","constraints":{}}`,
		Status:         status,
		CreatedAt:      created,
		UpdatedAt:      created,
		ExpiresAt:      &expires,
		CompromiseJSON: `{"time_window_min":300000000000}`,
		PollInterval:   90 * time.Second,
		NextPollAt:     created,
	}
}

func TestCreateGetSubscription(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, aid := openTestDB(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	row := sampleSubscriptionRow(uid, "sub_aaaa0001", domain.SubscriptionActive, now)

	if err := store.CreateSubscription(ctx, db, row); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	got, err := store.GetSubscription(ctx, db, uid, row.ID)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if got.ID != row.ID || got.UserID != row.UserID {
		t.Errorf("row mismatch: got %+v want %+v", got, row)
	}
	if got.GoalJSON != row.GoalJSON {
		t.Errorf("goal_json: got %q want %q", got.GoalJSON, row.GoalJSON)
	}
	if got.Status != row.Status {
		t.Errorf("status: got %v want %v", got.Status, row.Status)
	}
	if !got.CreatedAt.Equal(row.CreatedAt) {
		t.Errorf("created_at: got %v want %v", got.CreatedAt, row.CreatedAt)
	}
	if !got.UpdatedAt.Equal(row.UpdatedAt) {
		t.Errorf("updated_at: got %v want %v", got.UpdatedAt, row.UpdatedAt)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(*row.ExpiresAt) {
		t.Errorf("expires_at: got %v want %v", got.ExpiresAt, row.ExpiresAt)
	}
	if got.CompromiseJSON != row.CompromiseJSON {
		t.Errorf("compromise_json: got %q want %q", got.CompromiseJSON, row.CompromiseJSON)
	}
	if got.PollInterval != row.PollInterval {
		t.Errorf("poll_interval: got %v want %v", got.PollInterval, row.PollInterval)
	}
	if !got.NextPollAt.Equal(row.NextPollAt) {
		t.Errorf("next_poll_at: got %v want %v", got.NextPollAt, row.NextPollAt)
	}

	// FulfilledBy round-trip case.
	questID := domain.QuestID("q_fulfill_rt")
	questRow := store.QuestRow{
		ID:        questID,
		UserID:    uid,
		AccountID: aid,
		GoalJSON:  `{"date":"2026-06-10"}`,
		PlanHash:  "sha256:deadbeef",
		Status:    domain.StatusSubmitted,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateQuest(ctx, db, questRow); err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	row2 := sampleSubscriptionRow(uid, "sub_aaaa0002", domain.SubscriptionFulfilled, now.Add(time.Hour))
	row2.FulfilledBy = &questID
	if err := store.CreateSubscription(ctx, db, row2); err != nil {
		t.Fatalf("CreateSubscription fulfilled: %v", err)
	}
	got2, err := store.GetSubscription(ctx, db, uid, row2.ID)
	if err != nil {
		t.Fatalf("GetSubscription fulfilled: %v", err)
	}
	if got2.FulfilledBy == nil {
		t.Fatalf("fulfilled_by: got nil, want %v", questID)
	}
	if *got2.FulfilledBy != questID {
		t.Errorf("fulfilled_by: got %v want %v", *got2.FulfilledBy, questID)
	}
}

func TestGetSubscriptionNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openTestDB(t)

	_, err := store.GetSubscription(ctx, db, uid, domain.SubscriptionID("sub_missing"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSubscription: want ErrNotFound, got %v", err)
	}
}

func TestGetSubscriptionWrongUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openTestDB(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	row := sampleSubscriptionRow(uid, "sub_wrong001", domain.SubscriptionActive, now)
	if err := store.CreateSubscription(ctx, db, row); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	_, err := store.GetSubscription(ctx, db, domain.UserID("usr_other"), row.ID)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSubscription wrong user: want ErrNotFound, got %v", err)
	}
}

func TestListSubscriptionsFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openTestDB(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	row1 := sampleSubscriptionRow(uid, "sub_active001", domain.SubscriptionActive, now)
	row2 := sampleSubscriptionRow(uid, "sub_paused001", domain.SubscriptionPaused, now.Add(time.Hour))
	// Clear optional fields for row2 to vary them.
	row2.ExpiresAt = nil
	row2.CompromiseJSON = ""

	if err := store.CreateSubscription(ctx, db, row1); err != nil {
		t.Fatalf("CreateSubscription active: %v", err)
	}
	if err := store.CreateSubscription(ctx, db, row2); err != nil {
		t.Fatalf("CreateSubscription paused: %v", err)
	}

	got, err := store.ListSubscriptions(ctx, db, uid, store.SubscriptionListFilter{
		Status: []domain.SubscriptionStatus{domain.SubscriptionActive},
	})
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListSubscriptions: got %d rows, want 1", len(got))
	}
	if got[0].ID != row1.ID {
		t.Errorf("ListSubscriptions: got %v want %v", got[0].ID, row1.ID)
	}
	if got[0].Status != domain.SubscriptionActive {
		t.Errorf("status: got %v want Active", got[0].Status)
	}
}

func TestListSubscriptionsEmptyFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openTestDB(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	row1 := sampleSubscriptionRow(uid, "sub_active002", domain.SubscriptionActive, now)
	row2 := sampleSubscriptionRow(uid, "sub_paused002", domain.SubscriptionPaused, now.Add(time.Hour))
	if err := store.CreateSubscription(ctx, db, row1); err != nil {
		t.Fatalf("CreateSubscription active: %v", err)
	}
	if err := store.CreateSubscription(ctx, db, row2); err != nil {
		t.Fatalf("CreateSubscription paused: %v", err)
	}

	got, err := store.ListSubscriptions(ctx, db, uid, store.SubscriptionListFilter{})
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListSubscriptions: got %d rows, want 2", len(got))
	}
	// Ordered by created_at DESC, so paused (later) comes first.
	if got[0].ID != row2.ID {
		t.Errorf("ListSubscriptions[0]: got %v want %v", got[0].ID, row2.ID)
	}
	if got[1].ID != row1.ID {
		t.Errorf("ListSubscriptions[1]: got %v want %v", got[1].ID, row1.ID)
	}
}

func TestListSubscriptionsMultipleStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openTestDB(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	row1 := sampleSubscriptionRow(uid, "sub_active003", domain.SubscriptionActive, now)
	row2 := sampleSubscriptionRow(uid, "sub_paused003", domain.SubscriptionPaused, now.Add(time.Hour))
	if err := store.CreateSubscription(ctx, db, row1); err != nil {
		t.Fatalf("CreateSubscription active: %v", err)
	}
	if err := store.CreateSubscription(ctx, db, row2); err != nil {
		t.Fatalf("CreateSubscription paused: %v", err)
	}

	got, err := store.ListSubscriptions(ctx, db, uid, store.SubscriptionListFilter{
		Status: []domain.SubscriptionStatus{domain.SubscriptionActive, domain.SubscriptionPaused},
	})
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListSubscriptions: got %d rows, want 2", len(got))
	}
}

func TestListSubscriptionsLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openTestDB(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	for i, id := range []string{"sub_lim001", "sub_lim002", "sub_lim003"} {
		row := sampleSubscriptionRow(uid, id, domain.SubscriptionActive, now.Add(time.Duration(i)*time.Hour))
		if err := store.CreateSubscription(ctx, db, row); err != nil {
			t.Fatalf("CreateSubscription %s: %v", id, err)
		}
	}

	got, err := store.ListSubscriptions(ctx, db, uid, store.SubscriptionListFilter{
		Limit: 2,
	})
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListSubscriptions: got %d rows, want 2", len(got))
	}
}

func TestListSubscriptionsNoRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openTestDB(t)

	got, err := store.ListSubscriptions(ctx, db, uid, store.SubscriptionListFilter{})
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListSubscriptions: got %d rows, want 0", len(got))
	}
}

func TestUpdateSubscriptionStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, aid := openTestDB(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	// Create a quest row to use as fulfilled_by.
	questRow := store.QuestRow{
		ID:        domain.QuestID("q_fulfill001"),
		UserID:    uid,
		AccountID: aid,
		GoalJSON:  `{"date":"2026-06-10"}`,
		PlanHash:  "sha256:deadbeef",
		Status:    domain.StatusSubmitted,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateQuest(ctx, db, questRow); err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	row := sampleSubscriptionRow(uid, "sub_update001", domain.SubscriptionActive, now)
	if err := store.CreateSubscription(ctx, db, row); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	later := now.Add(time.Hour)
	questID := domain.QuestID("q_fulfill001")
	if err := store.UpdateSubscriptionStatus(ctx, db, uid, row.ID, domain.SubscriptionFulfilled, &questID, later, later); err != nil {
		t.Fatalf("UpdateSubscriptionStatus: %v", err)
	}

	got, err := store.GetSubscription(ctx, db, uid, row.ID)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if got.Status != domain.SubscriptionFulfilled {
		t.Errorf("status: got %v want Fulfilled", got.Status)
	}
	if got.FulfilledBy == nil || *got.FulfilledBy != questID {
		t.Errorf("fulfilled_by: got %v want %v", got.FulfilledBy, questID)
	}
	if !got.NextPollAt.Equal(later) {
		t.Errorf("next_poll_at: got %v want %v", got.NextPollAt, later)
	}
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("updated_at: got %v want %v", got.UpdatedAt, later)
	}
}

func TestUpdateSubscriptionStatusWrongUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openTestDB(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	row := sampleSubscriptionRow(uid, "sub_wrong002", domain.SubscriptionActive, now)
	if err := store.CreateSubscription(ctx, db, row); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	err := store.UpdateSubscriptionStatus(ctx, db, domain.UserID("usr_other"), row.ID, domain.SubscriptionPaused, nil, now, now)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateSubscriptionStatus wrong user: want ErrNotFound, got %v", err)
	}
}
