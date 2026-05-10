//go:build unix

package secrets

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// mlock locks the backing pages of b into RAM so they cannot be paged
// to swap. On Linux + macOS this is unix.Mlock(2). A failure here is
// surfaced to the caller — the Sealer refuses to construct rather
// than silently degrading.
func mlock(b []byte) error {
	if err := unix.Mlock(b); err != nil {
		return fmt.Errorf("secrets: mlock %d bytes: %w", len(b), err)
	}
	return nil
}

// munlock releases the lock. Best-effort; the error is returned to
// the caller but Close() does not unwind on a Munlock failure (the
// memory was wiped already).
func munlock(b []byte) error {
	if err := unix.Munlock(b); err != nil {
		return fmt.Errorf("secrets: munlock %d bytes: %w", len(b), err)
	}
	return nil
}
