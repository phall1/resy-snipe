//go:build !unix

package daemon

import "errors"

type fileLock struct{}

// ErrLockHeld is returned on platforms without flock support to keep
// the error contract consistent — though in practice the daemon
// refuses to start on those platforms.
var ErrLockHeld = errors.New("daemon: file locking unsupported on this platform")

func acquireFileLock(path string) (*fileLock, error) {
	return nil, errors.New("daemon: file locking unsupported on this platform")
}

func (l *fileLock) Close() error { return nil }
