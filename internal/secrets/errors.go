package secrets

import "errors"

// ErrNotFound is returned by Open / Delete when the (userID, accountID,
// kind) triple has no row in the secrets table.
var ErrNotFound = errors.New("secrets: not found")

// ErrTampered is returned by Open when the AEAD auth tag fails to
// verify against the stored ciphertext + nonce + associated-data
// triple. At the public API surface ErrTampered and ErrWrongKey are
// indistinguishable; the daemon distinguishes them via secrets_meta
// for operator messaging.
var ErrTampered = errors.New("secrets: AEAD verification failed")

// ErrWrongKey is returned by Open when the Sealer's current key does
// not match the key the row was sealed under. In practice this only
// fires across a key rotation — within a single boot, a verification
// failure surfaces as ErrTampered.
var ErrWrongKey = errors.New("secrets: wrong key")

// ErrInvalidKeyfile is returned by ReadKeyfile when the file is the
// wrong length, contains non-hex characters, or has world-readable
// permissions on a Unix host.
var ErrInvalidKeyfile = errors.New("secrets: invalid keyfile")

// ErrMixedVersion is returned at boot when at least one secrets row
// has a version != secrets_meta.active_version, which would indicate a
// botched rotation or a manual sqlite3 edit. The error wraps the list
// of stale row identifiers for the operator runbook.
var ErrMixedVersion = errors.New("secrets: mixed-version rows")
