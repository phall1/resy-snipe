-- 0005_subscriptions: persistent subscription hunts.

CREATE TABLE subscriptions (
    id            TEXT    PRIMARY KEY,
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    goal_json     TEXT    NOT NULL,
    status        TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    expires_at    INTEGER,
    fulfilled_by  TEXT    REFERENCES quests(id) ON DELETE SET NULL,
    compromise_json TEXT,
    poll_interval INTEGER NOT NULL DEFAULT 90,
    next_poll_at  INTEGER NOT NULL,
    CHECK (status IN ('active','paused','fulfilled','expired','cancelled'))
);

CREATE INDEX idx_subscriptions_user_status
    ON subscriptions(user_id, status);

CREATE INDEX idx_subscriptions_status_next_poll
    ON subscriptions(status, next_poll_at)
    WHERE status = 'active';
