# ADR 0008: Secrets sealed at rest; key controlled by operator

**Status**: Accepted
**Date**: 2026-05-10
**Decision-makers**: phall
**Related**: [ADR-0006](0006-sqlite-only-no-external-deps.md),
[ADR-0007](0007-self-hosted-only-no-saas.md),
[design/secrets.md](../design/secrets.md)

## Context

`data.db` will hold:

- Resy account passwords (hashed *for the homelab user* — see
  [ADR-0011](0011-operator-issued-invites-no-self-registration.md));
  but the *Resy* account credentials are stored as recoverable secrets
  because the daemon needs them to re-login when sessions expire.
- Resy session JWTs.
- Anti-bot signer state if applicable.

A laptop backup, NAS snapshot, or stolen disk image must not yield
those secrets. "Encrypted disk volume" is good defense-in-depth but
not enough — the SQLite file routinely gets `cp`'d for backups, may
land on Time Machine, etc.

## Decision

Secrets are sealed at rest by the daemon using **AES-GCM with a key
the operator controls**, never co-located with `data.db`. The unwrap
key is derived at daemon start from one of:

1. **Operator passphrase** prompted on stdin (or read from
   `RESY_SNIPE_PASSPHRASE` for systemd `LoadCredential`/Docker
   secrets), via Argon2id KDF. *Default.*
2. **`--keyfile <path>`** — operator points the daemon at an unsealed
   key file (e.g. an `age` identity, a 1Password CLI fetch, a tmpfs
   path). The daemon never copies it.

The derived key lives in process memory only. It is mlocked, never
written to disk, never logged, and is wiped on shutdown.

A `secrets` table in `data.db` stores the encrypted blobs:

```sql
CREATE TABLE secrets (
  user_id     TEXT NOT NULL,
  account_id  TEXT NOT NULL,
  kind        TEXT NOT NULL,   -- 'resy_password' | 'resy_session' | ...
  ciphertext  BLOB NOT NULL,   -- AES-GCM(plaintext, derived_key, nonce)
  nonce       BLOB NOT NULL,
  version     INT  NOT NULL,   -- key-rotation epoch
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (user_id, account_id, kind)
);
```

If `data.db` is taken without the unwrap key, the secrets are useless.

## Consequences

**Positive**
- Backups can be made of `data.db` alone. The key never lands in the
  same backup unless the operator chooses to put it there.
- The operator chooses their threat model: prompt-each-start (highest
  security, lowest convenience) or keyfile (lowest friction, requires
  separate-disk hygiene).
- Key rotation is a real operation: bump `version`, re-encrypt all
  rows, retire old key. A `secrets-rotate` admin subcommand handles
  it.

**Negative**
- Daemon needs an interactive start (or `LoadCredential`, or env-var
  injection). Plain `systemctl start` with no credential setup will
  fail by design — that's the point.
- Forgotten passphrase = lost secrets. Mitigated by operator
  documentation strongly recommending an offline copy.

**Neutral**
- Argon2id parameters are conservative-by-default (m=64MB, t=3, p=1).
  Tunable via config for slow boxes.

## Alternatives considered

1. **Plaintext in SQLite.** *Rejected:* a stolen backup is a
   credential dump.
2. **OS keychain (macOS Keychain, libsecret, DPAPI).** *Rejected:*
   not portable across the deployment shapes we care about (Linux
   container, macOS dev, headless homelab). Keychain on a Synology
   NAS is "lol."
3. **HSM / KMS / Vault.** *Rejected:* adds an external dependency
   (violates [ADR-0006](0006-sqlite-only-no-external-deps.md) spirit).
   Operator-managed key files cover the same use case for our scale.
4. **Encrypt the whole database file** (e.g. SQLCipher). *Rejected:*
   couples to a custom SQLite build, complicates backups (`cp` on a
   live SQLCipher file is fine but `litestream` / `sqlite3` CLI need
   the key). Per-row sealing is simpler and lets us encrypt only what
   matters.

## Notes

A "**dev-mode**" with `--insecure-no-encryption` exists but the daemon
refuses to start if invoked from a unit file or Docker entrypoint
(detected by absence of TTY + presence of `RESY_SNIPE_PROD=1`).
Operators must intentionally disable encryption — it never happens by
accident.

Plaintext logs never include secret values. The
[design/observability.md](../design/observability.md) logging contract
forbids logging anything from a `secrets.*` column.

See [design/secrets.md](../design/secrets.md) for the full sealing
protocol, KDF parameters, and rotation procedure.
