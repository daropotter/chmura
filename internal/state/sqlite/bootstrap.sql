CREATE TABLE bootstrap_admin (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    id TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL CHECK (length(name) > 0),
    key_hash BLOB NOT NULL CHECK (length(key_hash) = 32),
    token_hash BLOB NOT NULL CHECK (length(token_hash) = 32)
) STRICT;

PRAGMA user_version = 11;
