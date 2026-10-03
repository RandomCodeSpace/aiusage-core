-- Fixture for the Goose adapter: a scrubbed copy of a real
-- <data-dir>/sessions/sessions.db, loaded into a temp database by the tests.
--
-- The three CREATE TABLE statements are VERBATIM from a live goose 1.46.0
-- database (schema_version 16), including every column this adapter must never
-- read: sessions.total_tokens (assigned, not accumulated) and the whole
-- sessions.accumulated_* family (equal to SUM(usage_ledger) by construction).
-- They are here as bait, and they carry real numbers.
--
-- ROWS. Sessions 20260816_1 and 20260816_3 and ledger rows 1, 3 and 4 are REAL:
-- their counters, timestamps, model ids and ids are exactly what goose wrote on
-- 2026-08-16 while driving ollama. Everything a human or a model typed is
-- SCRUBBED — session names, prompts, replies, tool arguments and tool output are
-- synthetic placeholders, and several fields carry the marker
-- SCRUB-CANARY-<n> so the privacy test can plant a secret in every content
-- field a real database has.
--
-- The canary set covers every column of the three tables that can hold human or
-- model text — sessions.name/description/recipe_json/user_recipe_values_json/
-- extension_data, messages.content_json and messages.metadata_json — and every
-- CONTENT-BEARING BLOCK TYPE of content_json, not just the ones this adapter
-- looks at: text, thinking, image data, a tool request's arguments, a tool
-- confirmation prompt, the LLM-written `_meta` title, a failed call's error
-- string and a tool response's output. The block types the decode has no field
-- for are the point: they prove the allow-list drops them at the parse rather
-- than downstream of it.
--
-- The rows for sess_priced and gone_session are CONSTRUCTED: this machine's
-- providers report no cost and no cache tokens, so the priced, cached,
-- compaction, carried-forward and orphaned shapes are built from goose's own
-- writer (session_manager.rs insert_usage_ledger_row / record_usage_metrics and
-- token_usage.rs Usage) rather than observed here. Their SHAPES are sourced;
-- their numbers are made up and are labelled as such.

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

