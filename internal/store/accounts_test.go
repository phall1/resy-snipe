package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/store"
)

func TestListAccountsForUserScopedToUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)

	// Insert a second user and an account owned by them.
	otherUID := domain.UserID("usr_acctother")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES (?, 'c@example.com', x'', 'user', ?, ?)`,
		string(otherUID), string(uid), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert other user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
		VALUES ('acct_otherx', ?, 'c@example.com', 'other', ?)`,
		string(otherUID), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert other account: %v", err)
	}

	rows, err := store.ListAccountsForUser(ctx, db, uid)
	if err != nil {
		t.Fatalf("ListAccountsForUser: %v", err)
	}
	for _, r := range rows {
		if r.UserID != uid {
			t.Errorf("ListAccountsForUser leaked row from %s: %+v", r.UserID, r)
		}
	}

	otherRows, err := store.ListAccountsForUser(ctx, db, otherUID)
	if err != nil {
		t.Fatalf("ListAccountsForUser other: %v", err)
	}
	if len(otherRows) != 1 || otherRows[0].ID != "acct_otherx" {
		t.Errorf("other rows: got %+v", otherRows)
	}
}

func TestGetAccountByEmailRespectsUserID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)

	// openWithOperator inserts an account owned by uid with email
	// op@example.com. A cross-user lookup must return NotFound.
	got, err := store.GetAccountByEmail(ctx, db, uid, "op@example.com")
	if err != nil {
		t.Fatalf("GetAccountByEmail (correct user): %v", err)
	}
	if got.ResyEmail != "op@example.com" {
		t.Errorf("email: got %q", got.ResyEmail)
	}

	_, err = store.GetAccountByEmail(ctx, db, domain.UserID("usr_other"), "op@example.com")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("wrong user: want ErrNotFound, got %v", err)
	}
}

func TestBindLegacyAccountToUserClaimsNullRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)

	// Insert a legacy NULL-user_id account row (the shape v1
	// UpsertSession would produce).
	legacyID := "acct_legacy_a"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
		VALUES (?, NULL, 'legacy@example.com', 'legacy-v1', ?)`,
		legacyID, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed legacy account: %v", err)
	}

	got, err := store.BindLegacyAccountToUser(ctx, db, uid, "legacy@example.com")
	if err != nil {
		t.Fatalf("BindLegacyAccountToUser: %v", err)
	}
	if string(got) != legacyID {
		t.Errorf("account id: got %q want %q", got, legacyID)
	}

	// And the row is now bound.
	row, err := store.GetAccountByEmail(ctx, db, uid, "legacy@example.com")
	if err != nil {
		t.Fatalf("GetAccountByEmail after bind: %v", err)
	}
	if row.UserID != uid {
		t.Errorf("bound user: got %q want %q", row.UserID, uid)
	}
}

func TestBindLegacyAccountToUserIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)

	// openWithOperator already inserted an account bound to uid with
	// email op@example.com. A re-bind should return that same id, not
	// fork a second row.
	id1, err := store.BindLegacyAccountToUser(ctx, db, uid, "op@example.com")
	if err != nil {
		t.Fatalf("BindLegacyAccountToUser: %v", err)
	}
	id2, err := store.BindLegacyAccountToUser(ctx, db, uid, "op@example.com")
	if err != nil {
		t.Fatalf("BindLegacyAccountToUser second: %v", err)
	}
	if id1 != id2 {
		t.Errorf("idempotent bind: got %q then %q", id1, id2)
	}

	rows, err := store.ListAccountsForUser(ctx, db, uid)
	if err != nil {
		t.Fatalf("ListAccountsForUser: %v", err)
	}
	count := 0
	for _, r := range rows {
		if r.ResyEmail == "op@example.com" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one op@example.com row, got %d", count)
	}
}

func TestBindLegacyAccountToUserReturnsNotFoundWhenNoLegacyRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)

	_, err := store.BindLegacyAccountToUser(ctx, db, uid, "missing@example.com")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no legacy row: want ErrNotFound, got %v", err)
	}
}
