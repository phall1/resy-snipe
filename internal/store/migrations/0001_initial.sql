-- 0001_initial: create the Phase 1 schema.

CREATE TABLE users (
    id         TEXT PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE sessions (
    user_id    TEXT NOT NULL,
    provider   TEXT NOT NULL,
    jwt        TEXT NOT NULL,
    exp        TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (user_id, provider),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE venues (
    provider     TEXT NOT NULL,
    ref          TEXT NOT NULL,
    name         TEXT,
    tz           TEXT NOT NULL,
    last_seen_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (provider, ref)
);

CREATE TABLE snipes (
    id            TEXT PRIMARY KEY,
    intent_hash   TEXT NOT NULL,
    intent_json   TEXT NOT NULL,
    status        TEXT NOT NULL,
    scheduled_at  TEXT,
    result_json   TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX idx_snipes_intent_hash ON snipes(intent_hash);
CREATE INDEX idx_snipes_status      ON snipes(status);

CREATE TABLE events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    snipe_id    TEXT NOT NULL,
    type        TEXT NOT NULL,
    at          TEXT NOT NULL,
    fields_json TEXT,
    FOREIGN KEY (snipe_id) REFERENCES snipes(id) ON DELETE CASCADE
);

CREATE INDEX idx_events_snipe_id ON events(snipe_id);

CREATE TABLE observed_release_times (
    provider            TEXT NOT NULL,
    venue_ref           TEXT NOT NULL,
    observed_local_time TEXT NOT NULL,
    days_offset         INTEGER NOT NULL,
    observed_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (provider, venue_ref)
);
