CREATE TABLE clusters (
    id TEXT NOT NULL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE CHECK (length(name) > 0),
    key_hash TEXT NOT NULL UNIQUE
) STRICT;

PRAGMA user_version = 6;
