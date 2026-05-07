package store

import "errors"

// ErrNotFound is returned when a Get* operation finds no matching row.
// Callers should branch on errors.Is(err, ErrNotFound) — adapters wrap
// with %w for context.
var ErrNotFound = errors.New("store: not found")

// ErrSessionExpired is returned by GetSession when a row exists but
// its exp has passed at the supplied 'now'. The engine treats this as
// equivalent to a missing session for re-login purposes.
var ErrSessionExpired = errors.New("store: session expired")
