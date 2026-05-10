# Multi-user data model

resy-snipe v2 is multi-tenant from M1, not retrofitted later.
One daemon serves every homelab user on a box; one SQLite file
holds every user's data; every row carries a `user_id`; every
Service call carries a `UserID`; every Store query joins on it.
There is no "single-user mode" code path — a solo deployment is
one row in `users`.

This doc is the schema, the tenancy contract, and the
lifecycle. For the *why*, read:

- [ADR-0005](../adr/0005-multi-user-data-model-from-day-one.md) —
  multi-user lands in M1.
- [ADR-0010](../adr/0010-one-daemon-many-users.md) — one daemon, many
  users, logical tenancy.
- [ADR-0011](../adr/0011-operator-issued-invites-no-self-registration.md)
  — operator-issued invites, no self-registration.
- [ADR-0008](../adr/0008-secrets-sealed-at-rest-operator-key.md) —
  sealed-at-rest secrets (referenced by `secrets` table here).
- [ADR-0006](../adr/0006-sqlite-only-no-external-deps.md) — SQLite
  only.
- [design/secrets.md](secrets.md) — the seal protocol that fills the
  `secrets.ciphertext` blob.
- [docs/laws.md](../../laws.md), [docs/architecture.md](../../architecture.md).

## Vocabulary, fixed

Two words are load-bearing and easy to confuse:

- **User** — a homelab tenant. Authenticates with a password.
  `email` + opaque ID `usr_…`. Examples: phall, james.
- **Account** — a Resy login owned by a user. ID `acct_…`.
  A user has >=1 accounts; an account is owned by exactly one
  user in v1 (cross-user sharing deferred — see §15).

A user invites users. An account books reservations. v1's
`users` (Resy logins) becomes v2's `accounts`; v2's `users` is
new. See §14 for the migration.

## 1. Entity model

```
users (homelab tenants)
  |-- invites    (token -> user signup; consumed at accept)
  |-- tokens     (bearer auth per user; many per user)
  |-- accounts   (Resy logins owned by user)
  |     |-- sessions  (JWT bag, exists today)
  |     |-- secrets   (sealed Resy creds + JWT; see design/secrets.md)
  |-- quests     (owned by user, references account)
        |-- intents       (planner output, snapshot per run)
        |-- runs          (engine execution attempts)
        |-- quest_events  (engine state-change log)

audit_events (who-did-what across all users; one row per Service call)
```

`users` is the only table without a `user_id` FK (it is the root).
Every other user-data table has `user_id NOT NULL REFERENCES
users(id) ON DELETE CASCADE`. `audit_events` has two FK columns —
`user_id` (actor) and `target_user_id` (subject) — so admin actions
on another user appear in both views.

Cross-tenant tables (no `user_id`): `venues` and
`observed_release_times` (public Resy data, v1, unchanged) and
`schema_migrations` (bookkeeping). Everything else is tenant-scoped.

## 2. Schema (SQL DDL)

Type conventions: `TEXT` for opaque IDs, `INTEGER` for
unix-millis timestamps in v2, `BLOB` for raw bytes (password
hashes, ciphertext, nonces, token hashes). v1 tables keep
ISO8601; new tables use millis. `PRAGMA foreign_keys = ON` on
the connection.

### users

```sql
CREATE TABLE users (
    id            TEXT    PRIMARY KEY,                         -- 'usr_8xK3aZ'
    email         TEXT    NOT NULL UNIQUE,
    password_hash BLOB    NOT NULL,                            -- argon2id PHC-encoded
    role          TEXT    NOT NULL CHECK (role IN ('admin','user','readonly')),
    invited_by    TEXT             REFERENCES users(id) ON DELETE SET NULL,
    created_at    INTEGER NOT NULL,                            -- unix millis
    disabled_at   INTEGER                                      -- soft delete
);

CREATE INDEX idx_users_email ON users(email);
```

### invites

