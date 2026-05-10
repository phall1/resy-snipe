package secrets_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/secrets"
)

func newTestSealer(t *testing.T) *secrets.Sealer {
	t.Helper()
	db := openTestDB(t)
	c := clock.NewFake(time.Unix(1_700_000_000, 0))

	meta, err := secrets.LoadOrInitMeta(context.Background(), db, c)
	if err != nil {
		t.Fatalf("LoadOrInitMeta: %v", err)
	}
	k, err := secrets.DeriveKeyFromPassphrase([]byte("correct horse battery staple"), meta.KDFSalt)
	if err != nil {
		t.Fatalf("DeriveKeyFromPassphrase: %v", err)
	}
	s, err := secrets.NewSealer(db, k, meta, c)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSealerRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)
	plaintext := []byte("hunter2")
	ctx := context.Background()
	if err := s.Seal(ctx, testUserID, testAccountID, secrets.KindResyPassword, plaintext); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := s.Open(ctx, testUserID, testAccountID, secrets.KindResyPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Open=%q want %q", got, plaintext)
	}
}

func TestSealerReSealReplacesRow(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)
	ctx := context.Background()
	if err := s.Seal(ctx, testUserID, testAccountID, secrets.KindResyPassword, []byte("v1")); err != nil {
		t.Fatalf("first Seal: %v", err)
	}
	if err := s.Seal(ctx, testUserID, testAccountID, secrets.KindResyPassword, []byte("v2")); err != nil {
		t.Fatalf("second Seal: %v", err)
	}
	got, err := s.Open(ctx, testUserID, testAccountID, secrets.KindResyPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(got) != "v2" {
		t.Fatalf("Open=%q want %q", got, "v2")
	}
}

func TestSealerOpenMissingReturnsNotFound(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)
	_, err := s.Open(context.Background(), testUserID, testAccountID, secrets.KindResyPassword)
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Open missing: err=%v want ErrNotFound", err)
	}
}

