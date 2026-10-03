-- Crush's own schema, taken verbatim from a live crush.db (charmbracelet/crush,
-- migration 20250424200609_initial plus the summary_message_id / todos /
-- provider / is_summary_message additions) with `sqlite3 .schema`. Structure is
-- real, including the comments that claim milliseconds while every writer emits
-- seconds, and including the triggers this adapter must never fire.
--
-- sqlite_sequence is omitted: SQLite creates it on demand and refuses a manual
-- CREATE. Nothing here reads it.

CREATE TABLE goose_db_version (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		version_id INTEGER NOT NULL,
		is_applied INTEGER NOT NULL,
		tstamp TIMESTAMP DEFAULT (datetime('now'))
	);
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    parent_session_id TEXT,
    title TEXT NOT NULL,
    message_count INTEGER NOT NULL DEFAULT 0 CHECK (message_count >= 0),
    prompt_tokens  INTEGER NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens  INTEGER NOT NULL DEFAULT 0 CHECK (completion_tokens>= 0),
    cost REAL NOT NULL DEFAULT 0.0 CHECK (cost >= 0.0),
    updated_at INTEGER NOT NULL,  -- Unix timestamp in milliseconds
    created_at INTEGER NOT NULL   -- Unix timestamp in milliseconds
, summary_message_id TEXT, todos TEXT);
CREATE TRIGGER update_sessions_updated_at
AFTER UPDATE ON sessions
BEGIN
UPDATE sessions SET updated_at = strftime('%s', 'now')
WHERE id = new.id;
END;
CREATE TABLE files (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    path TEXT NOT NULL,
    content TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,  -- Unix timestamp in milliseconds
    updated_at INTEGER NOT NULL,  -- Unix timestamp in milliseconds
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
    UNIQUE(path, session_id, version)
);
CREATE INDEX idx_files_session_id ON files (session_id);
CREATE INDEX idx_files_path ON files (path);
CREATE TRIGGER update_files_updated_at
AFTER UPDATE ON files
BEGIN
UPDATE files SET updated_at = strftime('%s', 'now')
WHERE id = new.id;
END;
CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    role TEXT NOT NULL,
    parts TEXT NOT NULL default '[]',
    model TEXT,
    created_at INTEGER NOT NULL,  -- Unix timestamp in milliseconds
    updated_at INTEGER NOT NULL,  -- Unix timestamp in milliseconds
    finished_at INTEGER, provider TEXT, is_summary_message INTEGER DEFAULT 0 NOT NULL,  -- Unix timestamp in milliseconds
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE
);
CREATE INDEX idx_messages_session_id ON messages (session_id);
CREATE TRIGGER update_messages_updated_at
AFTER UPDATE ON messages
BEGIN
UPDATE messages SET updated_at = strftime('%s', 'now')
WHERE id = new.id;
END;
CREATE TRIGGER update_session_message_count_on_insert
AFTER INSERT ON messages
BEGIN
UPDATE sessions SET
    message_count = message_count + 1
WHERE id = new.session_id;
END;
CREATE TRIGGER update_session_message_count_on_delete
AFTER DELETE ON messages
BEGIN
UPDATE sessions SET
    message_count = message_count - 1
WHERE id = old.session_id;
END;
CREATE INDEX idx_sessions_created_at ON sessions (created_at);
CREATE INDEX idx_messages_created_at ON messages (created_at);
CREATE INDEX idx_files_created_at ON files (created_at);
CREATE TABLE read_files (
    session_id TEXT NOT NULL CHECK (session_id != ''),
    path TEXT NOT NULL CHECK (path != ''),
    read_at INTEGER NOT NULL,  -- Unix timestamp in seconds when file was last read
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
    PRIMARY KEY (path, session_id)
);
CREATE INDEX idx_read_files_session_id ON read_files (session_id);
CREATE INDEX idx_read_files_path ON read_files (path);
