// Package secrets owns the AES-GCM seal / unseal of recoverable
// credentials (Resy passwords, JWTs) at rest. It is the M2 answer to
// ADR-0008: a stolen data.db is useless without the operator's unwrap
// key.
//
// The package depends on internal/store's schema (tables secrets +
// secrets_meta from migration 0002_v2_multi_user.sql) but not on any
// concrete *store.SQLiteStore — it talks to *sql.DB directly so it can
// be reused from the migration tool and the live daemon alike. It also
// depends on internal/domain for the UserID / AccountID typed strings
// and on internal/clock so created_at is testable without touching the
// stdlib wall-clock directly (Law 7).
//
// The sealer is constructed once at daemon boot. Two key sources are
// supported, settled at boot by daemon.Config and immutable for the
// life of the process: an operator passphrase (Argon2id over the
// per-deployment salt in secrets_meta) or a hex-encoded keyfile. The
// derived key is mlocked, never logged, never marshaled, and wiped by
// Sealer.Close on shutdown.
//
// See docs/v2/design/secrets.md and ADR-0008 for the threat model and
// the operator runbook.
package secrets