DDL from
[ADR-0011 §Notes](../adr/0011-operator-issued-invites-no-self-registration.md#notes),
timestamps tightened to unix millis:

```sql
CREATE TABLE invites (
    token_hash   BLOB    PRIMARY KEY,                          -- SHA-256(token)
    email        TEXT    NOT NULL,
    role         TEXT    NOT NULL CHECK (role IN ('admin','user','readonly')),
    invited_by   TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    consumed_at  INTEGER                                       -- nullable; set on accept
);

CREATE INDEX idx_invites_email ON invites(email);
CREATE INDEX idx_invites_expires ON invites(expires_at);
```

### tokens

```sql
CREATE TABLE tokens (
    token_hash    BLOB    PRIMARY KEY,                         -- SHA-256(token)
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scope         TEXT    NOT NULL,                            -- 'api' | 'mcp' | 'cli'
    label         TEXT    NOT NULL,
    last_used_at  INTEGER,                                     -- nullable until first use
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER,                                     -- nullable = no expiry
    revoked_at    INTEGER
);

CREATE INDEX idx_tokens_user ON tokens(user_id);
```

### accounts

```sql
CREATE TABLE accounts (
    id            TEXT    PRIMARY KEY,                         -- 'acct_2pQ7vL'
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    resy_email    TEXT    NOT NULL,
    display_name  TEXT    NOT NULL,                            -- 'personal', 'work'
    created_at    INTEGER NOT NULL,
    disabled_at   INTEGER,
    UNIQUE (user_id, resy_email)
);

CREATE INDEX idx_accounts_user ON accounts(user_id);
```

### sessions

The v1 `sessions` table, rebound from `(user_id=resy_email)` to
`account_id` by the migration in §14. Timestamps stay ISO8601 to
match the v1 rows preserved through migration.

```sql
CREATE TABLE sessions (
    account_id    TEXT    NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    provider      TEXT    NOT NULL,                            -- 'resy'
    jwt           TEXT    NOT NULL,
    exp           TEXT    NOT NULL,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (account_id, provider)
);

CREATE INDEX idx_sessions_account ON sessions(account_id);
```

`jwt` is plaintext for v1 compatibility. M2 moves it into
`secrets` (sealed) and leaves `sessions` holding metadata only.
See [design/secrets.md](secrets.md).

### secrets

Sealed-at-rest credential store. The seal protocol (KDF, AAD,
nonce, version handling) is owned by
[design/secrets.md](secrets.md) and
[ADR-0008](../adr/0008-secrets-sealed-at-rest-operator-key.md).
This table is the storage shape only.

```sql
CREATE TABLE secrets (
    user_id     TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id  TEXT    NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL,                              -- 'resy_password' | 'resy_jwt' | 'signer_state'
    ciphertext  BLOB    NOT NULL,
    nonce       BLOB    NOT NULL,                              -- 12 bytes; per-row, never reused
    version     INTEGER NOT NULL,                              -- KDF/AEAD scheme version
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (user_id, account_id, kind)
);

CREATE INDEX idx_secrets_account ON secrets(account_id);
```

`user_id` is denormalized (it's already implied by `account_id`)
because §10's tenancy-check requires every user-data query to
filter on `user_id` directly without a join.

### quests

```sql
CREATE TABLE quests (
    id            TEXT    PRIMARY KEY,                         -- 'q_5kP2Mn'
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id    TEXT    NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    goal_json     TEXT    NOT NULL,                            -- canonicalized domain.Goal
    plan_hash     TEXT    NOT NULL,                            -- SHA-256 hex of Plan
    status        TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    completed_at  INTEGER,
    CHECK (status IN ('pending','scheduled','firing','booked','failed','cancelled'))
);

CREATE INDEX idx_quests_user_status ON quests(user_id, status);
CREATE INDEX idx_quests_user_created ON quests(user_id, created_at);
CREATE INDEX idx_quests_account ON quests(account_id);
```

### intents

```sql
CREATE TABLE intents (
    id            TEXT    PRIMARY KEY,                         -- 'int_3qR9Xy'
    quest_id      TEXT    NOT NULL REFERENCES quests(id) ON DELETE CASCADE,
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    intent_hash   TEXT    NOT NULL,                            -- domain.IntentHash hex
    intent_json   TEXT    NOT NULL,                            -- canonicalized domain.Intent
    drop_moment   INTEGER NOT NULL,
    strategy      TEXT    NOT NULL,                            -- ReleaseStrategy id
    created_at    INTEGER NOT NULL
);

CREATE INDEX idx_intents_quest ON intents(quest_id);
CREATE INDEX idx_intents_hash ON intents(intent_hash);
```

`user_id` denormalized for tenancy enforcement. The v1 `snipes`
table is unchanged; v2 joins `quests -> intents -> snipes` via
`intent_hash`.

### runs

```sql
CREATE TABLE runs (
    id            TEXT    PRIMARY KEY,                         -- 'run_7vK1Pq'
    quest_id      TEXT    NOT NULL REFERENCES quests(id) ON DELETE CASCADE,
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    intent_id     TEXT    NOT NULL REFERENCES intents(id) ON DELETE CASCADE,
    started_at    INTEGER NOT NULL,
    ended_at      INTEGER,
    outcome       TEXT,                                        -- 'booked'|'failed'|'cancelled'|'aborted'
    error_code    TEXT,                                        -- domain.ErrCode on failure
    confirmation  TEXT                                         -- Resy confirmation on booked
);

CREATE INDEX idx_runs_quest ON runs(quest_id);
CREATE INDEX idx_runs_user_started ON runs(user_id, started_at);
```

### events (quest_events)

The v1 `events` table (snipe-scoped) is unchanged. v2 adds a
quest-scoped peer:

```sql
CREATE TABLE quest_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    quest_id      TEXT    NOT NULL REFERENCES quests(id) ON DELETE CASCADE,
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    run_id        TEXT             REFERENCES runs(id) ON DELETE CASCADE,
    type          TEXT    NOT NULL,                            -- 'plan'|'create'|'schedule'|'fire'|'find'|'prepare'|'book'|'confirm'|'cancel'|'fail'
    at            INTEGER NOT NULL,
    fields_json   TEXT
);

CREATE INDEX idx_quest_events_quest ON quest_events(quest_id, at);
CREATE INDEX idx_quest_events_user ON quest_events(user_id, at);
```

This is what `WatchEvents` subscribers (CLI `quest watch`, MCP
`notifications/quest_event`) stream from.

### audit_events

```sql
CREATE TABLE audit_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,   -- actor
    target_user_id  TEXT             REFERENCES users(id) ON DELETE SET NULL,  -- subject; may equal actor
    action          TEXT    NOT NULL,                                          -- stable string; see §12
    target_id       TEXT,                                                      -- 'q_…' | 'acct_…' | 'usr_…' | NULL
    ok              INTEGER NOT NULL CHECK (ok IN (0, 1)),
    error_code      TEXT,                                                      -- domain.ErrCode on !ok
    ip              TEXT,                                                      -- caller IP if HTTP
    user_agent      TEXT,                                                      -- caller UA if HTTP
    created_at      INTEGER NOT NULL,
    details_json    TEXT
);

CREATE INDEX idx_audit_actor ON audit_events(user_id, created_at);
CREATE INDEX idx_audit_target ON audit_events(target_user_id, created_at);
CREATE INDEX idx_audit_action ON audit_events(action, created_at);
```

`target_user_id` is `SET NULL` on user delete so the audit trail
survives the subject's purge. `details_json` is bounded (<=4KiB);
fat payloads belong in `quest_events`.

## 3. `users` — detail

- **ID format.** `usr_` + 6 chars of base32 `crypto/rand`. New
  `UserID` type added to
  [`internal/domain/ids.go`](../../../internal/domain/ids.go);
  v1's `UserID` is renamed `AccountID` — see §14.
- **Email.** Case-folded on insert; trimmed and lowercased on
  login.
- **Password hash.** argon2id with `m=64MiB, t=3, p=1`, 16-byte
  salt, 32-byte hash, PHC-encoded so verification re-derives
  with the embedded params. Cost rotation is hash-by-hash.
- **Role.** Three values, DB-checked, immutable post-invite —
  to change, admin disables and re-invites.
- **`invited_by`.** Nullable for the seeded operator; non-NULL
  otherwise. A partial unique index (added by §14) enforces at
  most one operator row.
- **`disabled_at`.** Soft delete. Cannot log in, cannot call
  Service methods, audit retained. Hard delete is operator-only
  via SQL and cascades.

## 4. `tokens` — detail

**Creation.** `POST /v1/tokens` (auth by password) or
`resy-snipe token issue --label foo --scope api`:

1. Generate 256 bits via `crypto/rand`, base64url-encode -> secret.
2. Compute `sha256(secret)` -> `token_hash`.
3. INSERT one row; return secret to caller once.

**Verification.** Bearer-token auth on every API/MCP request:

1. Receive raw token from `Authorization: Bearer <token>`;
   compute `sha256(received)`.
2. SELECT by `token_hash`; constant-time compare with
   `subtle.ConstantTimeCompare`.
3. Reject if `revoked_at IS NOT NULL`, `expires_at < now`, or
   no row, or `users.disabled_at IS NOT NULL`.
4. Best-effort `UPDATE … SET last_used_at = ?` (coalesced 1Hz
   per token; failure does not deny the request).
5. Pass `user_id`, `scope` to the Service call.

`scope` is informational in v1; reserved for per-scope authz
later so the table doesn't migrate. Hash-only storage means a
DB dump cannot be turned into working tokens — same posture as
`invites.token_hash`.

## 5. `accounts` — detail

A Resy login owned by exactly one user. Cross-user sharing is
out of scope for v1 — see
[ADR-0010 §Notes](../adr/0010-one-daemon-many-users.md#notes)
and §15.

- **ID format.** `acct_` + 6 chars of base32. Different prefix
  from `usr_` so a typo never crosses tables silently.
- **`resy_email`.** Unique within a user, not globally.
- **`display_name`.** Operator-set human label. Never sent to
  Resy.
- **No password column.** Resy password lives in `secrets`,
  keyed `(user_id, account_id, 'resy_password')`.
- **`disabled_at`.** Soft delete. Cancels pending quests on
  this account, deletes `sessions`, wipes plaintext caches,
  leaves `secrets` intact (`--purge` removes them too).

## 6. `secrets` — detail

Cross-link: this is *storage*. The *seal protocol* (KDF, AES-
256-GCM AEAD, AAD, nonce policy, dev-mode key, rotation) lives
in [design/secrets.md](secrets.md) and
[ADR-0008](../adr/0008-secrets-sealed-at-rest-operator-key.md).

- **Read.** `secrets.Open(ctx, userID, accountID, kind)`. Looks
  up the row, checks `version`, builds AAD from
  `(userID, accountID, kind, version)`, calls
  `aead.Open(nonce, ciphertext, aad)`. Plaintext never lands
  in another column.
- **Write.** `secrets.Seal(ctx, userID, accountID, kind,
  plain)`. Generates a fresh 12-byte nonce (AES-GCM nonce
  reuse is catastrophic), builds AAD, upserts.
- **`version`.** Lets us swap KDF/AEAD without a flag day; old
  rows verify with old params, new writes use current.

**`kind` vocabulary in v1.**

| kind             | purpose                                           |
|------------------|---------------------------------------------------|
| `resy_password`  | written on `account add`; read on JWT expiry.     |
| `resy_jwt`       | (M2) replaces plaintext `sessions.jwt`.           |
| `signer_state`   | opaque blob if a signer wants persistent state.   |

## 7. `quests` — detail

- **`goal_json`.** Canonicalized `domain.Goal` (sorted keys, no
  insignificant whitespace, RFC3339 times with zone, lowercase
  IDs). Persisted so we can replan deterministically.
- **`plan_hash`.** SHA-256 of the canonicalized Plan returned
  at `quest plan` time. At `quest create`, the service
  recomputes from `(goal_json, venue_state, now)` and verifies;
  mismatch = 409 "re-plan required." See
  [ADR-0012](../adr/0012-plan-first-ux.md).
- **`status`.** DB-checked. Transitions: `pending -> scheduled
  -> firing -> { booked | failed | cancelled }`, with
  `pending|scheduled -> cancelled` shortcut. `pending` is
  transient.
- **`completed_at`.** Set on entry to any terminal state.

## 8. `audit_events` — full detail

Two FK columns: `user_id` is the *actor* (whose token was
used); `target_user_id` is the *subject* (whose data is
affected). For self-actions they're equal; for admin actions
on other users, they differ.

| action            | user_id (actor) | target_user_id (subject)        |
|-------------------|-----------------|---------------------------------|
| `quest.create`    | phall           | phall                           |
| `user.invite`     | phall (admin)   | NULL (no user yet)              |
| `user.disable`    | phall (admin)   | james                           |
| `user.reset`      | phall (admin)   | james                           |
| `user.accept`     | james           | james                           |
| `audit.read`      | phall (admin)   | NULL (cross-user listing)       |

Three indexes — actor-recent, target-recent, action-recent —
serve the common queries (one user's activity, everything done
to one user, every failed login this month).

**Write protocol.** The Service layer wraps every public method
in audit middleware: record `start`, call the method, INSERT
one row with `ok=1` (or `ok=0` and `error_code`). The audit
insert is in the same transaction as the Service call's primary
write when one exists (a failed `quest.create` rolls back the
audit row too; a fresh `ok=0` audit row is inserted outside the
failed tx). Hard DB failure falls through to the structured-log
sink.

**`details_json` bound.** <=4KiB per row, enforced at write
time. Never includes secrets, passwords, or full Goal/Plan
payloads — those belong in `quest_events`.

Sample row — `phall` cancels `q_5kP2Mn`:

```json
{
  "id": 18421,
  "user_id": "usr_phall1",
  "target_user_id": "usr_phall1",
  "action": "quest.cancel",
  "target_id": "q_5kP2Mn",
  "ok": 1,
  "error_code": null,
  "ip": "127.0.0.1",
  "user_agent": "resy-snipe-cli/2.0.0",
  "created_at": 1747000000000,
  "details_json": "{\"prev_status\":\"scheduled\",\"reason\":\"manual\"}"
}
```

## 9. `invites` — copied from ADR-0011

DDL above in §2. See
[ADR-0011](../adr/0011-operator-issued-invites-no-self-registration.md)
for token format (256 bits `crypto/rand`, base64url, single-use,
default 7-day expiry), rate-limit posture on the acceptance
endpoint, and rationale. We store `sha256(token)`, not the
token, so a DB dump cannot reuse outstanding invites. `email`
is *not* unique — multiple outstanding invites for one email
are allowed.

## 10. Tenancy enforcement contract

The single most important property of v2's data model: a user
can only see their own data, and the schema forces it.

**Schema rules.**

- Every user-data table has `user_id TEXT NOT NULL REFERENCES
  users(id) ON DELETE CASCADE`.
- Denormalization in `secrets` and `intents` (where `user_id`
  is implied by another FK) is intentional — every query
  filters on `user_id` directly without a join.
- Cross-tenant tables (`venues`, `observed_release_times`,
  `schema_migrations`) are listed and reviewed as exceptions.

**Store interface rules.**

- Every Store method that reads or writes user data takes
  `userID domain.UserID` (typically second, after `ctx`).
- Every emitted SQL query includes `WHERE user_id = ?` (or
  `INSERT … (user_id, …) VALUES (?, …)`).
- `audit_events` reads parameterize on `user_id`,
  `target_user_id`, or both. Cross-user reads are role-gated
  at the Service layer.

**`tenant_check.go` test.** Build tag `tenancy`. Run as
`go test -tags=tenancy ./internal/store/...`. The test:

1. Reflects over `Store` to enumerate exported methods.
2. Asserts each takes a `domain.UserID` *or* is on an explicit
   cross-tenant allowlist (`UpsertVenue`,
   `RecordObservedRelease`, `RunMigrations`, etc.) — listed in
   `tenant_check.go` with a comment per entry.
3. For each user-scoped method, runs it against an in-memory
   SQLite seeded with `usr_a`, `usr_b` (each with
   account/quest/etc.) and verifies: `usr_a` calls return only
   `usr_a`'s data; `usr_b` calls return only `usr_b`'s; captured
   SQL (via a driver wrapper) contains `user_id = ?` in WHERE.
4. New Store method without a `UserID` and not on the
   allowlist, or emitting a query without `user_id` filtering,
   = test fails.

The test is wired into CI as a required check.

**Service-layer rules.** Every public Service method takes
`userID domain.UserID`. The Service never re-derives `userID`
from a request payload — it trusts the auth-middleware value.
Cross-user methods (`AdminListUsers`, `AdminReadAudit`)
role-check `admin` before calling Store and audit the attempt
regardless of outcome.

These rules together make tenancy violations a build/test
failure, not a runtime bug.

## 11. Roles and permissions

| role       | Service access                                   | User-mgmt verbs                | Audit reads      |
|------------|--------------------------------------------------|--------------------------------|------------------|
| `admin`    | full, on own + via admin verbs cross-user        | invite/list/disable/reset all  | own + every user |
| `user`     | full, on own data only                           | none                           | own only         |
| `readonly` | list/get on own; no create/cancel/seal           | none                           | own only         |

The first user (seeded by the operator) is `admin` with
`invited_by = NULL`. Subsequent invites default to `user`. Admin
can explicitly invite another admin via `--role=admin`.

Roles are immutable post-invite — to change, admin disables
then re-invites with the new role; the user accepts with a new
password. Audit shows the chain. Readonly enforcement is
Service-layer; Store does not check role (Store is tenancy-only;
role is policy, tenancy is mechanism). Admin scope is per-box —
no global admin; each daemon is its own world.

## 12. Audit-log conventions

Every Service call writes one row, success or failure. Action
names are stable lowercase `<resource>.<verb>` strings. Treat
like log-level names; don't rename without a migration.

**Action vocabulary, v1.**

Auth:
- `auth.login` — password login attempt; `target_id` = user_id
  on success, NULL on failure.
- `auth.logout` — token revoke or session end.

Users:
- `user.invite` — admin issued an invite.
- `user.accept` — invitee set password and consumed invite.
- `user.disable` — admin disabled a user.
- `user.enable` — admin re-enabled a previously disabled user.
- `user.reset` — admin issued a password-reset invite.
- `user.list` — admin enumerated users.

Tokens:
- `token.issue` — token created.
- `token.revoke` — token revoked.
- `token.list` — user listed their own tokens (or admin listed
  any user's).

Accounts:
- `account.add` — user added a Resy account; secrets sealed.
- `account.remove` — user disabled/removed a Resy account.
- `account.list` — user listed their accounts.
- `account.login` — engine performed Resy login (auto-refresh).

Quests:
- `quest.plan` — service computed a Plan (no persistence).
- `quest.create` — quest persisted; engine scheduled.
- `quest.cancel` — user cancelled a quest.
- `quest.replan` — quest's plan_hash refreshed.
- `quest.list` — user listed their quests.
- `quest.get` — user fetched a quest.
- `quest.watch` — user subscribed to events stream (audited
  on subscribe, not per event).

Audit (admin):
- `audit.read` — admin read another user's audit log.

Engine (autonomous, attributed to the quest's user):
- `engine.fire` — entered Firing.
- `engine.book` — booking succeeded.
- `engine.fail` — terminal failure.

Conventions: new actions land via a constant in
`internal/audit/actions.go` plus a line above. Renames require
a migration that rewrites historical rows. `target_id` matches
the action's primary subject (`q_…`, `acct_…`, `usr_…`).
Coverage is enforced by §16.8.

## 13. Lifecycle operations

Each operation is a CLI verb on `cmd/resy-snipe` hitting a
Service method. HTTP and MCP transports expose the same.

### `user invite`

Admin generates an invite (per
[ADR-0011](../adr/0011-operator-issued-invites-no-self-registration.md)):

```
$ resy-snipe user invite james@example.com --role=user --expires=7d
invite link: https://snipe.phall.example/invite/8xK3aZ2qR…
expires:     2026-05-17 23:59 UTC
```

`Service.InviteUser(ctx, adminID, email, role, expiresIn)`
generates token, hashes, INSERTs `invites`. Audit `user.invite`
(target_user_id = NULL).

### `user accept-invite`

Invitee opens the link, sets a password, gets a bearer token.
`Service.AcceptInvite(ctx, token, password)` runs in one tx:
SELECT `WHERE token_hash = sha256(token) AND consumed_at IS
NULL AND expires_at > now`; argon2id the password; INSERT the
`users` row (with `invited_by` from the invite); UPDATE
`invites SET consumed_at = now`; INSERT a fresh `tokens` row;
audit `user.accept`. The invite stays consumable on
transactional failure.

### `user list` / `disable` / `reset`

Admin operations.

- **list** — `Service.ListUsers(ctx, adminID)`. Role-checks
  `admin`. Audit `user.list`.
- **disable** — `Service.DisableUser(ctx, adminID, targetID)`:
  sets `disabled_at`, cancels pending/scheduled quests (one
  audit row per cancel), revokes outstanding tokens, wipes
  in-memory caches, audit `user.disable`.
- **reset** — `Service.ResetUser(ctx, adminID, email)`: INSERTs
  a fresh invite for the email + their existing role. The old
  password keeps working until the new invite is accepted (so
  the user isn't locked out if they lose the new link); on
  acceptance, `password_hash` is replaced. Audit `user.reset`
  on issue, `user.accept` on consume.

### `account add` / `remove`

- **add** — `Service.AddAccount(ctx, userID, resyEmail,
  resyPass, displayName)`: calls `provider.Login` (must
  succeed), INSERTs `accounts`, `secrets.Seal(…,
  'resy_password', resyPass)`, UPSERT `sessions`, audit
  `account.add`.
- **remove** — `Service.RemoveAccount(ctx, userID, accountID)`:
  verifies ownership, cancels pending quests, sets
  `disabled_at`, deletes the `sessions` row; with `--purge`,
  also deletes `secrets`. Audit `account.remove`. Quest history
  and audit retained.

### `quest create` / `cancel` / `list`

Standard Service flow — see
[design/overview.md](overview.md#request-flow-goal-to-booked-reservation).
Every call carries `userID`; every Store query joins on it;
every action writes one audit row.

## 14. Migration plan from v1 schema

v1 has two tables that overlap with v2's vocabulary:

- v1 `users` — Resy users, keyed by Resy email. In v2 this becomes
  `accounts`.
- v1 `sessions` — keyed `(user_id=resy_email, provider)`. In v2 it's
  keyed by `account_id`.

v2 `users` is a brand-new table for homelab tenants.

### Migration `0002_multi_user.sql`

Idempotent. Outline (line-by-line in
[`internal/store/migrations/0002_multi_user.sql`](../../../internal/store/migrations/0002_multi_user.sql)
when written):

```sql
-- 0002_multi_user: introduce homelab users; rebind v1 users -> accounts.
BEGIN;

-- 1. Rename v1 users -> accounts.
ALTER TABLE users RENAME TO accounts_old;

-- 2. Create v2 users (homelab tenants).
CREATE TABLE users (
    id            TEXT    PRIMARY KEY,
    email         TEXT    NOT NULL UNIQUE,
    password_hash BLOB    NOT NULL,
    role          TEXT    NOT NULL CHECK (role IN ('admin','user','readonly')),
    invited_by    TEXT             REFERENCES users(id) ON DELETE SET NULL,
    created_at    INTEGER NOT NULL,
    disabled_at   INTEGER
);
CREATE INDEX idx_users_email ON users(email);

-- 3. Create v2 accounts shape with NOT NULL user_id and unique-per-user
--    resy_email. (SQLite can't ALTER PK in place; we rebuild.)
CREATE TABLE accounts (
    id            TEXT    PRIMARY KEY,                         -- new acct_… id
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    resy_email    TEXT    NOT NULL,                            -- v1's id
    display_name  TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    disabled_at   INTEGER,
    UNIQUE (user_id, resy_email)
);
CREATE INDEX idx_accounts_user ON accounts(user_id);

-- 4. Seed the operator user.  The Go-side migration runner reads
--    OPERATOR_EMAIL + OPERATOR_PASSWORD (env or interactive prompt),
--    argon2id-hashes, and INSERTs a single row with role='admin',
--    invited_by=NULL.  If users already has rows, this step is skipped.
--    (SQL placeholder shown; bound by Go.)
INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
VALUES (?, ?, ?, 'admin', NULL, ?);

-- 5. Mint acct_… ids and bind every existing account to the operator.
--    (Go-side: for each row in accounts_old, generate an acct_… id,
--    INSERT into accounts with user_id = the operator's id,
--    resy_email = old id, display_name = 'imported',
--    created_at = unix_millis(old created_at).)

-- 6. Sessions: rebind user_id (resy_email) to account_id.
ALTER TABLE sessions RENAME TO sessions_old;
CREATE TABLE sessions (
    account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    provider    TEXT NOT NULL,
    jwt         TEXT NOT NULL,
    exp         TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (account_id, provider)
);
INSERT INTO sessions (account_id, provider, jwt, exp, created_at, updated_at)
SELECT a.id, s.provider, s.jwt, s.exp, s.created_at, s.updated_at
FROM sessions_old s
JOIN accounts a ON a.resy_email = s.user_id;
DROP TABLE sessions_old;
DROP TABLE accounts_old;
CREATE INDEX idx_sessions_account ON sessions(account_id);

-- 7. Create v2 tables: invites, tokens, secrets, quests, intents,
--    runs, quest_events, audit_events. (DDL from §2.)

-- 8. Constrain at most one operator (NULL invited_by) user.
CREATE UNIQUE INDEX idx_users_one_operator ON users(invited_by)
   WHERE invited_by IS NULL;

COMMIT;
```

**Idempotence.** `IF NOT EXISTS` on every `CREATE`; operator
seed is skipped when `SELECT count(*) FROM users` > 0. Re-running
on a migrated DB is a no-op.

**Operator seeding.** First start of the v2 binary detects an
empty `users` table and prompts for `OPERATOR_EMAIL` +
`OPERATOR_PASSWORD` (env or interactive). If neither is
supplied, the daemon refuses to serve and prints instructions.

**Tested.** See §16.1.

## 15. Cross-user account sharing — deferred

Not in v1. Notes captured here so a future ADR doesn't have to
re-derive the design.

**Two viable shapes.**

1. **`accounts.shared_with` JSON column.** A JSON array of
   `usr_…` IDs. Cheapest schema change. Hard to enforce at the
   DB. Best when sharing is rare.
2. **`account_grants` join table** `(account_id, user_id, role,
   granted_at, granted_by)`. Cleaner enforcement, audit-friendly,
   supports per-grant role. Adds a JOIN to every account-bearing
   query. Best when sharing becomes common.

**Why deferred.** Rate-limiting nondeterminism — Resy's
per-account rate limit is shared, so two users hitting one
`acct_X` contend on a single bucket the planner can't reason
about cleanly. Audit murk — "who booked Carbone?" today the
actor equals the account owner; sharing splits them and forces
a third column or a convention.

**Operator workaround.** If two users genuinely need a shared
account, register the same Resy email under both users (each
with their own `acct_…` row and sealed password). They drift
independently; rate-limit contention happens at the *Resy*
layer, not ours. Not great, but it's the v1 escape hatch. A
future ADR picks one shape when there's a concrete request.

## 16. Test plan

Every claim above has a test. Failing tests block merge.

### 16.1 Schema migration tests

`internal/store/migrations_test.go`. v1 fixture: empty DB,
apply `0001_initial.sql`, populate with two Resy logins
(`phall@…`, `james@…`) each with a session, plus snipes,
events, observed releases. Apply `0002_multi_user.sql` with
`OPERATOR_EMAIL=phall@…`. Assert: `users` has one
`role=admin, invited_by IS NULL` row; `accounts` has two rows
bound to phall with `resy_email` preserved; `sessions` joined
to `account_id` with `jwt` preserved; `idx_users_one_operator`
rejects a second NULL-inviter insert; all v2 tables exist.
Idempotence: re-apply, unchanged. No-data start: apply v1 then
v2 to an empty DB, operator seeding still runs.

### 16.2 Tenancy enforcement tests

`internal/store/tenant_check.go`, build tag `tenancy`. See §10.
A new Store method without `userID` and not on the allowlist
fails with a pointer to this doc.

### 16.3 Concurrent-user load test

`internal/service/service_concurrent_test.go`. Seed `usr_a`,
`usr_b`, each with one account. Two goroutines each create 50
quests via `Service.CreateQuest` in parallel. Assert: each
user's `ListQuests` returns exactly their 50; cross-user queries
return zero rows; `audit_events` shows 100 `ok=1` rows; no
deadlocks; under 2s on dev hardware. Bonus goroutine: admin
reads audit cross-user, sees both users' events.

### 16.4 Invite/accept round-trip

`internal/service/invite_test.go`. `InviteUser` returns token;
`AcceptInvite(token, password)` returns `(userID, bearerToken)`.
Assert: `users` has the new row; `invites.consumed_at` set;
`tokens` has one row for the new user; re-using the token
returns `ErrInviteConsumed`; `auth.login` with the new
password works; audit chain `user.invite` + `user.accept` +
`auth.login` recorded. Negative cases: expired, wrong-hash,
double-accept.

### 16.5 Authorization matrix

`internal/service/authz_matrix_test.go`. Table-driven `(role,
method, expected)`:

| role     | method                | expected           |
|----------|-----------------------|--------------------|
| admin    | InviteUser            | ok                 |
| user     | InviteUser            | ErrForbidden       |
| readonly | InviteUser            | ErrForbidden       |
| admin    | DisableUser           | ok                 |
| user     | DisableUser           | ErrForbidden       |
| admin    | ListUsers             | ok                 |
| user     | ListUsers             | ErrForbidden       |
| admin    | CreateQuest (own)     | ok                 |
| user     | CreateQuest (own)     | ok                 |
| readonly | CreateQuest (own)     | ErrForbidden       |
| user     | CreateQuest (other's) | ErrForbidden       |
| readonly | ListQuests (own)      | ok                 |
| readonly | CancelQuest (own)     | ErrForbidden       |
| admin    | ReadAudit (other)     | ok                 |
| user     | ReadAudit (other)     | ErrForbidden       |

Adding a Service method requires adding rows for all three
roles; the test fails if any role is missing.

### 16.6 Token lifecycle

Issue returns secret once; hash stored. Verify with secret =
ok; `last_used_at` updated eventually. Verify with revoked,
expired, disabled-user, wrong-secret = 401. Sanity check that
the call site uses `subtle.ConstantTimeCompare`.

### 16.7 Sealed-secrets round-trip

Cross-doc with [design/secrets.md](secrets.md). Tests
`secrets.Seal` + `secrets.Open` round-trip, AAD mismatch fails,
swapping a row's `account_id` fails Open, `version` lets old
rows verify with old params.

### 16.8 Audit-write coverage

`internal/audit/coverage_test.go`. Reflects over `Service`,
calls each public method (with stub Store/engine), asserts
exactly one `audit_events` row written with the §12 action
name. New Service method without an audit action = test fails.
New audit action not listed in §12 = test fails.

These eight groups are the contract. Pass them, the model is
sound.
