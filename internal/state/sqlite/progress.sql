ALTER TABLE operations ADD COLUMN status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'canceled'));
ALTER TABLE operations ADD COLUMN version INTEGER NOT NULL DEFAULT 1
    CHECK (version > 0);
ALTER TABLE operations ADD COLUMN result BLOB NOT NULL DEFAULT X''
    CHECK ((status IN ('pending', 'running') AND length(result) = 0)
        OR (status IN ('succeeded', 'failed', 'canceled') AND length(result) > 0));

CREATE TABLE operation_updates (
    operation_id TEXT NOT NULL REFERENCES operations(id),
    key_hash TEXT NOT NULL,
    expected_version INTEGER NOT NULL CHECK (expected_version > 0),
    status TEXT NOT NULL,
    result BLOB NOT NULL,
    PRIMARY KEY (operation_id, key_hash),
    UNIQUE (operation_id, expected_version)
) STRICT;

PRAGMA user_version = 2;
