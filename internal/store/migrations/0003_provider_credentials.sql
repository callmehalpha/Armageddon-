-- Optional Git-provider credentials (contract §7.5, plan M2.8): a PAT or a
-- server-generated SSH key per user and provider host. The secret is
-- encrypted with the server data key (keys/data.key); key_id names the key
-- that sealed it. public_key is the SSH public key (not secret) or ''.

CREATE TABLE provider_credentials (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id),
    host       TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('https-token', 'ssh-key')),
    username   TEXT NOT NULL DEFAULT '',
    public_key TEXT NOT NULL DEFAULT '',
    key_id     TEXT NOT NULL,
    ciphertext TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (user_id, host, kind)
);