-- REAL session, type 'hidden' (goose's own internal session). Its name is
-- generated from the conversation, i.e. content: scrubbed.
INSERT INTO sessions (id, name, description, user_set_name, session_type, working_dir,
    created_at, updated_at, extension_data, total_tokens, input_tokens, output_tokens,
    accumulated_total_tokens, accumulated_input_tokens, accumulated_output_tokens,
    accumulated_cache_read_tokens, accumulated_cache_write_tokens, accumulated_cost,
    provider_name, model_config_json, goose_mode)
VALUES ('20260816_1', 'SCRUB-CANARY-1 session name', 'SCRUB-CANARY-2 description', 0, 'hidden',
    '/home/dev/projects/demo', '2026-08-16 04:05:14', '2026-08-16 04:05:16', '{}',
    7455, 7447, 8, 7455, 7447, 8, 0, 0, NULL,
    'ollama', '{"model_name":"gemma4:31b-cloud","context_limit":null}', 'auto');

-- REAL session: two ledger rows, sessions.total_tokens holding ONLY the last of
-- them (7503) while accumulated_total_tokens holds both (14984). The adapter
-- must reproduce 14984 from the ledger and must never read either column.
INSERT INTO sessions (id, name, description, user_set_name, session_type, working_dir,
    created_at, updated_at, extension_data, total_tokens, input_tokens, output_tokens,
    accumulated_total_tokens, accumulated_input_tokens, accumulated_output_tokens,
    accumulated_cache_read_tokens, accumulated_cache_write_tokens, accumulated_cost,
    recipe_json, user_recipe_values_json,
    provider_name, model_config_json, goose_mode)
VALUES ('20260816_3', 'SCRUB-CANARY-3 session name', '', 1, 'user',
    '/home/dev/projects/demo', '2026-08-16 04:25:20', '2026-08-16 04:25:21',
    '{"enabled_extensions.v0":{"extensions":[{"type":"platform","name":"developer","description":"SCRUB-CANARY-17 extension data"}]}}',
    7503, 7491, 12, 14984, 14957, 27, 0, 0, NULL,
    '{"instructions":"SCRUB-CANARY-4 recipe instructions"}',
    '{"note":"SCRUB-CANARY-5 recipe value"}',
    'ollama', '{"model_name":"gemma4:31b-cloud","context_limit":null}', 'auto');

-- CONSTRUCTED session for the priced / cached / compaction / carried-forward
-- shapes this machine's providers never produce.
INSERT INTO sessions (id, name, description, user_set_name, session_type, working_dir,
    created_at, updated_at, extension_data, total_tokens, input_tokens, output_tokens,
    accumulated_total_tokens, accumulated_input_tokens, accumulated_output_tokens,
    accumulated_cache_read_tokens, accumulated_cache_write_tokens, accumulated_cost,
    provider_name, model_config_json, goose_mode)
VALUES ('sess_priced', 'SCRUB-CANARY-6 session name', '', 1, 'user',
    '/home/dev/projects/priced', '2026-08-16 05:00:00', '2026-08-16 05:10:00', '{}',
    920, 900, 20, 13440, 12900, 540, 9000, 1500, 0.038,
    'anthropic', '{"model_name":"claude-sonnet-4-5","context_limit":null}', 'auto');

-- REAL ledger row: ollama, no cache, no cost, no cost_source.
INSERT INTO usage_ledger (id, session_id, created_timestamp, model, input_tokens,
    output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, cost,
    cost_source, is_compaction)
VALUES (1, '20260816_1', 1786853116, 'gemma4:31b', 7447, 8, 7455, NULL, NULL, NULL, NULL, 0);

-- REAL pair: two turns of one session written in the SAME second. Only the
-- rowid separates them, which is what the dedup key is built on.
INSERT INTO usage_ledger (id, session_id, created_timestamp, model, input_tokens,
    output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, cost,
    cost_source, is_compaction)
VALUES (3, '20260816_3', 1786854321, 'gemma4:31b', 7466, 15, 7481, NULL, NULL, NULL, NULL, 0);
INSERT INTO usage_ledger (id, session_id, created_timestamp, model, input_tokens,
    output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, cost,
    cost_source, is_compaction)
VALUES (4, '20260816_3', 1786854321, 'gemma4:31b', 7491, 12, 7503, NULL, NULL, NULL, NULL, 0);

-- CONSTRUCTED: cache-INCLUSIVE input, goose's normalisation for every provider.
-- input 12000 = 1500 fresh + 9000 cache read + 1500 cache write; total 12400 =
-- input + output, with the cache already inside. Cost is provider-reported.
INSERT INTO usage_ledger (id, session_id, created_timestamp, model, input_tokens,
    output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, cost,
    cost_source, is_compaction)
VALUES (5, 'sess_priced', 1786860000, 'claude-sonnet-4-5', 12000, 400, 12400, 9000, 1500,
    0.0365, 'provider_reported', 0);

-- CONSTRUCTED: a compaction turn. It spends real tokens and is ordinary usage.
INSERT INTO usage_ledger (id, session_id, created_timestamp, model, input_tokens,
    output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, cost,
    cost_source, is_compaction)
VALUES (6, 'sess_priced', 1786860100, 'claude-sonnet-4-5', 900, 120, 1020, NULL, NULL,
    0.0015, 'estimated', 1);

-- CONSTRUCTED: the gap filler. No model (goose's INSERT ... SELECT binds none),
-- cost NULL because accumulated_cost did not exceed the ledger's sum. It is the
-- difference between the accumulator and the ledger, so DROPPING it undercounts.
INSERT INTO usage_ledger (id, session_id, created_timestamp, model, input_tokens,
    output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, cost,
    cost_source, is_compaction)
VALUES (7, 'sess_priced', 1786860200, NULL, 20, 0, 20, 0, 0, NULL, 'carried_forward', 0);

-- CONSTRUCTED: a ledger row whose session row is gone. Still an observed fact;
-- it simply has no project and no provider.
INSERT INTO usage_ledger (id, session_id, created_timestamp, model, input_tokens,
    output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, cost,
    cost_source, is_compaction)
VALUES (8, 'gone_session', 1786860300, 'gpt-5', 300, 40, 340, NULL, NULL, NULL, NULL, 0);

-- CONSTRUCTED: an all-zero row. Nothing was spent, so nothing is emitted.
INSERT INTO usage_ledger (id, session_id, created_timestamp, model, input_tokens,
    output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, cost,
    cost_source, is_compaction)
VALUES (9, 'sess_priced', 1786860400, 'claude-sonnet-4-5', 0, 0, 0, 0, 0, NULL, NULL, 0);

-- REAL message sequence of session 20260816_3, structure verbatim, every piece
-- of text replaced. Note metadata_json: goose records per-message usage there
-- too. It is a SECOND usage surface and reading it alongside the ledger would
-- double count, so the adapter never touches it.
INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp, tokens, metadata_json)
VALUES (7, 'msg_11111111-1111-1111-1111-111111111111', '20260816_3', 'user',
    '[{"type":"text","text":"SCRUB-CANARY-7 user prompt"}]', 1786854320, NULL,
    '{"userVisible":true,"agentVisible":true}');

INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp, tokens, metadata_json)
VALUES (8, 'msg_22222222-2222-2222-2222-222222222222', '20260816_3', 'user',
    '[{"type":"text","text":"<turn-context>SCRUB-CANARY-8 turn context</turn-context>"}]',
    1786854320, NULL, '{"userVisible":false,"agentVisible":true,"turnContext":true}');

