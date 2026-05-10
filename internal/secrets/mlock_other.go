//go:build !unix

package secrets

import (
	"log/slog"
	"sync"
)

// mlock is a no-op on platforms that do not expose an mlock-equivalent
// in golang.org/x/sys. The deliberate non-fatal exists so developers
// on Windows-WSL can run the daemon under tests; the daemon emits a
// structured warning the first time the path fires so the operator
// can audit it.
//
// design/secrets.md §Key lifecycle in process explicitly designates
// this as a deliberate non-fatal.
func mlock(b []byte) error {
	warnOnce()
	return nil
}

func munlock(b []byte) error {
	return nil
}

var warnOnce = sync.OnceFunc(func() {
	slog.Warn("secrets: mlock unavailable on this platform; key pages may be swapped to disk",
		"event", "secrets.mlock_unavailable",
	)
})
