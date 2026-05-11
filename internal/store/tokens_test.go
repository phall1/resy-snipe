package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/store"
)

// sampleTokenRow builds a TokenRow with non-zero defaults for fields
// the InsertToken validator demands. Tests override only what they care
// about.
func sampleTokenRow(uid domain.UserID, id string, hash []byte, created time.Time) store.TokenRow {
	return store.TokenRow{
		ID:        id,
		UserID:    uid,
		Hash:      hash,
		Scopes:    "user",
		Label:     "test-label",
		CreatedAt: created,
	}
}

func TestInsertTokenAndFindByHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	hash := []byte("hash_aaaa")

	if err := store.InsertToken(ctx, db, sampleTokenRow(uid, "tok_aaaa0001", hash, now)); err != nil {
		t.Fatalf("InsertToken: %v", err)
	}
	got, err := store.FindTokenByHash(ctx, db, hash)
	if err != nil {
		t.Fatalf("FindTokenByHash: %v", err)
	}
	if got.ID != "tok_aaaa0001" || got.UserID != uid || got.Scopes != "user" || got.Label != "test-label" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt: got %v, want %v", got.CreatedAt, now)
	}
	if got.LastSeen != nil || got.RevokedAt != nil {
		t.Errorf("LastSeen/RevokedAt: got %v / %v, want nil / nil", got.LastSeen, got.RevokedAt)
	}
}

func TestFindTokenByHashSkipsRevoked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	hash := []byte("hash_revoked")

	if err := store.InsertToken(ctx, db, sampleTokenRow(uid, "tok_revoke01", hash, now)); err != nil {
		t.Fatalf("InsertToken: %v", err)
	}
	if err := store.RevokeToken(ctx, db, uid, "tok_revoke01", now.Add(time.Minute)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := store.FindTokenByHash(ctx, db, hash); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoked lookup: got %v, want ErrNotFound", err)
	}
}

func TestFindTokenByHashMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _, _ := openWithOperator(t)
	if _, err := store.FindTokenByHash(ctx, db, []byte("hash_unknown")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing lookup: got %v, want ErrNotFound", err)
	}
}

func TestInsertTokenRejectsBadScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	row := sampleTokenRow(uid, "tok_badscop1", []byte("hash_bad"), now)
	row.Scopes = "wheel"
	if err := store.InsertToken(ctx, db, row); err == nil {
		t.Fatal("InsertToken: want error for invalid scope, got nil")
	}
}

func TestInsertTokenRejectsZeroCreatedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)
	row := sampleTokenRow(uid, "tok_zerot001", []byte("hash_zero"), time.Time{})
	if err := store.InsertToken(ctx, db, row); err == nil {
		t.Fatal("InsertToken: want error for zero CreatedAt (Law 7), got nil")
	}
}

func TestListTokensForUserScopedToUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)
	otherUID := domain.UserID("usr_tokother")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES (?, ?, X'00', 'user', ?, ?)`,
		string(otherUID), string(otherUID)+"@example.com", string(uid), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed other user: %v", err)
	}
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	// Two tokens for uid, one for the other.
	if err := store.InsertToken(ctx, db, sampleTokenRow(uid, "tok_mine_001", []byte("hash_m1"), now)); err != nil {
		t.Fatalf("InsertToken mine 1: %v", err)
	}
	if err := store.InsertToken(ctx, db, sampleTokenRow(uid, "tok_mine_002", []byte("hash_m2"), now.Add(time.Second))); err != nil {
		t.Fatalf("InsertToken mine 2: %v", err)
	}
	if err := store.InsertToken(ctx, db, sampleTokenRow(otherUID, "tok_other001", []byte("hash_o1"), now)); err != nil {
		t.Fatalf("InsertToken other: %v", err)
	}

	got, err := store.ListTokensForUser(ctx, db, uid)
	if err != nil {
		t.Fatalf("ListTokensForUser: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListTokensForUser: got %d rows, want 2", len(got))
	}
	if got[0].ID != "tok_mine_001" || got[1].ID != "tok_mine_002" {
		t.Errorf("ordering: got [%s, %s], want [tok_mine_001, tok_mine_002]", got[0].ID, got[1].ID)
	}
	for _, r := range got {
		if r.UserID != uid {
			t.Errorf("cross-tenant leak: row %s has user_id %s", r.ID, r.UserID)
		}
	}
}

func TestListTokensForUserIncludesRevoked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	if err := store.InsertToken(ctx, db, sampleTokenRow(uid, "tok_live_001", []byte("hash_live"), now)); err != nil {
		t.Fatalf("InsertToken live: %v", err)
	}
	if err := store.InsertToken(ctx, db, sampleTokenRow(uid, "tok_dead_001", []byte("hash_dead"), now.Add(time.Second))); err != nil {
		t.Fatalf("InsertToken dead: %v", err)
	}
	if err := store.RevokeToken(ctx, db, uid, "tok_dead_001", now.Add(time.Minute)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	got, err := store.ListTokensForUser(ctx, db, uid)
	if err != nil {
		t.Fatalf("ListTokensForUser: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (revoked still listed for audit)", len(got))
	}
	var sawRevoked bool
	for _, r := range got {
		if r.ID == "tok_dead_001" {
			if r.RevokedAt == nil {
				t.Errorf("tok_dead_001: RevokedAt is nil, want set")
			}
			sawRevoked = true
		}
	}
	if !sawRevoked {
		t.Error("revoked token missing from list")
	}
}

func TestRevokeTokenCrossTenantFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)
	otherUID := domain.UserID("usr_revx_other")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES (?, 'revx@example.com', X'00', 'user', ?, ?)`,
		string(otherUID), string(uid), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed other user: %v", err)
	}
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	if err := store.InsertToken(ctx, db, sampleTokenRow(otherUID, "tok_other001", []byte("hash_rx"), now)); err != nil {
		t.Fatalf("InsertToken other: %v", err)
	}
	if err := store.RevokeToken(ctx, db, uid, "tok_other001", now.Add(time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cross-tenant revoke: got %v, want ErrNotFound", err)
	}
	// Verify still live.
	got, err := store.FindTokenByHash(ctx, db, []byte("hash_rx"))
	if err != nil {
		t.Fatalf("FindTokenByHash after failed cross-tenant revoke: %v", err)
	}
	if got.RevokedAt != nil {
		t.Errorf("cross-tenant revoke unexpectedly succeeded: %+v", got)
	}
}

func TestRevokeTokenIdempotentSecondCallNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	if err := store.InsertToken(ctx, db, sampleTokenRow(uid, "tok_dupr_001", []byte("hash_dr"), now)); err != nil {
		t.Fatalf("InsertToken: %v", err)
	}
	if err := store.RevokeToken(ctx, db, uid, "tok_dupr_001", now.Add(time.Minute)); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	// Second revoke surfaces ErrNotFound — the design has no separate
	// "already revoked" sentinel and a re-revoke of a dead credential
	// is a no-op from the caller's perspective.
	if err := store.RevokeToken(ctx, db, uid, "tok_dupr_001", now.Add(2*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second revoke: got %v, want ErrNotFound", err)
	}
}

func TestTouchTokenSeenStampsLastSeen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	hash := []byte("hash_touch")
	if err := store.InsertToken(ctx, db, sampleTokenRow(uid, "tok_touch001", hash, now)); err != nil {
		t.Fatalf("InsertToken: %v", err)
	}
	touch := now.Add(5 * time.Minute)
	if err := store.TouchTokenSeen(ctx, db, hash, touch); err != nil {
		t.Fatalf("TouchTokenSeen: %v", err)
	}
	got, err := store.FindTokenByHash(ctx, db, hash)
	if err != nil {
		t.Fatalf("FindTokenByHash: %v", err)
	}
	if got.LastSeen == nil {
		t.Fatal("LastSeen: got nil, want set")
	}
	if !got.LastSeen.Equal(touch) {
		t.Errorf("LastSeen: got %v, want %v", got.LastSeen, touch)
	}
}

func TestTokenUserCascadeOnDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, uid, _ := openWithOperator(t)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	if err := store.InsertToken(ctx, db, sampleTokenRow(uid, "tok_casc_001", []byte("hash_c"), now)); err != nil {
		t.Fatalf("InsertToken: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, string(uid)); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := store.FindTokenByHash(ctx, db, []byte("hash_c")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("post-user-delete lookup: got %v, want ErrNotFound (cascade)", err)
	}
}
