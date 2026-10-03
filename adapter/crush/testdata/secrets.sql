-- The live session's shape with a canary planted in EVERY content-bearing
-- column Crush writes, and a non-zero cost so the adapter really does emit a
-- row to inspect.
--
-- Poisoned: sessions.title (Crush generates it from the user's prompt),
-- sessions.todos, messages.parts (text, a tool call's name and input, a tool
-- result's content, a shell command and its output), files.path,
-- files.content, read_files.path.
--
-- NOT poisoned: ids, model and provider names, counters, timestamps — those are
-- names and numbers, and the adapter is allowed to carry them. The project path
-- is likewise left clean on purpose: model.UsageEvent.Project IS the workspace
-- path by definition, so planting the canary there would test the schema, not
-- the adapter.
--
-- Messages precede sessions for the trigger reason documented in live.sql.

INSERT INTO messages VALUES(
  'msg-secret-user',
  'sess-secret',
  'user',
  '[{"type":"text","data":{"text":"CANARY-a7f3c2e1-SECRET my private prompt"}}]',
  '',
  1786853157,1786853157,NULL,NULL,0);

INSERT INTO messages VALUES(
  'msg-secret-assistant',
  'sess-secret',
  'assistant',
  '[{"type":"text","data":{"text":"CANARY-a7f3c2e1-SECRET reply body"}},{"type":"tool_call","data":{"id":"tc-1","name":"CANARY-a7f3c2e1-SECRET","input":"{\"command\":\"CANARY-a7f3c2e1-SECRET\"}"}},{"type":"tool_result","data":{"tool_call_id":"tc-1","name":"bash","content":"CANARY-a7f3c2e1-SECRET output"}},{"type":"shell_command","data":{"command":"CANARY-a7f3c2e1-SECRET","output":"CANARY-a7f3c2e1-SECRET","exit_code":0}},{"type":"finish","data":{"reason":"end_turn","time":1786853158}}]',
  'claude-sonnet-4-5',
  1786853157,1786853158,1786853158,'anthropic',0);

INSERT INTO sessions VALUES(
  'sess-secret',
  NULL,
  'CANARY-a7f3c2e1-SECRET title from the prompt',
  2,
  15290,
  8,
  1.5,
  1786853158,
  1786853157,
  NULL,
  '[{"task":"CANARY-a7f3c2e1-SECRET todo"}]');

INSERT INTO files VALUES(
  'file-1',
  'sess-secret',
  '/home/someone/CANARY-a7f3c2e1-SECRET/main.go',
  'package main // CANARY-a7f3c2e1-SECRET',
  1,
  1786853157,1786853158);

INSERT INTO read_files VALUES(
  'sess-secret',
  '/home/someone/CANARY-a7f3c2e1-SECRET/notes.txt',
  1786853158);
