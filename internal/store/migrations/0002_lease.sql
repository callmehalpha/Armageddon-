-- Local write mode (plan M7): lease handoff, replica status, quarantine
-- resolution (contract §3.2, §3.3, §6.7, §8.2).
ALTER TABLE leases ADD COLUMN handoff_to TEXT;                 -- 'server' or 'device:<id>' while state = 'handoff'
ALTER TABLE leases ADD COLUMN handoff_deadline INTEGER NOT NULL DEFAULT 0;

ALTER TABLE quarantines ADD COLUMN resolution TEXT;            -- 'dropped' | 'applied' once resolved
CREATE INDEX quarantines_ws_dev ON quarantines (workspace_id, source_device_id);

CREATE TABLE replicas (
    workspace_id     TEXT NOT NULL REFERENCES workspaces(id),
    device_id        TEXT NOT NULL REFERENCES devices(id),
    state            TEXT NOT NULL,
    mode             TEXT NOT NULL CHECK (mode IN ('follow', 'write')),
    last_applied_seq INTEGER NOT NULL DEFAULT 0,
    pending          INTEGER NOT NULL DEFAULT 0,
    last_seen_at     INTEGER NOT NULL,
    agent_version    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (workspace_id, device_id)
);