-- REAL tool call. The block shape, the id form, the toolCall envelope and the
-- _meta extension key are exactly as goose wrote them; the arguments are
-- scrubbed, and _meta carries the LLM-written title key a real record has.
INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp, tokens, metadata_json)
VALUES (9, 'chatcmpl-543', '20260816_3', 'assistant',
    '[{"type":"toolRequest","id":"call_srht34n0","toolCall":{"status":"success","value":{"name":"shell","arguments":{"command":"SCRUB-CANARY-9 argument"}}},"_meta":{"goose_extension":"developer","goose.toolSummary.title":"SCRUB-CANARY-10 generated title"}}]',
    1786854321, NULL,
    '{"userVisible":true,"agentVisible":true,"inference":{"provider":"ollama","requestedModel":"gemma4:31b-cloud"},"usage":{"inputTokens":7466,"outputTokens":15,"totalTokens":7481}}');

-- REAL tool response, on the FOLLOWING USER row. It holds the tool's output and
-- the adapter never reads a user row at all.
INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp, tokens, metadata_json)
VALUES (10, 'msg_33333333-3333-3333-3333-333333333333', '20260816_3', 'user',
    '[{"type":"toolResponse","id":"call_srht34n0","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"SCRUB-CANARY-11 command output"}]}}}]',
    1786854321, NULL, '{"userVisible":true,"agentVisible":true}');

INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp, tokens, metadata_json)
VALUES (11, 'chatcmpl-629', '20260816_3', 'assistant',
    '[{"type":"text","text":"SCRUB-CANARY-12 assistant reply"}]', 1786854321, NULL,
    '{"userVisible":true,"agentVisible":true,"usage":{"inputTokens":7491,"outputTokens":12,"totalTokens":7503}}');

-- CONSTRUCTED: one assistant turn holding THREE tool-call blocks — a plain call,
-- a call whose name already carries its extension prefix, and a failed call
-- that carries no name at all. Two rows come out of it, both saying the turn
-- had two calls.
--
-- It also carries the content-bearing block types this adapter has NO field for
-- — thinking, redactedThinking, image data and a tool confirmation prompt — plus
-- a canary inside metadata_json, the second usage surface no query selects.
-- None of them may reach an emitted row, and none of them may produce one:
-- callName gates on the block TYPE, so a confirmation request is not a call.
INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp, tokens, metadata_json)
VALUES (12, 'chatcmpl-900', 'sess_priced', 'assistant',
    '[{"type":"thinking","thinking":"SCRUB-CANARY-18 chain of thought","signature":"sig"},{"type":"redactedThinking","data":"SCRUB-CANARY-19 redacted"},{"type":"text","text":"SCRUB-CANARY-13 assistant preamble"},{"type":"toolRequest","id":"call_a","toolCall":{"status":"success","value":{"name":"text_editor","arguments":{"path":"SCRUB-CANARY-14 path"}}},"_meta":{"goose_extension":"developer","goose.toolChain.summary":"SCRUB-CANARY-22 chain summary"}},{"type":"toolRequest","id":"call_b","toolCall":{"status":"success","value":{"name":"todo__todo_write","arguments":{"todos":"SCRUB-CANARY-15 todo text"}}},"_meta":{"goose_extension":"todo"}},{"type":"toolRequest","id":"call_c","toolCall":{"status":"error","error":"SCRUB-CANARY-16 tool error"}},{"type":"image","data":"SCRUB-CANARY-20 base64 image bytes","mimeType":"image/png"},{"type":"toolConfirmationRequest","id":"call_d","toolName":"developer__shell","arguments":{"command":"SCRUB-CANARY-21 pending command"},"prompt":"SCRUB-CANARY-23 confirmation prompt"}]',
    1786860000, NULL,
    '{"userVisible":true,"agentVisible":true,"note":"SCRUB-CANARY-24 message metadata","usage":{"inputTokens":12000,"outputTokens":400,"totalTokens":12400}}');
