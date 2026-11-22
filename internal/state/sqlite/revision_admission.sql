CREATE UNIQUE INDEX revisions_project_identity ON revisions(project_id, id);

CREATE TABLE revision_selections (
    project_id TEXT NOT NULL REFERENCES projects(id),
    version INTEGER NOT NULL CHECK (version > 0),
    revision_id TEXT NOT NULL,
    base_revision_id TEXT NOT NULL,
    operation_id TEXT NOT NULL REFERENCES operations(id),
    key_hash TEXT NOT NULL,
    PRIMARY KEY (project_id, version),
    UNIQUE (project_id, key_hash),
    UNIQUE (project_id, operation_id),
    CHECK ((version = 1 AND base_revision_id = '') OR (version > 1 AND length(base_revision_id) > 0)),
    FOREIGN KEY (project_id, revision_id) REFERENCES revisions(project_id, id)
) STRICT;

PRAGMA user_version = 10;
