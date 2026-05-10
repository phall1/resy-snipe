-- 0002_v2_multi_user: introduce homelab users + every v2 tenant-scoped
-- table; rebind v1 Resy logins (`users`) -> `accounts` and v1
-- `sessions(user_id=resy_email)` -> `sessions(account_id)`.
--
-- Spec: docs/v2/design/multi-user.md §schema and §migration-plan.
-- Tenancy contract: docs/v2/design/multi-user.md §tenancy-enforcement.
-- Sealed-secrets shape: docs/v2/design/secrets.md (this file owns the
-- storage DDL, the seal protocol is owned by internal/secrets).
--
-- ADR-0005 (multi-user from M1), ADR-0010 (one daemon, many users),
-- ADR-0011 (operator-issued invites), ADR-0008 (sealed-at-rest secrets),
-- ADR-0006 (SQLite-only).
--
-- Conventions:
--   * Opaque IDs (`usr_…`, `acct_…`, `q_…`, `int_…`, `run_…`) match
--     domain.UserID, domain.AccountID, etc. Prefixes differ per type so
--     a typo never crosses tables silently.
--   * v2 timestamps are INTEGER unix millis. v1's `sessions` keeps
--     ISO8601 TEXT to preserve preserved-rows verbatim across the
--     rename (design/multi-user.md §sessions).
--   * Every user-data row carries `user_id` (denormalized where it is
--     implied by another FK — design §tenancy-enforcement requires
--     every Store query filter on `user_id` without a join).
--   * Operator seeding (the first `users` row, role=admin,
--     invited_by IS NULL) and v1 data backfill (mint acct_… ids,
--     bind to operator, replay sessions) are M1-16's job. This
--     migration leaves `accounts_old` / `sessions_old` in place for
--     that follow-up — wait, it doesn't: it does the rebind here so a
--     v1 row survives the upgrade end-to-end. `accounts.user_id` is
--     NULLABLE for now precisely because no operator exists at
--     migration time; M1-16 backfills it.
--
-- The Go-side migration runner (internal/store/migrate.go) wraps this
-- file in a single BEGIN/COMMIT and records the version row in
-- schema_migrations on success; partial application is impossible.

-----------------------------------------------------------------------
-- 1. Rebind v1 `users` (Resy logins) -> v2 `accounts`.
-----------------------------------------------------------------------

-- Step 1a: park the v1 table out of the way. With foreign_keys=ON,
-- modern SQLite rewrites FK references in dependent tables (sessions)
-- to follow the rename.
ALTER TABLE users RENAME TO accounts_old;

-----------------------------------------------------------------------
-- 2. v2 users (homelab tenants).
-----------------------------------------------------------------------

CREATE TABLE users (
    id            TEXT    PRIMARY KEY,                         -- 'usr_8xK3aZ' (domain.UserID)
    email         TEXT    NOT NULL UNIQUE,
    password_hash BLOB    NOT NULL,                            -- argon2id PHC-encoded
    role          TEXT    NOT NULL CHECK (role IN ('admin','user','readonly')),
    invited_by    TEXT             REFERENCES users(id) ON DELETE SET NULL,
    created_at    INTEGER NOT NULL,                            -- unix millis
    disabled_at   INTEGER                                      -- soft delete; NULL = active
);

CREATE INDEX idx_users_email ON users(email);

-- At most one operator (NULL invited_by). Partial unique index per
-- design §users.detail. We index a *constant* over the WHERE-
-- predicate subset so the uniqueness collision fires regardless of
-- SQLite's "NULLs are distinct" handling on a column index (which
-- would let multiple NULL-inviter rows slip through).
CREATE UNIQUE INDEX idx_users_one_operator ON users((1))
    WHERE invited_by IS NULL;

-----------------------------------------------------------------------
-- 3. v2 accounts (Resy logins owned by a user).
-----------------------------------------------------------------------

