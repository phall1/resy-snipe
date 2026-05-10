package secrets

import (
	"bytes"
	"testing"
)

// White-box test in the secrets package proper so it can read the
// unexported buf via the unexported bytes() helper.

func TestKeyCloseWipesBuffer(t *testing.T) {
	t.Parallel()
	src := bytes.Repeat([]byte{0xAA}, KeyLen)
	k, err := newKey(src)
	if err != nil {
		t.Fatalf("newKey: %v", err)
	}
	if !bytes.Equal(k.bytes(), src) {
		t.Fatalf("key.bytes()=%x want %x", k.bytes(), src)
	}
	// Hold the slice header so we can confirm the bytes are zeroed
	// before Close drops the reference.
	held := k.buf
	if err := k.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if k.bytes() != nil {
		t.Fatalf("bytes() not nil after Close: %x", k.bytes())
	}
	if !isZero(held) {
		t.Fatalf("key buffer not zeroed after Close: %x", held)
	}
	// Double close must be a no-op.
	if err := k.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestNewKeyRejectsWrongLength(t *testing.T) {
	t.Parallel()
	if _, err := newKey(make([]byte, 16)); err == nil {
		t.Fatal("expected error on 16-byte key")
	}
	if _, err := newKey(make([]byte, 64)); err == nil {
		t.Fatal("expected error on 64-byte key")
	}
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
