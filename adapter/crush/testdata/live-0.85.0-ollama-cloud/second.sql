-- Sanitized live Crush v0.85.0 Ollama Cloud capture. See capture.json.

-- Messages first: retain source session timestamps despite insert triggers.

INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at, finished_at, provider, is_summary_message) VALUES (
  'bb0dadc0-302e-4e89-9eb4-c99dc4445434',
  '142e73d2-10b5-4218-baa5-5d5015dc2bc7',
  'user',
  '[{"type":"text","data":{"text":"PLACEHOLDER captured content"}}]',
  '',
  1789287044,
  1789287044,
  NULL,
  NULL,
  0);

INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at, finished_at, provider, is_summary_message) VALUES (
  'ea5a281e-2e8b-4eb6-bc79-3f7ee6bbb231',
  '142e73d2-10b5-4218-baa5-5d5015dc2bc7',
  'assistant',
  '[{"type":"text","data":{"text":"PLACEHOLDER captured content"}}]',
  'gpt-oss:20b',
  1789287044,
  1789287045,
  1789287045,
  'ollama-cloud',
  0);

INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at, finished_at, provider, is_summary_message) VALUES (
  '4a5d651d-2eca-442b-9c83-bf39451f52a5',
  '142e73d2-10b5-4218-baa5-5d5015dc2bc7',
  'assistant',
  '[{"type":"text","data":{"text":"PLACEHOLDER captured content"}}]',
  'gpt-oss:20b',
  1789287085,
  1789287087,
  1789287087,
  'ollama-cloud',
  0);

INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at, finished_at, provider, is_summary_message) VALUES (
  'f67da944-78b1-47d5-9d87-d22b14ec46d0',
  '142e73d2-10b5-4218-baa5-5d5015dc2bc7',
  'user',
  '[{"type":"text","data":{"text":"PLACEHOLDER captured content"}}]',
  '',
  1789287085,
  1789287085,
  NULL,
  NULL,
  0);

INSERT INTO sessions (id, parent_session_id, title, message_count, prompt_tokens, completion_tokens, cost, updated_at, created_at, summary_message_id, todos) VALUES (
  '142e73d2-10b5-4218-baa5-5d5015dc2bc7',
  NULL,
  'PLACEHOLDER session title',
  4,
  10540,
  29,
  1.547010000000000139e-03,
  1789287087,
  1789287044,
  NULL,
  NULL);
