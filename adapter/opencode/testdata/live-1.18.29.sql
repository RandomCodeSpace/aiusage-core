-- OpenCode writer 1.18.29; isolated data/config capture extracted 2026-09-12T16:23:52.171931+00:00.
-- Writer entry SHA256 before/after: ca6c0e1f42be3120595bf6848937e7586ec862c87fa7aa111e89c7cc6e9a4650
-- Capture limitation: inherited PWD selected the original workspace; one project metadata file was opened write-capable.
-- Original usage DB/config were not targeted; prior project-metadata bytes were not recorded.
-- Stable session/message/part/call IDs are unchanged; project paths are sanitized.
-- Text, reasoning content, tool arguments/results and credentials are omitted.
-- Counters, provider/model, tool name/status and timestamps are retained.
CREATE TABLE session (id TEXT PRIMARY KEY, version TEXT);
CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, data TEXT, time_created INTEGER, time_updated INTEGER);
CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, data TEXT, time_created INTEGER, time_updated INTEGER);
INSERT INTO session VALUES ('ses_f69974741ffetkJiV5sUeyaFHD', '1.18.29');
INSERT INTO message VALUES ('msg_09668bd14001q1BuXF6wDvraAx', 'ses_f69974741ffetkJiV5sUeyaFHD', '{"role":"assistant","modelID":"gemma4:31b-cloud","providerID":"capture","time":{"created":1789229841685,"completed":1789229847281},"tokens":{"total":2427,"input":540,"output":31,"reasoning":0,"cache":{"write":0,"read":1856}},"cost":0,"id":"msg_09668bd14001q1BuXF6wDvraAx","sessionID":"ses_f69974741ffetkJiV5sUeyaFHD","path":{"cwd":"/sample/project","root":"/sample"}}', 1789229841685, 1789229847284);
INSERT INTO part VALUES ('prt_09668d09c001BlpnFa4AaBIObH', 'msg_09668bd14001q1BuXF6wDvraAx', 'ses_f69974741ffetkJiV5sUeyaFHD', '{"id":"prt_09668d09c001BlpnFa4AaBIObH","callID":"call_hg526bt7","sessionID":"ses_f69974741ffetkJiV5sUeyaFHD","messageID":"msg_09668bd14001q1BuXF6wDvraAx","type":"tool","tool":"read","state":{"status":"error"}}', 1789229846685, 1789229846718);
-- second observed message
INSERT INTO message VALUES ('msg_09668d2fa001g0ysR4uWzbpKDF', 'ses_f69974741ffetkJiV5sUeyaFHD', '{"role":"assistant","modelID":"gemma4:31b-cloud","providerID":"capture","time":{"created":1789229847291,"completed":1789229853752},"tokens":{"total":2589,"input":204,"output":17,"reasoning":0,"cache":{"write":0,"read":2368}},"cost":0,"id":"msg_09668d2fa001g0ysR4uWzbpKDF","sessionID":"ses_f69974741ffetkJiV5sUeyaFHD","path":{"cwd":"/sample/project","root":"/sample"}}', 1789229847291, 1789229853754);
