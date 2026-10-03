-- GitHub Copilot CLI 1.0.80 session-store.db, taken verbatim from a live
-- ~/.copilot/session-store.db with `sqlite3 .schema`. `sessions` and `turns`
-- are here to be NEVER READ: `turns.user_message` / `turns.assistant_response`
-- and `sessions.summary` hold whole prompts and replies, and the fixture plants
-- the privacy canary in all three.

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    cwd TEXT,
    repository TEXT,
    host_type TEXT,
    branch TEXT,
    summary TEXT,
    created_at TEXT DEFAULT (datetime('now')),
    updated_at TEXT DEFAULT (datetime('now'))
    );

CREATE TABLE turns (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    turn_index INTEGER NOT NULL,
    user_message TEXT,
    assistant_response TEXT,
    timestamp TEXT DEFAULT (datetime('now')),
    UNIQUE(session_id, turn_index)
    );

CREATE TABLE assistant_usage_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    turn_index INTEGER,
    agent_id TEXT,
    parent_tool_call_id TEXT,
    model TEXT NOT NULL,
    input_tokens INTEGER,
    output_tokens INTEGER,
    cache_read_tokens INTEGER,
    cache_write_tokens INTEGER,
    reasoning_tokens INTEGER,
    total_nano_aiu INTEGER,
    request_multiplier REAL,
    duration_ms INTEGER,
    time_to_first_token_ms INTEGER,
    inter_token_latency_ms INTEGER,
    initiator TEXT,
    api_endpoint TEXT,
    reasoning_effort TEXT,
    finish_reason TEXT,
    content_filter_triggered INTEGER,
    token_details_json TEXT,
    created_at TEXT DEFAULT (datetime('now'))
    );
