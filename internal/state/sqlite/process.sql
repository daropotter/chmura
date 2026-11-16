CREATE TABLE generation_processes (
    scope TEXT NOT NULL,
    slot_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    identity BLOB NOT NULL CHECK (length(identity) > 0),
    identity_hash TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ever_ready INTEGER NOT NULL CHECK (ever_ready IN (0, 1)),
    PRIMARY KEY (scope, slot_id, generation_id),
    UNIQUE (scope, identity_hash),
    FOREIGN KEY (scope, slot_id, generation_id) REFERENCES generations(scope, slot_id, id)
) STRICT;

CREATE TABLE process_observations (
    scope TEXT NOT NULL,
    slot_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    key_hash TEXT NOT NULL,
    ever_ready INTEGER NOT NULL CHECK (ever_ready IN (0, 1)),
    PRIMARY KEY (scope, slot_id, generation_id, key_hash),
    FOREIGN KEY (scope, slot_id, generation_id) REFERENCES generation_processes(scope, slot_id, generation_id)
) STRICT;

PRAGMA user_version = 4;
