-- The two tables of session-store.db that hold conversation text. Nothing in
-- internal/adapter/copilot reads either; the canary proves it.

INSERT INTO sessions (id, cwd, repository, host_type, branch, summary, created_at, updated_at)
VALUES ('11111111-2222-4333-8444-555555555555', 'CANARY-4f1d9b02-SECRET', 'CANARY-4f1d9b02-SECRET', 'local', 'main', 'CANARY-4f1d9b02-SECRET', '2026-08-15T08:54:23.000Z', '2026-08-15T18:01:21.000Z');

INSERT INTO turns (session_id, turn_index, user_message, assistant_response, timestamp)
VALUES ('11111111-2222-4333-8444-555555555555', 0, 'CANARY-4f1d9b02-SECRET', 'CANARY-4f1d9b02-SECRET', '2026-08-15T13:41:28.623Z');
INSERT INTO turns (session_id, turn_index, user_message, assistant_response, timestamp)
VALUES ('11111111-2222-4333-8444-555555555555', 1, 'CANARY-4f1d9b02-SECRET', 'CANARY-4f1d9b02-SECRET', '2026-08-15T13:41:28.623Z');
INSERT INTO turns (session_id, turn_index, user_message, assistant_response, timestamp)
VALUES ('11111111-2222-4333-8444-555555555555', 2, 'CANARY-4f1d9b02-SECRET', 'CANARY-4f1d9b02-SECRET', '2026-08-15T13:41:28.623Z');
