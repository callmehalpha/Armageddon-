-- Portable SQL subset (contract §8.1): TEXT ULIDs, INTEGER unix-ms timestamps,
-- JSON stored as TEXT, no AUTOINCREMENT, no triggers.

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'user')),
    created_at    INTEGER NOT NULL,
    disabled_at   INTEGER
);

CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id),
    token_hash   TEXT NOT NULL UNIQUE,
    csrf_token   TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    reauth_at    INTEGER NOT NULL
);

CREATE TABLE one_time_tokens (
    token_hash  TEXT PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('setup', 'invite')),
    role        TEXT NOT NULL DEFAULT 'user',
    created_by  TEXT,
    expires_at  INTEGER NOT NULL,
    consumed_at INTEGER
);

CREATE TABLE devices (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id),
    name         TEXT NOT NULL,
    platform     TEXT NOT NULL DEFAULT '',
    public_key   TEXT NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    revoked_at   INTEGER
);

CREATE TABLE device_pairings (
    id             TEXT PRIMARY KEY,
    user_code_hash TEXT NOT NULL UNIQUE,
    poll_secret_hash TEXT NOT NULL,
    public_key     TEXT NOT NULL,
    name           TEXT NOT NULL,
    platform       TEXT NOT NULL DEFAULT '',
    expires_at     INTEGER NOT NULL,
    approved_by    TEXT REFERENCES users(id),
    approved_at    INTEGER,
    device_id      TEXT REFERENCES devices(id)
);

CREATE TABLE auth_nonces (
    nonce      TEXT PRIMARY KEY,
    device_id  TEXT NOT NULL REFERENCES devices(id),
    expires_at INTEGER NOT NULL
);

CREATE TABLE device_tokens (
    token_hash TEXT PRIMARY KEY,
    device_id  TEXT NOT NULL REFERENCES devices(id),
    expires_at INTEGER NOT NULL
);

CREATE TABLE workspaces (
    id                    TEXT PRIMARY KEY,
    owner_id              TEXT NOT NULL REFERENCES users(id),
    name                  TEXT NOT NULL,
    slug                  TEXT NOT NULL,
    state                 TEXT NOT NULL,
    state_reason          TEXT NOT NULL DEFAULT '',
    source_kind           TEXT NOT NULL CHECK (source_kind IN ('empty', 'clone')),
    source_url            TEXT NOT NULL DEFAULT '',
    os_user               TEXT NOT NULL DEFAULT '',
    current_checkpoint_id TEXT,
    checkpoint_seq        INTEGER NOT NULL DEFAULT 0,
    row_version           INTEGER NOT NULL DEFAULT 0,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    deleted_at            INTEGER,
    UNIQUE (owner_id, slug)
);

CREATE TABLE workspace_members (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    user_id      TEXT NOT NULL REFERENCES users(id),
    role         TEXT NOT NULL CHECK (role IN ('owner', 'member')),
    added_by     TEXT,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (workspace_id, user_id)
);

CREATE TABLE leases (
    workspace_id     TEXT PRIMARY KEY REFERENCES workspaces(id),
    holder_kind      TEXT NOT NULL CHECK (holder_kind IN ('server', 'device')),
    holder_device_id TEXT,
    epoch            INTEGER NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('held', 'handoff', 'stale')),
    acquired_at      INTEGER NOT NULL,
    heartbeat_at     INTEGER NOT NULL
);

CREATE TABLE checkpoints (
    id               TEXT NOT NULL,      -- commit oid in checkpoints.git
    workspace_id     TEXT NOT NULL REFERENCES workspaces(id),
    seq              INTEGER NOT NULL,
    epoch            INTEGER NOT NULL,
    parent_id        TEXT,
    author_kind      TEXT NOT NULL,
    author_device_id TEXT,
    head_ref         TEXT NOT NULL DEFAULT '',
    head_oid         TEXT NOT NULL DEFAULT '',
    worktree_tree    TEXT NOT NULL,
    kind             TEXT NOT NULL,
    created_at       INTEGER NOT NULL,
    PRIMARY KEY (workspace_id, id),
    UNIQUE (workspace_id, seq)
);

CREATE TABLE quarantines (
    id               TEXT PRIMARY KEY,
    workspace_id     TEXT NOT NULL REFERENCES workspaces(id),
    source_kind      TEXT NOT NULL,
    source_device_id TEXT,
    checkpoint_id    TEXT NOT NULL,
    base_checkpoint_id TEXT,
    reason           TEXT NOT NULL,
    created_at       INTEGER NOT NULL,
    resolved_at      INTEGER
);

CREATE TABLE events (
    id           TEXT PRIMARY KEY,
    ts           INTEGER NOT NULL,
    workspace_id TEXT,
    actor_kind   TEXT NOT NULL,
    actor_id     TEXT NOT NULL DEFAULT '',
    type         TEXT NOT NULL,
    payload      TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX events_ws_ts ON events (workspace_id, ts);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
