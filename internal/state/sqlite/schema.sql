CREATE TABLE operations (
    id TEXT NOT NULL PRIMARY KEY,
    scope TEXT NOT NULL,
    key_hash TEXT NOT NULL,
    kind TEXT NOT NULL,
    intent BLOB NOT NULL,
    UNIQUE (scope, key_hash)
) STRICT;

PRAGMA user_version = 1;
