-- 0004_tokens_reshape: align the tokens table with the M2 bearer-auth
-- contract in docs/v2/design/daemon.md §4.2.
--
-- The 0002 schema modelled tokens as `token_hash` BLOB PRIMARY KEY
-- with a singular `scope` column drawn from a {'api','mcp','cli'}
-- vocabulary, plus an `expires_at` field. The M2 design pins a
-- different shape:
--
--   * a public ULID `id` (so DELETE /v1/auth/tokens/{id} has a stable
--     non-secret handle in the URL path),
--   * an opaque `hash` column carrying BLAKE2b(token); lookups by hash
--     are unique but `id` is the primary key,
--   * `scopes` ∈ {'user','operator'} — the role tier the daemon
--     enforces at the Service layer (see design/multi-user.md),
--   * `last_seen` (renamed from `last_used_at`) — coalesced ~1Hz by
--     the auth middleware,
--   * no `expires_at` — operator tokens are long-lived; revocation is
--     the cancellation mechanism. A later milestone may reintroduce
--     expiry under a different column.
--
-- M2 has not shipped any real tokens yet, so the migration drops the
-- 0002 table and re-creates the table fresh. Forward-only, single
-- transaction (the Migrate runner wraps every migration).

DROP TABLE IF EXISTS tokens;

CREATE TABLE tokens (
    id          TEXT    PRIMARY KEY,                                  -- ULID; public handle for DELETE /v1/auth/tokens/{id}
    user_id     TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    hash        BLOB    NOT NULL,                                     -- BLAKE2b-256(token); plaintext never stored
    scopes      TEXT    NOT NULL CHECK (scopes IN ('user','operator')),
    label       TEXT    NOT NULL,                                     -- operator-set, e.g. "phall-laptop"
    created_at  INTEGER NOT NULL,                                     -- unix seconds
    last_seen   INTEGER,                                              -- unix seconds; NULL until first auth
    revoked_at  INTEGER                                                -- unix seconds; NULL while live
);

-- Auth middleware looks up tokens by hash. Uniqueness is a contract
-- guarantee: two issuances must not collide on hash, which would
-- happen only if 32 bytes of crypto/rand collided — practically
-- impossible but we let the database enforce it.
CREATE UNIQUE INDEX idx_tokens_hash ON tokens(hash);

-- ListTokens(user_id) scans this index.
CREATE INDEX idx_tokens_user ON tokens(user_id, created_at);
