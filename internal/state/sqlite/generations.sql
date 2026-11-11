CREATE TABLE generations (
    scope TEXT NOT NULL,
    slot_id TEXT NOT NULL,
    id TEXT NOT NULL,
    operation_id TEXT NOT NULL REFERENCES operations(id),
    key_hash TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    intent BLOB NOT NULL CHECK (length(intent) > 0),
    PRIMARY KEY (scope, slot_id, id),
    UNIQUE (scope, id),
    UNIQUE (scope, slot_id, key_hash),
    UNIQUE (scope, slot_id, version),
    UNIQUE (scope, slot_id, operation_id)
) STRICT;

PRAGMA user_version = 3;
