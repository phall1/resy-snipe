//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// fileLock holds a non-blocking exclusive flock on a lockfile next
// to data.db so two daemons cannot share the same SQLite WAL. The
// lock is released by Close (called from Daemon.Close in shutdown
// order).
type fileLock struct {
	file *os.File
}

// ErrLockHeld is returned by acquireFileLock when another process
// already holds the lock. The daemon's main maps this to exit code 2
// per design/daemon.md §2.2.
var ErrLockHeld = errors.New("daemon: data.db lock is already held")

func acquireFileLock(path string) (*fileLock, error) {
	// O_CREATE + 0o600: lockfile lives next to data.db, owner-only
	// since it's intentionally per-deployment metadata.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- lock path comes from operator config.
	if err != nil {
		return nil, fmt.Errorf("daemon: open lockfile %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLockHeld, path)
		}
		return nil, fmt.Errorf("daemon: flock %s: %w", path, err)
	}
	return &fileLock{file: f}, nil
}

func (l *fileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}
