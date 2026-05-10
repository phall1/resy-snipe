package secrets

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters fixed at compile time. These are the design's
// "operator-passphrase mode" defaults from docs/v2/design/secrets.md
// §Key derivation. Tunable via daemon config in M2-04; this package
// surfaces the floor.
const (
	Argon2MemoryKiB = 64 * 1024 // 64 MiB
	Argon2Iters     = 3
	Argon2Parallel  = 1
)

// DeriveKeyFromPassphrase runs Argon2id over (passphrase, salt) and
// returns an mlocked 32-byte Key. The passphrase is read once and may
// be wiped by the caller as soon as this returns; the Key holds an
// independent copy.
//
// Pinned parameters: memory=64MiB, time=3, parallelism=1, keyLen=32.
// A parameter drift fails the Argon2id KAT in kdf_test.go so this is
// a one-line review check, not a remember-to-bump-the-constants game.
func DeriveKeyFromPassphrase(passphrase, salt []byte) (*Key, error) {
	if len(passphrase) == 0 {
		return nil, errors.New("secrets: DeriveKeyFromPassphrase: empty passphrase")
	}
	if len(salt) != SaltLen {
		return nil, fmt.Errorf("secrets: DeriveKeyFromPassphrase: salt length %d, want %d", len(salt), SaltLen)
	}
	raw := argon2.IDKey(passphrase, salt, Argon2Iters, Argon2MemoryKiB, Argon2Parallel, KeyLen)
	k, err := newKey(raw)
	// Wipe the unwrapped derived bytes the moment we have a Key copy.
	// argon2.IDKey returns a fresh slice; nothing else references it.
	for i := range raw {
		raw[i] = 0
	}
	if err != nil {
		return nil, err
	}
	return k, nil
}

// ReadKeyfile parses a hex-encoded keyfile from path and returns an
// mlocked 32-byte Key. The file must be exactly KeyLen*2 = 64 hex
// characters with an optional trailing newline. On Unix the file
// must not be world-readable (mode & 0o077 must be zero); the world-
// readable check is a soft guard against the "I cp'd it from /tmp"
// mistake — root can chmod around it.
func ReadKeyfile(path string) (*Key, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: ReadKeyfile stat: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory", ErrInvalidKeyfile, path)
	}
	if isWorldOrGroupReadable(info.Mode().Perm()) {
		return nil, fmt.Errorf("%w: %s permissions %o leak to group/other", ErrInvalidKeyfile, path, info.Mode().Perm())
	}

	f, err := os.Open(path) // #nosec G304 -- operator-supplied keyfile path is the entire point.
	if err != nil {
		return nil, fmt.Errorf("secrets: ReadKeyfile open: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Bound the read at 256 bytes so a misaimed --keyfile pointing at a
	// runaway file does not pull megabytes into RAM. 64 hex chars + LF
	// fits comfortably; anything larger is malformed.
	buf, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return nil, fmt.Errorf("secrets: ReadKeyfile read: %w", err)
	}
	hexStr := strings.TrimRight(string(buf), "\r\n")
	if len(hexStr) != KeyLen*2 {
		return nil, fmt.Errorf("%w: %s has %d hex chars, want %d", ErrInvalidKeyfile, path, len(hexStr), KeyLen*2)
	}
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidKeyfile, path, err)
	}
	k, err := newKey(raw)
	for i := range raw {
		raw[i] = 0
	}
	if err != nil {
		return nil, err
	}
	return k, nil
}

// isWorldOrGroupReadable reports whether any bit outside owner is set.
// We accept owner-only modes (0o400, 0o600, 0o700). 0o600 is the
// canonical "private file" mode and the recommendation in the
// operator runbook.
func isWorldOrGroupReadable(perm os.FileMode) bool {
	return perm&0o077 != 0
}
