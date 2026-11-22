CREATE TABLE revisions (
    id TEXT NOT NULL PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    key_hash TEXT NOT NULL,
    manifest BLOB NOT NULL CHECK (length(manifest) > 0),
    UNIQUE (project_id, key_hash)
) STRICT;

PRAGMA user_version = 9;
