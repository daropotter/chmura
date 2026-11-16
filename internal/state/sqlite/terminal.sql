CREATE TABLE process_terminals (
    scope TEXT NOT NULL,
    slot_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    key_hash TEXT NOT NULL,
    cause TEXT NOT NULL CHECK (cause IN ('runtime-exit', 'startup-timeout', 'unhealthy')),
    occurred_at TEXT NOT NULL,
    evidence BLOB NOT NULL CHECK (length(evidence) > 0),
    PRIMARY KEY (scope, slot_id, generation_id),
    FOREIGN KEY (scope, slot_id, generation_id) REFERENCES generation_processes(scope, slot_id, generation_id)
) STRICT;

PRAGMA user_version = 5;