-- `user_id` is NULLABLE here, deliberately: M1-16 (operator seeding +
-- v1 data adoption) is what mints the operator `usr_…` and backfills
-- this column for any rows carried over from `accounts_old`. The
-- NOT NULL invariant is then enforced by the Service layer (every
-- account-bearing Store method requires a userID). See task notes in
-- M1-14 and design/multi-user.md §migration-plan step 4-5.
CREATE TABLE accounts (
    id            TEXT    PRIMARY KEY,                         -- 'acct_2pQ7vL' (domain.AccountID)
    user_id       TEXT             REFERENCES users(id) ON DELETE CASCADE,
    resy_email    TEXT    NOT NULL,                            -- v1's id; unique per user
    display_name  TEXT    NOT NULL,                            -- operator-set label, e.g. 'imported'
    created_at    INTEGER NOT NULL,                            -- unix millis
    disabled_at   INTEGER,
    UNIQUE (user_id, resy_email)
);

CREATE INDEX idx_accounts_user ON accounts(user_id);

-- Carry every v1 row forward. We mint a fresh acct_… id from
-- randomblob (SQLite built-in; pure-Go modernc driver supports it).
-- `user_id` stays NULL until M1-16 backfills the operator binding.
-- `created_at` is converted from v1's ISO8601 TEXT to unix millis;
-- strftime('%s', ts) returns seconds-since-epoch, so we * 1000.
INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
SELECT
    'acct_' || lower(substr(hex(randomblob(4)), 1, 8)),
    NULL,
    id,
    'imported',
    CAST(strftime('%s', created_at) AS INTEGER) * 1000
FROM accounts_old;

-----------------------------------------------------------------------
-- 4. Rebind v1 `sessions(user_id=resy_email)` -> v2 `sessions(account_id)`.
-----------------------------------------------------------------------

ALTER TABLE sessions RENAME TO sessions_old;

CREATE TABLE sessions (
    account_id  TEXT    NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    provider    TEXT    NOT NULL,                              -- 'resy'
    jwt         TEXT    NOT NULL,                              -- plaintext in v1; sealed in M2 (design/secrets.md)
    exp         TEXT    NOT NULL,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (account_id, provider)
);

CREATE INDEX idx_sessions_account ON sessions(account_id);

-- Replay sessions, joining old user_id (resy_email) to the freshly
-- minted accounts.id via accounts.resy_email.
INSERT INTO sessions (account_id, provider, jwt, exp, created_at, updated_at)
SELECT a.id, s.provider, s.jwt, s.exp, s.created_at, s.updated_at
FROM sessions_old s
JOIN accounts a ON a.resy_email = s.user_id;

DROP TABLE sessions_old;
DROP TABLE accounts_old;

-----------------------------------------------------------------------
-- 5. tokens (bearer auth; many per user).
-----------------------------------------------------------------------

CREATE TABLE tokens (
    token_hash    BLOB    PRIMARY KEY,                         -- SHA-256(token); raw token never stored
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scope         TEXT    NOT NULL,                            -- 'api' | 'mcp' | 'cli'
    label         TEXT    NOT NULL,
    last_used_at  INTEGER,                                     -- coalesced ~1Hz; NULL until first use
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER,                                     -- NULL = no expiry
    revoked_at    INTEGER
);

CREATE INDEX idx_tokens_user ON tokens(user_id);

-----------------------------------------------------------------------
-- 6. invites (ADR-0011: operator-issued, single-use, hashed at rest).
-----------------------------------------------------------------------

CREATE TABLE invites (
    token_hash   BLOB    PRIMARY KEY,                          -- SHA-256(token)
    email        TEXT    NOT NULL,                             -- not unique; multiple outstanding allowed
    role         TEXT    NOT NULL CHECK (role IN ('admin','user','readonly')),
    invited_by   TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    consumed_at  INTEGER                                       -- NULL = open; set on accept
);

CREATE INDEX idx_invites_email   ON invites(email);
CREATE INDEX idx_invites_expires ON invites(expires_at);

