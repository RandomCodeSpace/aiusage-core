BEGIN;

CREATE TABLE schema_version (
    version INTEGER PRIMARY KEY,
    applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    user_set_name BOOLEAN DEFAULT FALSE,
    session_type TEXT NOT NULL DEFAULT 'user',
    working_dir TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    extension_data TEXT DEFAULT '{}',
    total_tokens INTEGER,
    input_tokens INTEGER,
    output_tokens INTEGER,
    cache_read_tokens INTEGER,
    cache_write_tokens INTEGER,
    accumulated_total_tokens INTEGER,
    accumulated_input_tokens INTEGER,
    accumulated_output_tokens INTEGER,
    accumulated_cache_read_tokens INTEGER,
    accumulated_cache_write_tokens INTEGER,
    accumulated_cost REAL,
    schedule_id TEXT,
    recipe_json TEXT,
    user_recipe_values_json TEXT,
    provider_name TEXT,
    model_config_json TEXT,
    goose_mode TEXT NOT NULL DEFAULT 'auto',
    archived_at TIMESTAMP,
    project_id TEXT,
    parent_session_id TEXT
);

CREATE TABLE usage_ledger (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    created_timestamp INTEGER NOT NULL,
    model TEXT,
    input_tokens INTEGER,
    output_tokens INTEGER,
    total_tokens INTEGER,
    cache_read_tokens INTEGER,
    cache_write_tokens INTEGER,
    cost REAL,
    cost_source TEXT,
    is_compaction INTEGER DEFAULT 0
);

CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id TEXT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    role TEXT NOT NULL,
    content_json TEXT NOT NULL,
    created_timestamp INTEGER NOT NULL,
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    tokens INTEGER,
    metadata_json TEXT
);

CREATE INDEX idx_sessions_updated ON sessions(updated_at DESC);
CREATE INDEX idx_sessions_type ON sessions(session_type);
CREATE INDEX idx_sessions_parent ON sessions(parent_session_id);
CREATE INDEX idx_usage_ledger_session ON usage_ledger(session_id);
CREATE INDEX idx_messages_session ON messages(session_id);
CREATE INDEX idx_messages_timestamp ON messages(timestamp);
CREATE INDEX idx_messages_message_id ON messages(message_id);
CREATE INDEX idx_messages_session_created ON messages(session_id, created_timestamp, id);

INSERT INTO schema_version (version, applied_at)
VALUES (16, '2026-08-31 15:10:23');

INSERT INTO sessions (
    id, name, description, user_set_name, session_type, working_dir,
    created_at, updated_at, extension_data,
    total_tokens, input_tokens, output_tokens,
    cache_read_tokens, cache_write_tokens,
    accumulated_total_tokens, accumulated_input_tokens,
    accumulated_output_tokens, accumulated_cache_read_tokens,
    accumulated_cache_write_tokens, accumulated_cost,
    provider_name, model_config_json, goose_mode
) VALUES (
    '20260831_1', '<redacted>', '', 0, 'user', '/workspace/capture',
    '2026-08-31 15:10:23', '2026-08-31 15:10:25', '{}',
    361, 359, 2,
    NULL, NULL,
    361, 359,
    2, 0,
    0, NULL,
    'ollama', '{}', 'auto'
);

INSERT INTO usage_ledger (
    id, session_id, created_timestamp, model,
    input_tokens, output_tokens, total_tokens,
    cache_read_tokens, cache_write_tokens,
    cost, cost_source, is_compaction
) VALUES (
    1, '20260831_1', 1788189025, 'gemma4:31b',
    359, 2, 361,
    NULL, NULL,
    NULL, NULL, 0
);

INSERT INTO messages (
    id, message_id, session_id, role, content_json,
    created_timestamp, timestamp, tokens, metadata_json
) VALUES
    (1, 'message-sanitized-1', '20260831_1', 'user',
     '[{"type":"text","text":"<redacted>"}]',
     1788189023, '2026-08-31 15:10:23', NULL, '{}'),
    (2, 'message-sanitized-2', '20260831_1', 'user',
     '[{"type":"text","text":"<redacted>"}]',
     1788189023, '2026-08-31 15:10:23', NULL, '{}'),
    (3, 'chatcmpl-sanitized', '20260831_1', 'assistant',
     '[{"type":"text","text":"<redacted>"}]',
     1788189025, '2026-08-31 15:10:25', NULL,
     '{"userVisible":true,"agentVisible":true,"inference":{"provider":"ollama","requestedModel":"gemma4:31b-cloud"}}');

COMMIT;