func TestSealerTamperedCiphertext(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	meta, err := secrets.LoadOrInitMeta(context.Background(), db, c)
	if err != nil {
		t.Fatalf("LoadOrInitMeta: %v", err)
	}
	k, err := secrets.DeriveKeyFromPassphrase([]byte("hunter2"), meta.KDFSalt)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	s, err := secrets.NewSealer(db, k, meta, c)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Seal(context.Background(), testUserID, testAccountID, secrets.KindResyPassword, []byte("plain")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE secrets SET ciphertext = X'00' || substr(ciphertext, 2)
		WHERE user_id = ? AND account_id = ? AND kind = ?`,
		testUserID, testAccountID, secrets.KindResyPassword); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if _, err := s.Open(context.Background(), testUserID, testAccountID, secrets.KindResyPassword); !errors.Is(err, secrets.ErrTampered) {
		t.Fatalf("Open: err=%v want ErrTampered", err)
	}
}

func TestSealerTamperedAssociatedData(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	meta, err := secrets.LoadOrInitMeta(context.Background(), db, c)
	if err != nil {
		t.Fatalf("LoadOrInitMeta: %v", err)
	}
	k, err := secrets.DeriveKeyFromPassphrase([]byte("hunter2"), meta.KDFSalt)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	s, err := secrets.NewSealer(db, k, meta, c)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Seal(context.Background(), testUserID, testAccountID, secrets.KindResyPassword, []byte("plain")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Rewriting kind on disk without re-encrypting breaks the AD
	// binding. The AEAD auth tag fails because Open recomputes AD
	// from the row's (user_id, account_id, kind), and kind no longer
	// matches what was bound at Seal time.
	if _, err := db.ExecContext(context.Background(), `UPDATE secrets SET kind = ?
		WHERE user_id = ? AND account_id = ? AND kind = ?`,
		secrets.KindResySessionJWT, testUserID, testAccountID, secrets.KindResyPassword); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if _, err := s.Open(context.Background(), testUserID, testAccountID, secrets.KindResySessionJWT); !errors.Is(err, secrets.ErrTampered) {
		t.Fatalf("Open after AD swap: err=%v want ErrTampered", err)
	}
}

func TestSealerWrongKey(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	meta, err := secrets.LoadOrInitMeta(context.Background(), db, c)
	if err != nil {
		t.Fatalf("LoadOrInitMeta: %v", err)
	}

	k1, err := secrets.DeriveKeyFromPassphrase([]byte("passphrase-A"), meta.KDFSalt)
	if err != nil {
		t.Fatalf("derive A: %v", err)
	}
	s1, err := secrets.NewSealer(db, k1, meta, c)
	if err != nil {
		t.Fatalf("NewSealer A: %v", err)
	}
	if err := s1.Seal(context.Background(), testUserID, testAccountID, secrets.KindResyPassword, []byte("secret")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	_ = s1.Close()

	k2, err := secrets.DeriveKeyFromPassphrase([]byte("passphrase-B"), meta.KDFSalt)
	if err != nil {
		t.Fatalf("derive B: %v", err)
	}
	s2, err := secrets.NewSealer(db, k2, meta, c)
	if err != nil {
		t.Fatalf("NewSealer B: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	if _, err := s2.Open(context.Background(), testUserID, testAccountID, secrets.KindResyPassword); !errors.Is(err, secrets.ErrTampered) {
		// Public surface returns ErrTampered for either tampering or
		// wrong-key; that conflation is documented in errors.go.
		t.Fatalf("Open with wrong key: err=%v want ErrTampered", err)
	}
}

func TestSealerListAndDelete(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)
	ctx := context.Background()

	if err := s.Seal(ctx, testUserID, testAccountID, secrets.KindResyPassword, []byte("pw")); err != nil {
		t.Fatalf("Seal pw: %v", err)
	}
	if err := s.Seal(ctx, testUserID, testAccountID, secrets.KindResySessionJWT, []byte("jwt")); err != nil {
		t.Fatalf("Seal jwt: %v", err)
	}

	kinds, err := s.List(ctx, testUserID, testAccountID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	slices.Sort(kinds)
	wantKinds := []secrets.Kind{secrets.KindResyPassword, secrets.KindResySessionJWT}
	slices.Sort(wantKinds)
	if !slices.Equal(kinds, wantKinds) {
		t.Fatalf("List=%v want %v", kinds, wantKinds)
	}

	if err := s.Delete(ctx, testUserID, testAccountID, secrets.KindResyPassword); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, testUserID, testAccountID, secrets.KindResyPassword); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("second Delete: err=%v want ErrNotFound", err)
	}
	if _, err := s.Open(ctx, testUserID, testAccountID, secrets.KindResyPassword); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("Open after Delete: err=%v want ErrNotFound", err)
	}
}

func TestSealerRejectsUnknownKind(t *testing.T) {
	t.Parallel()
	s := newTestSealer(t)
	if err := s.Seal(context.Background(), testUserID, testAccountID, secrets.Kind("bogus"), []byte{}); err == nil {
		t.Fatal("Seal accepted unknown kind")
	}
}

func TestSealerCascadesOnUserDelete(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	meta, _ := secrets.LoadOrInitMeta(context.Background(), db, c)
	k, _ := secrets.DeriveKeyFromPassphrase([]byte("hunter2"), meta.KDFSalt)
	s, err := secrets.NewSealer(db, k, meta, c)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Seal(context.Background(), testUserID, testAccountID, secrets.KindResyPassword, []byte("x")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `DELETE FROM users WHERE id = ?`, testUserID); err != nil {
		t.Fatalf("DELETE users: %v", err)
	}
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM secrets WHERE user_id = ?`, testUserID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("secrets row count after DELETE users = %d, want 0 (FK CASCADE)", n)
	}
}