-----------------------------------------------------------------------
-- 7. secrets + secrets_meta (sealed-at-rest credential store).
--
-- Storage shape only. The KDF/AEAD seal protocol is owned by
-- internal/secrets and design/secrets.md (ADR-0008).
-----------------------------------------------------------------------

CREATE TABLE secrets (
    user_id     TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id  TEXT    NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL,                              -- 'resy_password' | 'resy_jwt' | 'signer_state'
    ciphertext  BLOB    NOT NULL,
    nonce       BLOB    NOT NULL,                              -- 12 bytes; per-row, never reused (AES-GCM)
    version     INTEGER NOT NULL,                              -- KDF/AEAD scheme version
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (user_id, account_id, kind)
);

CREATE INDEX idx_secrets_account ON secrets(account_id);

-- secrets_meta: exactly one row, ever. Holds the KDF salt bound to
-- this data.db. design/secrets.md §schema. The row is INSERTed by
-- M1-16 (alongside operator seeding) since `kdf_salt = randomblob(32)`
-- belongs to the seal init, not the schema.
CREATE TABLE secrets_meta (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    kdf_salt        BLOB    NOT NULL,
    active_version  INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL
);

-----------------------------------------------------------------------
-- 8. quests + intents + runs + quest_events (the goal-driven core).
-----------------------------------------------------------------------

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

CREATE INDEX idx_quests_user_status  ON quests(user_id, status);
CREATE INDEX idx_quests_user_created ON quests(user_id, created_at);
CREATE INDEX idx_quests_account      ON quests(account_id);

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
CREATE INDEX idx_intents_hash  ON intents(intent_hash);

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

CREATE INDEX idx_runs_quest        ON runs(quest_id);
CREATE INDEX idx_runs_user_started ON runs(user_id, started_at);

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
CREATE INDEX idx_quest_events_user  ON quest_events(user_id, at);

-----------------------------------------------------------------------
-- 9. audit_events (one row per Service call, success or failure).
--
-- target_user_id is ON DELETE SET NULL so the audit trail survives the
-- subject's hard delete. user_id (actor) cascades — if an actor is
-- purged, their audit trail goes with them (design/multi-user.md §8).
-----------------------------------------------------------------------

CREATE TABLE audit_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,   -- actor
    target_user_id  TEXT             REFERENCES users(id) ON DELETE SET NULL,  -- subject; may equal actor; NULL on cross-user listings
    action          TEXT    NOT NULL,                                          -- stable string; see design §audit-log-conventions
    target_id       TEXT,                                                      -- 'q_…' | 'acct_…' | 'usr_…' | NULL
    ok              INTEGER NOT NULL CHECK (ok IN (0, 1)),
    error_code      TEXT,                                                      -- domain.ErrCode on !ok
    ip              TEXT,                                                      -- caller IP if HTTP
    user_agent      TEXT,                                                      -- caller UA if HTTP
    created_at      INTEGER NOT NULL,
    details_json    TEXT                                                       -- <=4KiB, enforced at write time
);

CREATE INDEX idx_audit_actor  ON audit_events(user_id, created_at);
CREATE INDEX idx_audit_target ON audit_events(target_user_id, created_at);
CREATE INDEX idx_audit_action ON audit_events(action, created_at);

-----------------------------------------------------------------------
-- 10. Resolver caches (cross-tenant, no user_id — public Resy data).
--
-- design/resolver.md §caching-contract. TTL is enforced at the Go
-- layer; SQLite stores `resolved_at` as unix seconds.
-----------------------------------------------------------------------

CREATE TABLE venues_cache (
    slug         TEXT    NOT NULL,
    city         TEXT    NOT NULL,
    venue_json   TEXT    NOT NULL,                             -- serialised domain.Venue
    resolved_at  INTEGER NOT NULL,                             -- unix seconds
    PRIMARY KEY (slug, city)
);

CREATE TABLE name_search_cache (
    query        TEXT    PRIMARY KEY,
    results_json TEXT    NOT NULL,                             -- serialised []domain.Venue
    resolved_at  INTEGER NOT NULL
);
