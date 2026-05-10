package secrets

import (
	"crypto/subtle"
	"errors"
)

// KeyLen is the AEAD key length in bytes (256-bit AES-GCM).
const KeyLen = 32

// Key holds the 32-byte symmetric key in process memory. The backing
// buffer is mlocked at construction so the page never gets swapped to
// disk; Close wipes it in constant time and Munlocks the page. The
// type is opaque on purpose: callers can construct it (via
// DeriveKeyFromPassphrase or ReadKeyfile) and pass it to NewSealer,
// but cannot read the bytes out — the only consumer of the bytes is
// the AEAD cipher inside the sealer.
//
// A *Key is not safe for use after Close.
type Key struct {
	// buf is a heap-allocated 32-byte slice. We keep a slice header
	// (rather than a [32]byte) so the address stays stable for mlock;
	// Go's allocator does not compact, so the backing array's
	// underlying pointer is fixed for the slice's lifetime.
	buf []byte
}

// newKey copies src into a freshly mlocked buffer and returns the
// Key. src is left untouched; callers are responsible for wiping
// their own copy if it holds sensitive material.
func newKey(src []byte) (*Key, error) {
	if len(src) != KeyLen {
		return nil, errors.New("secrets: key must be 32 bytes")
	}
	buf := make([]byte, KeyLen)
	if err := mlock(buf); err != nil {
		// mlock failures on non-unix platforms are warned-and-noop'd;
		// a real failure here means the unix lock syscall errored,
		// which we surface so the operator knows the in-memory page
		// is swap-eligible.
		return nil, err
	}
	copy(buf, src)
	return &Key{buf: buf}, nil
}

// Close wipes the key bytes in constant time and Munlocks the
// backing page. Calling Close twice is safe.
func (k *Key) Close() error {
	if k == nil || k.buf == nil {
		return nil
	}
	subtle.ConstantTimeCopy(1, k.buf, make([]byte, KeyLen))
	err := munlock(k.buf)
	k.buf = nil
	return err
}

// bytes exposes the raw key for the AEAD cipher constructor inside
// this package. It is intentionally unexported — outside callers
// should not need the bytes. Returning the same slice (not a copy)
// keeps the lifetime tied to the *Key so a Close after construction
// invalidates anyone holding the underlying array.
func (k *Key) bytes() []byte {
	if k == nil {
		return nil
	}
	return k.buf
}
