# Secrets

**Layer**: `internal/secrets` (new in M2)
**Status**: Design — implementation lands in M2
**Related ADRs**: [0008](../adr/0008-secrets-sealed-at-rest-operator-key.md),
[0005](../adr/0005-multi-user-data-model-from-day-one.md),
[0006](../adr/0006-sqlite-only-no-external-deps.md),
[0010](../adr/0010-one-daemon-many-users.md)
**Related design**: [overview.md](overview.md),
[multi-user.md](multi-user.md),
[service-layer.md](service-layer.md),
[daemon.md](daemon.md)

## Purpose

Resy passwords and JWT session blobs are recoverable secrets — anyone
holding them can sign in as the user. v1 stored both in plaintext
inside `data.db`. That was acceptable for a single-shot CLI run on
the operator's laptop; it is not acceptable for a long-lived daemon
that holds state for multiple users on a homelab box.

The secrets layer's single job: **a stolen `data.db` is useless
without the operator's unwrap key**. The unwrap key is never co-
located with `data.db` — that property is enforced by operator
runbook, not by code, but the daemon refuses configurations that
weaken it (see [§Dev-mode](#dev-mode)).

What this protects:

- Resy passwords (used by `resy.Client.Login` to refresh JWTs).
- Resy session JWTs (used as the `Authorization` header).
- Future kinds: signer state, OpenTable credentials, push tokens.
  Each new kind is an entry in the `Kind` enum (see §Kinds) and an
  ADR.

What this explicitly does **not** protect: anything in
`store.snipes`, `store.events`, `store.audit_events`. Quest history
is not a secret — it's an audit trail. Sealing it would break the
operator's ability to debug a stuck quest with `sqlite3 data.db`.
See [ADR-0008 §Scope](../adr/0008-secrets-sealed-at-rest-operator-key.md).

## Sealing primitive

AES-GCM with a 256-bit key. The standard envelope:

- **Key**: 32 bytes derived per [§Key derivation](#key-derivation).
- **Nonce**: 96 bits from `crypto/rand.Read`. Per-secret, never
  reused with the same key. Stored alongside the ciphertext in
  `secrets.nonce`.
- **Auth tag**: 128 bits, included in the ciphertext blob (Go's
  `crypto/cipher.AEAD.Seal` appends it; `Open` verifies it).
- **Associated data**: `user_id || account_id || kind` as UTF-8
  bytes, separator `0x1f`. Binds a sealed row to its identity —
  swapping a row's `(user_id, account_id, kind)` columns and re-
  encoding the BLOB still fails the AEAD check.

Plaintext byte-shape is defined per kind, never inferred:

| Kind                | Plaintext bytes                                  |
|---------------------|--------------------------------------------------|
| `resy_password`     | UTF-8 of the user-typed password, no trim.       |
| `resy_session_jwt`  | Canonical JSON of `domain.SessionRow.JWT` field. |

Canonical JSON means `encoding/json` with default field order; the
same struct value round-trips byte-identically. We do not invent a
custom canonicalisation — `domain.SessionRow` has no maps, so
default Go encoding is already deterministic.

Why AES-GCM rather than NaCl secretbox or XChaCha20-Poly1305: AES-
GCM is in `crypto/cipher` and `crypto/aes` in the standard library.
[ADR-0006](../adr/0006-sqlite-only-no-external-deps.md) — no
external dependencies — applies to the secrets layer too.

## Key derivation

Two operator-facing sources. Exactly one is active per daemon
process. The choice is wired at boot from `daemon.Config` and never
changes for a running daemon.

### Source A — Operator passphrase

The most common deployment. The daemon derives the AES key from a
passphrase the operator types (or systemd injects).

- **Stdin prompt**: `resy-snipe serve` blocks on stdin read at
  startup if no env var is set. Prompt is written to stderr with
  echo disabled (`golang.org/x/term.ReadPassword`). The daemon
  refuses to start if stdin is not a TTY and no env var is set
  (see [§Dev-mode](#dev-mode) for the explicit override).
- **Env var**: `RESY_SNIPE_PASSPHRASE`. Read once at boot, then
  zeroed in memory immediately after derivation. Intended for
  systemd `LoadCredential=` and Docker `secrets:` — both deliver
  the value out-of-band and unset it before child processes start.
  Never set this from a shell rc file; the daemon logs a CRITICAL
  warning if its parent process is an interactive shell.
- **KDF**: Argon2id, parameters fixed at compile time:
    - `memory = 64 MiB` (`64 * 1024` KiB)
    - `iterations (t) = 3`
    - `parallelism (p) = 1`
    - `keyLen = 32` (256 bits)
    - `salt = 32 bytes` from `secrets_meta.kdf_salt`
- **Salt**: generated once at first boot via `crypto/rand`, written
  to `secrets_meta.kdf_salt`, and never changed except by
  `secrets rotate`. The salt is not secret — it is in
  `data.db` — but it is per-deployment, so a rainbow table built
  against one operator's box is useless against another's.

### Source B — Keyfile

For deployments that prefer a file mount over a typed passphrase
(typical on systems where systemd credentials aren't available).

- **`--keyfile <path>`** flag on `resy-snipe serve`. The file
  contains a 32-byte raw key, **hex-encoded** (64 ASCII chars +
  optional trailing newline). Hex is deliberate: copy-paste
  through a terminal preserves it; binary files do not.
- The daemon reads the file once, validates length and charset,
  decodes to 32 bytes, and never copies the bytes elsewhere. The
  file descriptor is closed before the engine boots.
- Operator places this file on a separate volume / age-encrypted
  archive / 1Password CLI fetch / `pass show`. Out-of-scope for
  the daemon — `--keyfile` accepts a path and reads it. Where the
  path comes from is the operator's call.
- **Refusal**: file is missing, file is wrong length, file is
  world-readable on a Unix host (`mode & 0o077 != 0`). The world-
  readable check is a soft guard — root can chmod around it — but
  it catches the common "I `cp`'d it from `/tmp`" mistake.

The two sources produce indistinguishable 32-byte keys. The
`Sealer` cannot tell which source the key came from; nor can the
ciphertext on disk.

## Key lifecycle in process

The derived key is the most sensitive byte string in the daemon.
Treat it like a goroutine-local crown jewel.

- **In memory only.** Never marshalled, never written to disk
  outside `secrets_meta` (which only holds the *salt*, not the key),
  never logged. The `*Sealer` value holds the key in an unexported
  `[32]byte` field; callers see only `Seal` / `Open` methods.
- **`mlock`'d.** On Linux and macOS, the daemon calls
  `golang.org/x/sys/unix.Mlock` on the key's backing buffer at
  derivation time to prevent the page from being swapped to disk.
  On platforms without `Mlock` (Windows is the only candidate the
  build matrix ever sees), the daemon logs a structured warning
  with `event="secrets.mlock_unavailable"` and continues; this is
  a deliberate non-fatal so developers on Windows-WSL can still
  run the daemon.
- **Wiped on shutdown.** The `Sealer.Close()` method runs a
  `crypto/subtle.ConstantTimeCopy(1, dst, zeros)` over the 32-byte
  buffer, then `Munlock`'s, then drops the reference. The daemon's
  shutdown sequence calls `Close` after the engine has drained
  in-flight quests but before the SQLite connection closes. See
  [design/daemon.md](daemon.md) §Shutdown order.
- **Re-derived on rotate.** After `secrets rotate` succeeds, the
  daemon either re-prompts (Source A) or re-reads the keyfile
  (Source B) and rebuilds the `Sealer`. The old key is wiped in
  the same step.

The daemon never holds two live keys simultaneously **except**
during `secrets rotate`, which is the entire point of the rotate
flow (see [§Key rotation](#key-rotation)).

## The `secrets` table

Two new tables. Both belong to the M2 schema and live alongside
the multi-user tables described in
[design/multi-user.md](multi-user.md) (`users`, `accounts`,
`audit_events`, `invites`).

```sql
-- 0002_m2_secrets.sql

CREATE TABLE secrets (
  user_id     TEXT NOT NULL,
  account_id  TEXT NOT NULL,
  kind        TEXT NOT NULL,
  ciphertext  BLOB NOT NULL,
  nonce       BLOB NOT NULL,
  version     INTEGER NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (user_id, account_id, kind),
  FOREIGN KEY (user_id)    REFERENCES users(id)    ON DELETE CASCADE,
  FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
);

CREATE TABLE secrets_meta (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  kdf_salt        BLOB    NOT NULL,
  active_version  INTEGER NOT NULL DEFAULT 1,
  created_at      INTEGER NOT NULL
);
```

Notes on the shape:

- **Composite primary key**. A user can have multiple Resy accounts
  (one personal, one for the household guest list, etc.); each
  account has at most one of each `kind`. The PK reflects that.
- **`version` per row**. Not per-table. Mid-rotation crash recovery
  needs to know which rows have been re-encrypted under the new
  key and which haven't. See [§Key rotation](#key-rotation).
- **`nonce` separate column**. We could prepend the nonce to the
  ciphertext blob; we don't, because keeping it in its own column
  makes "did this row use a unique nonce" inspectable from
  `sqlite3` without writing a Go program.
- **`kdf_salt` lives in `secrets_meta`, not in env or filesystem**.
  The salt is bound to this `data.db`; copying the DB to another
  box and pointing a fresh daemon at it is a supported recovery
  path, and the salt has to come along.
- **`secrets_meta.id = 1`**. There is exactly one row, ever. The
  CHECK constraint enforces it. Schema migration `0002` inserts the
  row with `kdf_salt = randomblob(32)` and `active_version = 1`.
- **No `updated_at`**. A re-encrypt during rotate is conceptually a
  new row — we bump `version` and overwrite. If you need a history,
  use `audit_events`; that's the right place for it.

The migration follows the pattern in
[`internal/store/migrations/0001_initial.sql`](../../../internal/store/migrations/0001_initial.sql)
— numbered file, no down migration, applied idempotently by
`store.Migrate`. See [ADR-0006](../adr/0006-sqlite-only-no-external-deps.md)
for why migrations are forward-only.

## Kinds

`Kind` is a string enum at the Go layer (`type Kind string`). The
initial M2 set:

```go
package secrets

type Kind string

const (
    KindResyPassword   Kind = "resy_password"
    KindResySessionJWT Kind = "resy_session_jwt"
)
```

Adding a kind is an ADR — not because the data model changes (it
doesn't; it's just a string) but because the kind defines the
plaintext byte-shape and that contract has to be written down
somewhere reviewers can find it.

The Service layer translates kinds to provider-shaped values at the
point of use. The Sealer never knows what a JWT looks like; it
seals bytes.

## Sealed-store interface

```go
// Package secrets owns the AES-GCM seal/unseal of recoverable
// secrets at rest. It depends on internal/store (the secrets table),
// internal/domain (UserID, AccountID), and internal/clock. It does
// not depend on cmd/, daemon/, or service/.
package secrets

type Sealer interface {
    // Seal encrypts plaintext under the active key and writes the
    // row, replacing any existing (userID, accountID, kind) row.
    // Idempotent: calling Seal twice with the same plaintext
    // produces two different ciphertexts (fresh nonce each time)
    // but Open returns the same plaintext.
    Seal(ctx context.Context, userID domain.UserID, accountID domain.AccountID, kind Kind, plaintext []byte) error

    // Open decrypts the row. Returns ErrNotFound if the row is
    // missing, ErrTampered if the AEAD auth tag fails, ErrWrongKey
    // if the auth tag fails AND the operator just rotated keys.
    // (At the API surface, ErrTampered and ErrWrongKey are the same
    // failure mode; the daemon distinguishes them via secrets_meta
    // for operator messaging.)
    Open(ctx context.Context, userID domain.UserID, accountID domain.AccountID, kind Kind) ([]byte, error)

    // List returns the kinds present for one (user, account). Used
    // by `resy-snipe accounts inspect` to show what's stored
    // without unsealing.
    List(ctx context.Context, userID domain.UserID, accountID domain.AccountID) ([]Kind, error)

    // Delete removes a row. Cascade-on-delete from users/accounts
    // handles bulk removal; this is for explicit per-kind purge
    // (e.g. `accounts forget-password`).
    Delete(ctx context.Context, userID domain.UserID, accountID domain.AccountID, kind Kind) error

    // Close wipes the in-process key material. Called once during
    // daemon shutdown.
    Close() error
}
```

The implementation is a struct holding the 32-byte key plus a
`store.Store` reference. Construction is the responsibility of
`internal/daemon` — the only package that owns concrete
implementations (see
[overview.md §Dependency rules](overview.md#dependency-rules)).

Service-layer callers receive a `Sealer` via DI; they never touch
the key. The Service's `CreateAccount` flow:

1. Receive `password` from the transport (HTTP/MCP).
2. Call `sealer.Seal(ctx, userID, accountID, KindResyPassword, []byte(password))`.
3. Zero the local `password` variable (`runtime.KeepAlive` plus
   explicit zero loop — Go has no `wipe` primitive).
4. Return.

The plaintext lifetime in the daemon is bounded: from transport
deserialisation to the `Seal` call. Anything longer is a bug.

## Key rotation

Rotation is a single-operator workflow run from the daemon host.
The verb is `resy-snipe secrets rotate`. The implementation is in
`internal/secrets/rotate.go`; it runs in-process inside a long-
lived `serve` (the CLI subcommand sends a request over the local
socket; the daemon does the work).

### Default flow

```
$ resy-snipe secrets rotate
Current passphrase: ******
New passphrase:     ******
Confirm:            ******

Re-encrypting 14 rows... done.
Rotation complete. secrets_meta.active_version = 2.
```

Step by step:

1. Daemon prompts for the **current** passphrase (or reads
   `--current-keyfile`). Derives `key_old`.
2. Daemon prompts for the **new** passphrase (or reads
   `--new-keyfile`). Derives `key_new`.
3. Daemon takes a write lock on the `secrets` table (a SQLite
   `BEGIN IMMEDIATE` plus an in-process `sync.Mutex` held for the
   duration of the rotate). New `Seal` calls block; `Open` calls
   continue using `key_old` until each row is rewritten.
4. For each row in `secrets`:
    - `Open` with `key_old`.
    - `Seal` with `key_new`, fresh nonce.
    - `UPDATE secrets SET ciphertext=?, nonce=?, version=? WHERE …`
      with the new `secrets_meta.active_version`.
5. `UPDATE secrets_meta SET active_version = active_version + 1`.
6. Commit. Release the write lock.
7. Wipe `key_old`. Replace the daemon's live `Sealer` key with
   `key_new`.

The whole thing is one transaction. Either every row is on the new
key with the new `active_version`, or none of them are.

### Mid-rotation crash recovery

The transaction guarantees atomic completion. The recoverable
failure case is the daemon being killed *between* the operator
typing the new passphrase and the COMMIT — at which point SQLite
rolls back, no rows changed, `active_version` unchanged, daemon
boots normally with the old key.

The unrecoverable case is operator-induced: keyfile changed on
disk between rotate sessions, or the operator forgets which
passphrase they used last. In that case:

- On boot, the daemon attempts a sentinel `Open` of one row.
- AEAD auth fails ⟹ daemon refuses to start with
  `event="secrets.boot_unwrap_failed"` and a log line listing
  affected `(user_id, account_id, kind)` triples.
- Operator runbook: see [§Operator runbook](#operator-runbook).

### Refusal: mixed-version table

The daemon refuses to start if any row's `version` does not equal
`secrets_meta.active_version`. This catches:

- A rotate that crashed *outside* the transaction boundary
  (impossible by design, but the check is cheap and defensive).
- An operator who manually edited rows with `sqlite3`.
- A botched migration.

The error names which rows are stale. The fix is always "complete
the rotate that was interrupted" — re-run `secrets rotate`.

### Switching sources

`secrets rotate --to-keyfile <path>` rotates from passphrase to
keyfile (or keyfile to keyfile). `secrets rotate --to-passphrase`
rotates the other way. The daemon writes the *type* of source
into `daemon.Config` (not into `data.db`) so that subsequent boots
know whether to prompt or to read.

## Dev-mode

`resy-snipe serve --insecure-no-encryption --dev` runs the daemon
with `Sealer` replaced by a passthrough that stores plaintext in
`secrets.ciphertext` and a 96-bit zero blob in `secrets.nonce`.
Existence: it lets contributors run `make integration` without
juggling a passphrase.

This mode is hostile to misuse. The daemon refuses to start in
dev-mode if **any** of the following hold:

- Env `RESY_SNIPE_PROD=1` is set. Operators set this in production
  systemd units; it is a tripwire, not a security boundary.
- `os.Stdin` is not a TTY and `--dev` is not also passed.
  Catches "I piped a passphrase into the wrong invocation."
- `/.dockerenv` exists. We don't ship a "dev mode" container; if
  you're inside Docker, you have a real deployment.
- `RESY_SNIPE_DATA_DIR` resolves under `/var/lib`, `/srv`, or
  `/opt`. A best-effort heuristic that catches accidental dev-flag
  use on production boxes.

When dev-mode is active:

- The daemon prints a CRITICAL banner on stderr at boot and on
  every `healthz` response:
  ```
  !!! DEV MODE: SECRETS ARE NOT ENCRYPTED. DO NOT USE IN PRODUCTION. !!!
  ```
- Every `Seal` call writes a structured log record at
  `level=WARN` with `event="secrets.dev_mode_seal"`.
- The `audit_events` row tags the actor with
  `dev_mode=true` so the trail is unambiguous after the fact.

There is no quiet way to run dev-mode. That is the design.

## Logging contract

Plaintext from the secrets layer never appears in logs. The rules:

- **No structured field of any log record may contain a value read
  from `secrets.ciphertext`, `secrets.nonce`, or any unsealed
  plaintext.** Reviewed at lint time
  ([docs/laws.md](../../laws.md) §Logging carries forward; an
  added gate greps for `slog.*("...", ..., plaintext)` patterns).
- The logging adapter (`internal/observability/redact.go`) runs a
  defense-in-depth regex filter against any string field. The
  filter matches:
    - Base64url runs of length ≥ 32 (the JWT shape).
    - UUIDs (the session token shape).
    - Bcrypt prefixes (`$2[abxy]$`).
  Matched values are replaced with `<redacted len=N>`. This is
  alarming-but-defensive: the right answer is for the call site
  not to log the value at all; the regex catches the bug we
  missed in review.
- **Audit log**. `secrets.seal` and `secrets.open` actions are
  recorded in `audit_events` (see [design/multi-user.md](multi-user.md)
  for the table) with `(actor_user_id, target_user_id,
  account_id, kind, action, at)`. Plaintext is never recorded.
  The audit row is the answer to "who unsealed what, when."

The redactor sits between the application and the slog handler;
it cannot be bypassed by writing directly to `log/slog` because
the daemon's `slog.Logger` is constructed *with* the redactor as
its handler. There is no global default logger
([docs/laws.md](../../laws.md) §15).

## Threat model

In-scope. The secrets layer raises the cost of these attacks from
"trivial" to "needs the unwrap key":

- **Stolen disk image**. Attacker `dd`'s the SSD. Without the
  passphrase or keyfile, `data.db` is opaque for `secrets`-table
  rows.
- **Stolen backup**. Operator's `restic` snapshot is exfiltrated.
  Same property as stolen disk, provided the backup didn't also
  include the keyfile / passphrase manager.
- **Casual `ssh` as a non-root user**. A user account on the box
  reads `data.db` (group-readable by accident, NFS export, etc.).
  Cannot unseal without the in-process key.
- **Co-tenant on the same SQLite file**. Another daemon process
  with read access to `data.db` sees ciphertext. Cannot unseal
  without the in-process key.

Out-of-scope. The secrets layer does not protect against these and
should not be expected to:

- **Root on the daemon's box**. Root reads `/proc/<pid>/mem` and
  extracts the AES key from the daemon's address space. `mlock`
  prevents swap leaks; it does not stop a privileged ptrace.
  Game over either way.
- **Memory-extraction with physical access**. Cold-boot, JTAG,
  hibernation file. Out-of-scope — same reasoning as root.
- **Supply-chain compromise of the binary**. A modified
  `resy-snipe` binary that exfils the key on derivation. The fix
  is reproducible builds and signed releases, not anything the
  secrets layer can do.
- **Operator typing the passphrase into a logged terminal session**.
  We disable echo on the prompt; we cannot prevent an operator
  from running `tee` over their own session.
- **Resy itself being compromised**. If Resy's API is owned, the
  JWTs we hold are useless to us *and* to an attacker — but
  that's not a property of our sealing.

[ADR-0008](../adr/0008-secrets-sealed-at-rest-operator-key.md) is
the authoritative threat-model statement; this section is the
field guide.

## Operator runbook

Five scenarios cover ~all real-world pain.

### "I forgot my passphrase."

Secrets are unrecoverable. Argon2id with a 32-byte salt is not a
brute-force target. The recovery path:

1. Stop the daemon.
2. `DELETE FROM secrets;` (or drop and recreate the table — both
   are fine).
3. Restart the daemon with a new passphrase.
4. Re-add Resy accounts via `resy-snipe accounts add`.
5. Existing quests in non-terminal states refer to accounts whose
   credentials no longer exist; the daemon will fail their next
   tick with `providers.ErrAuthRequired`. Cancel and recreate
   them.

This is documented in [docs/getting-started.md](../../getting-started.md)
under "Backup your passphrase before you forget it."

### "I want to migrate from passphrase to keyfile."

```
$ resy-snipe secrets rotate --to-keyfile /etc/resy-snipe/key.hex
Current passphrase: ******
Re-encrypting 14 rows... done.
Update your serve unit to pass --keyfile /etc/resy-snipe/key.hex.
```

### "I rotated the keyfile by mistake without rotating secrets."

The daemon won't start; it sentinel-opens a row and fails. Recovery:

1. Restore the previous keyfile from your backup. (You took a
   backup before changing it, right?)
2. Start the daemon with the restored keyfile.
3. Run `resy-snipe secrets rotate --to-keyfile <new-path>` properly.

If the previous keyfile is unrecoverable: same path as forgotten
passphrase — drop `secrets`, re-add accounts.

### "Can I store the keyfile next to `data.db`?"

You can. You should not. The threat model above assumes the
keyfile and `data.db` are not co-located. If they are:

- Stolen-disk and stolen-backup attacks regress to v1 levels.
- The daemon does not detect this — it cannot tell where
  `--keyfile` is mounted from.

The runbook target is: keyfile lives on a separate block device,
or is age-encrypted, or is fetched from a secret manager at boot.

### "Backup strategy."

- Back up `data.db` to your usual restic/borg target.
- Back up the keyfile (or the passphrase entry in your password
  manager) to a *different* target with *different* credentials.
- A single backup snapshot must not contain both. If your operator
  backs up the whole `/etc` and `/var/lib` to one repo, your
  `data.db` and your keyfile are in the same snapshot — same
  problem as co-locating them.

`resy-snipe doctor` (see
[design/observability.md](observability.md)) checks for the common
co-location footgun and prints a warning if it fires.

## Test plan

The tests live in `internal/secrets/secrets_test.go`. Each one is
independently runnable with `go test -race ./internal/secrets/...`.

- **Round-trip seal/open**. Derive a key from a known passphrase
  with the documented Argon2id parameters and a fixed salt. Seal
  a known plaintext, Open it back, assert byte-equality. Pin the
  test against an externally-computed Argon2id KAT so a parameter
  drift fails the test.
- **Tampered ciphertext**. Seal, flip one bit in `secrets.ciphertext`,
  Open ⟹ `ErrTampered`. Repeat for `secrets.nonce`. Repeat for
  the associated-data binding by changing `kind` on the row
  without re-encrypting.
- **Wrong passphrase**. Construct two `Sealer`s from different
  passphrases. Seal with one, Open with the other ⟹ `ErrWrongKey`.
  Note: we do not pre-validate the key by attempting an Open at
  boot — the failure is surfaced at the call site. The test
  asserts that path explicitly.
- **Missing row**. Open a `(user, account, kind)` that was never
  sealed ⟹ `ErrNotFound` (not `ErrWrongKey`).
- **Mid-rotation recovery**. Start a rotate, kill the process
  before COMMIT, restart ⟹ daemon comes up on the old key, all
  rows still openable. Re-run `secrets rotate` to completion.
- **Mixed-version refusal**. Manually `UPDATE secrets SET version=99
  WHERE …` to one row, restart ⟹ daemon refuses, names the row.
- **Plaintext mode requires explicit dev flags**. Spawn the daemon
  with `--insecure-no-encryption` but no `--dev` ⟹ refuses.
  With both flags but `RESY_SNIPE_PROD=1` ⟹ refuses. With both
  flags and no production tripwire ⟹ boots, banner present.
- **Dev-mode banner**. Boot in dev-mode, hit `/healthz`, assert the
  banner is in the response body and on stderr.
- **Mlock unavailable doesn't crash**. Build-tag-gated test on
  Windows/WSL; assert that `Sealer` construction succeeds and the
  warning is emitted.
- **Logging redactor**. Construct a `slog.Logger` wrapped with the
  redactor, log a record carrying a 64-char base64 string, assert
  the output is `<redacted len=64>`.
- **Cascade**. `DELETE FROM users WHERE id = ?` cascades to
  `secrets`. Sanity check on the FK.

The test plan does not cover: the operator runbook scenarios
(those are manual), or the threat model (those are written to be
out-of-scope or unfalsifiable in unit tests).

## Cross-references

- The data model context for `users` / `accounts` / `audit_events`:
  [design/multi-user.md](multi-user.md).
- Where the `Sealer` is constructed and wired: the daemon's boot
  order in [design/daemon.md](daemon.md) §Boot.
- How the Service layer consumes the `Sealer`:
  [design/service-layer.md](service-layer.md) §Sealing seam.
- The decision to seal at all and the threat-model ADR:
  [ADR-0008](../adr/0008-secrets-sealed-at-rest-operator-key.md).
- Why no external dependency (KMS, Vault) shows up here:
  [ADR-0006](../adr/0006-sqlite-only-no-external-deps.md).
- The unchanged engine, store, and clock invariants this layer
  inherits from v1: [docs/architecture.md](../../architecture.md),
  [docs/laws.md](../../laws.md),
  [docs/invariants.md](../../invariants.md).
