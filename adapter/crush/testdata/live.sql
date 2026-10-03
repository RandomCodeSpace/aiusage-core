-- Scrubbed capture of a REAL Crush session, taken from
-- /home/dev/harness-lab/crush/.crush/crush.db on 2026-08-16.
--
-- Every counter, id, timestamp, model and provider is the real recorded value.
-- Every CONTENT field is replaced with a synthetic placeholder: the session
-- title (which Crush generates from the user's prompt), and the text inside
-- messages.parts. Nothing in this package reads either.
--
-- This is the case the adapter must emit NOTHING for: 15290 assigned prompt
-- tokens, 8 assigned completion tokens, and cost 0.0 because the provider is a
-- local ollama model Crush's catalog cannot price. Unmeasured and unpriced is
-- not free.
--
-- Rows are inserted messages-first on purpose. Crush's real
-- update_session_message_count_on_insert trigger updates sessions on every
-- message insert, and update_sessions_updated_at then rewrites updated_at with
-- strftime('%s','now') — which would overwrite the real timestamps this fixture
-- exists to preserve. With no session row present yet, both triggers no-op and
-- the sessions row lands byte-for-byte as recorded.

INSERT INTO messages VALUES(
  '941524d3-01be-45db-ab46-f515aa7cf2ec',
  '9dcd37b6-c9cb-4c49-a571-9cbc4caca957',
  'user',
  '[{"type":"text","data":{"text":"PLACEHOLDER user prompt"}},{"type":"finish","data":{"reason":"stop","time":0}}]',
  '',
  1786853157,1786853157,NULL,NULL,0);

INSERT INTO messages VALUES(
  '330171a4-0878-45c9-aa96-ae43309b5685',
  '9dcd37b6-c9cb-4c49-a571-9cbc4caca957',
  'assistant',
  '[{"type":"text","data":{"text":"PLACEHOLDER assistant reply"}},{"type":"finish","data":{"reason":"end_turn","time":1786853158}}]',
  'gemma4:31b-cloud',
  1786853157,1786853158,1786853158,'ollama',0);

INSERT INTO sessions VALUES(
  '9dcd37b6-c9cb-4c49-a571-9cbc4caca957',
  NULL,
  'PLACEHOLDER session title',
  2,
  15290,
  8,
  0.0,
  1786853158,
  1786853157,
  NULL,
  NULL);
