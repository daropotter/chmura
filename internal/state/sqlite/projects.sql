CREATE TABLE projects (
    id TEXT NOT NULL PRIMARY KEY,
    name TEXT NOT NULL CHECK (length(name) > 0),
    space_id TEXT NOT NULL REFERENCES spaces(id),
    key_hash TEXT NOT NULL,
    UNIQUE (space_id, name),
    UNIQUE (space_id, key_hash)
) STRICT;

PRAGMA user_version = 8;
