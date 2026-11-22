CREATE TABLE spaces (
    id TEXT NOT NULL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE CHECK (length(name) > 0),
    cluster_id TEXT NOT NULL REFERENCES clusters(id),
    key_hash TEXT NOT NULL UNIQUE
) STRICT;

PRAGMA user_version = 7;
