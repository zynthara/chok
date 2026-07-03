-- 0001_notes.sql — the fixture's application table. In versioned mode
-- this file IS the schema authority: the columns mirror db.Model
-- (id/rid/version/timestamps) plus the fixture's own title column.
CREATE TABLE notes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    rid        VARCHAR(24) NOT NULL UNIQUE,
    version    INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP,
    updated_at TIMESTAMP,
    title      VARCHAR(200) NOT NULL DEFAULT ''
);

CREATE INDEX idx_notes_created_at ON notes (created_at);
