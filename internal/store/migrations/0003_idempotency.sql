-- 0003_idempotency: Service-layer idempotency keys for CreateQuest /
-- CancelQuest (and any future replayable verb).
--
-- Spec: docs/v2/design/service-layer.md §idempotency.
-- M1-12 wires CreateQuest / CancelQuest to consult this table on the
-- way in and persist the outcome on the way out. Retention: 24h.
--
-- Storage shape:
--   * key_hash is sha256(userID + ":" + scope + ":" + plaintextKey +
--     ":" + payloadHash). Hashing the (scope, userID, payload) into
--     the primary key means the same plaintext key cannot collide
--     across scopes, across users, or across mismatched inputs —
--     a mismatched-input replay surfaces as a primary-key miss, which
--     the Service then disambiguates from "never seen this key" via a
--     secondary lookup keyed only on (userID, scope, plaintextKey).
--   * target_id captures the questID created/canceled so a replay
--     returns the exact prior outcome.
--   * result_code is 'ok' | 'error'; result_err carries the service
--     sentinel name on error replays so the Service can return the
--     same wrapped sentinel a fresh call would have.
--   * expires_at is when the row stops being authoritative; reads
--     past that threshold treat the entry as not-found and the
--     caller proceeds fresh.

CREATE TABLE idempotency_keys (
    key_hash    BLOB    PRIMARY KEY,                              -- sha256(userID + ':' + scope + ':' + plaintextKey + ':' + payloadHash)
    user_id     TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scope       TEXT    NOT NULL,                                 -- 'create_quest' | 'cancel_quest'
    plain_hash  BLOB    NOT NULL,                                 -- sha256(userID + ':' + scope + ':' + plaintextKey); detects key reuse with different payload
    target_id   TEXT,                                             -- the questID created/canceled; nullable on error replays without a target
    result_code TEXT    NOT NULL CHECK (result_code IN ('ok','error')),
    result_err  TEXT,                                             -- service sentinel name on error replays
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at  TEXT    NOT NULL
);

-- Secondary lookup: same (userID, scope, plaintextKey) with a
-- mismatched payload still finds the prior row, which lets the
-- Service surface ErrIdempotencyConflict instead of silently treating
-- the call as fresh.
CREATE INDEX idx_idempotency_user_scope_plain ON idempotency_keys(user_id, scope, plain_hash);

-- Pruning index for the daemon's periodic sweep.
CREATE INDEX idx_idempotency_expires_at ON idempotency_keys(expires_at);
