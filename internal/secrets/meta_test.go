package secrets_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/secrets"
)

func TestLoadOrInitMetaFirstCallCreatesRow(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	c := clock.NewFake(time.Unix(1_700_000_000, 0))

	m, err := secrets.LoadOrInitMeta(context.Background(), db, c)
	if err != nil {
		t.Fatalf("LoadOrInitMeta: %v", err)
	}
	if got, want := len(m.KDFSalt), secrets.SaltLen; got != want {
		t.Fatalf("salt length=%d want %d", got, want)
	}
	if m.ActiveVersion != 1 {
		t.Fatalf("active_version=%d want 1", m.ActiveVersion)
	}
	if isAllZero(m.KDFSalt) {
		t.Fatalf("salt is all zero; randomness failed")
	}
}

func TestLoadOrInitMetaIsIdempotent(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	c := clock.NewFake(time.Unix(1_700_000_000, 0))

	first, err := secrets.LoadOrInitMeta(context.Background(), db, c)
	if err != nil {
		t.Fatalf("first LoadOrInitMeta: %v", err)
	}
	second, err := secrets.LoadOrInitMeta(context.Background(), db, c)
	if err != nil {
		t.Fatalf("second LoadOrInitMeta: %v", err)
	}
	if !bytes.Equal(first.KDFSalt, second.KDFSalt) {
		t.Fatalf("salt changed across calls; first=%x second=%x", first.KDFSalt, second.KDFSalt)
	}
	if first.ActiveVersion != second.ActiveVersion {
		t.Fatalf("active_version changed: %d -> %d", first.ActiveVersion, second.ActiveVersion)
	}
}

func TestLoadOrInitMetaRejectsNil(t *testing.T) {
	t.Parallel()
	if _, err := secrets.LoadOrInitMeta(context.Background(), nil, clock.Real{}); err == nil {
		t.Fatal("expected error on nil db")
	}
	db := openTestDB(t)
	if _, err := secrets.LoadOrInitMeta(context.Background(), db, nil); err == nil {
		t.Fatal("expected error on nil clock")
	}
}

func isAllZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
