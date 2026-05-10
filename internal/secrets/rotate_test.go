package secrets_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/secrets"
)

func TestRotateReencryptsAllRows(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	meta, err := secrets.LoadOrInitMeta(context.Background(), db, c)
	if err != nil {
		t.Fatalf("LoadOrInitMeta: %v", err)
	}
	oldKey, err := secrets.DeriveKeyFromPassphrase([]byte("old-pass"), meta.KDFSalt)
	if err != nil {
		t.Fatalf("derive old: %v", err)
	}
	s, err := secrets.NewSealer(db, oldKey, meta, c)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.Seal(ctx, testUserID, testAccountID, secrets.KindResyPassword, []byte("pw-plain")); err != nil {
		t.Fatalf("Seal pw: %v", err)
	}
	if err := s.Seal(ctx, testUserID, testAccountID, secrets.KindResySessionJWT, []byte("jwt-plain")); err != nil {
		t.Fatalf("Seal jwt: %v", err)
	}

	// Capture pre-rotate ciphertexts so we can confirm the rows actually
	// changed on disk (defense against a "rotate is a no-op" regression).
	preCT := readCiphertexts(t, db)

	newKey, err := secrets.DeriveKeyFromPassphrase([]byte("new-pass"), meta.KDFSalt)
	if err != nil {
		t.Fatalf("derive new: %v", err)
	}
	if err := s.Rotate(ctx, newKey); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	postCT := readCiphertexts(t, db)
	for kind, ct := range preCT {
		if bytes.Equal(ct, postCT[kind]) {
			t.Fatalf("kind %s ciphertext unchanged after Rotate", kind)
		}
	}

	if got, err := s.Open(ctx, testUserID, testAccountID, secrets.KindResyPassword); err != nil {
		t.Fatalf("Open pw after rotate: %v", err)
	} else if string(got) != "pw-plain" {
		t.Fatalf("Open pw=%q want %q", got, "pw-plain")
	}
	if got, err := s.Open(ctx, testUserID, testAccountID, secrets.KindResySessionJWT); err != nil {
		t.Fatalf("Open jwt after rotate: %v", err)
	} else if string(got) != "jwt-plain" {
		t.Fatalf("Open jwt=%q want %q", got, "jwt-plain")
	}

	var v int
	if err := db.QueryRowContext(ctx, `SELECT active_version FROM secrets_meta WHERE id = 1`).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != meta.ActiveVersion+1 {
		t.Fatalf("active_version=%d, want %d", v, meta.ActiveVersion+1)
	}
	if err := secrets.CheckMixedVersion(ctx, db); err != nil {
		t.Fatalf("CheckMixedVersion after Rotate: %v", err)
	}
}

func TestCheckMixedVersionDetectsStaleRow(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	meta, err := secrets.LoadOrInitMeta(context.Background(), db, c)
	if err != nil {
		t.Fatalf("LoadOrInitMeta: %v", err)
	}
	k, _ := secrets.DeriveKeyFromPassphrase([]byte("p"), meta.KDFSalt)
	s, err := secrets.NewSealer(db, k, meta, c)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Seal(context.Background(), testUserID, testAccountID, secrets.KindResyPassword, []byte("x")); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Simulate an operator manually editing the row to a stale version.
	if _, err := db.ExecContext(context.Background(),
		`UPDATE secrets SET version = 99 WHERE user_id = ? AND account_id = ?`,
		testUserID, testAccountID); err != nil {
		t.Fatalf("UPDATE version: %v", err)
	}
	err = secrets.CheckMixedVersion(context.Background(), db)
	if !errors.Is(err, secrets.ErrMixedVersion) {
		t.Fatalf("CheckMixedVersion: err=%v want ErrMixedVersion", err)
	}
	if !strings.Contains(err.Error(), testUserID) {
		t.Fatalf("error did not name stale row: %v", err)
	}
}

func TestCheckMixedVersionAllAligned(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	if _, err := secrets.LoadOrInitMeta(context.Background(), db, c); err != nil {
		t.Fatalf("LoadOrInitMeta: %v", err)
	}
	// Empty `secrets` table is also a valid aligned state.
	if err := secrets.CheckMixedVersion(context.Background(), db); err != nil {
		t.Fatalf("CheckMixedVersion empty: %v", err)
	}
}

func TestRotateRejectsBadKey(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	c := clock.NewFake(time.Unix(1_700_000_000, 0))
	meta, _ := secrets.LoadOrInitMeta(context.Background(), db, c)
	k, _ := secrets.DeriveKeyFromPassphrase([]byte("p"), meta.KDFSalt)
	s, err := secrets.NewSealer(db, k, meta, c)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Rotate(context.Background(), nil); err == nil {
		t.Fatal("Rotate accepted nil key")
	}
}

func readCiphertexts(t *testing.T, db *sql.DB) map[string][]byte {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT kind, ciphertext FROM secrets`)
	if err != nil {
		t.Fatalf("query ciphertexts: %v", err)
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var kind string
		var ct []byte
		if err := rows.Scan(&kind, &ct); err != nil {
			t.Fatalf("scan ciphertext: %v", err)
		}
		out[kind] = ct
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ciphertexts: %v", err)
	}
	return out
}
