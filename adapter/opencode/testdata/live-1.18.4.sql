-- Embedded session.version=1.18.4; exact per-message writer version unverified; extracted 2026-09-12T15:56:44.402703+00:00.
-- Capture host: Linux/x86_64; main database only, WAL excluded.
-- Main DB SHA256 before: 7afc82fd84575b3e9b501374fdf95cf2add7fdf952bda4c507944975241bfc30
-- Main DB SHA256 after:  7afc82fd84575b3e9b501374fdf95cf2add7fdf952bda4c507944975241bfc30
-- Project paths/tool names are sanitized. Stable session/message/part/call IDs are unchanged.
-- Usage, provider/model and event times are unchanged.
-- Prompts, text/reasoning content, arguments, results and operational metadata are omitted.
CREATE TABLE session (id TEXT PRIMARY KEY, version TEXT);
CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, data TEXT, time_created INTEGER, time_updated INTEGER);
CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, data TEXT, time_created INTEGER, time_updated INTEGER);
INSERT INTO session VALUES ('ses_06aa3d3c2ffe6RYymjXr7w8SZQ', '1.18.4');
INSERT INTO message VALUES ('msg_f956e63fd001h5S0I3jNKNPWZK', 'ses_06aa3d3c2ffe6RYymjXr7w8SZQ', '{"role":"assistant","modelID":"gpt-5.6-terra","providerID":"openai","time":{"created":1784918467581,"completed":1784918479305},"tokens":{"total":117658,"input":2959,"output":56,"reasoning":467,"cache":{"write":0,"read":114176}},"id":"msg_f956e63fd001h5S0I3jNKNPWZK","sessionID":"ses_06aa3d3c2ffe6RYymjXr7w8SZQ","path":{"cwd":"/sample/project","root":"/sample"},"cost":0}', 1784918467581, 1784918479305);
INSERT INTO part VALUES ('prt_f956e8f13001sqfOoc7J3eAjeh', 'msg_f956e63fd001h5S0I3jNKNPWZK', 'ses_06aa3d3c2ffe6RYymjXr7w8SZQ', '{"id":"prt_f956e8f13001sqfOoc7J3eAjeh","callID":"call_gTG1Boc4KOlGwio6IM68VA9F","sessionID":"ses_06aa3d3c2ffe6RYymjXr7w8SZQ","messageID":"msg_f956e63fd001h5S0I3jNKNPWZK","type":"tool","tool":"live-tool-1"}', 1784918478611, 1784918479224);
-- second observed message
INSERT INTO message VALUES ('msg_f956edde0001iABPXwtVvo21W3', 'ses_06aa3d3c2ffe6RYymjXr7w8SZQ', '{"role":"assistant","modelID":"gpt-5.6-terra","providerID":"openai","time":{"created":1784918498784,"completed":1784918504248},"tokens":{"total":120135,"input":1151,"output":120,"reasoning":80,"cache":{"write":0,"read":118784}},"id":"msg_f956edde0001iABPXwtVvo21W3","sessionID":"ses_06aa3d3c2ffe6RYymjXr7w8SZQ","path":{"cwd":"/sample/project","root":"/sample"},"cost":0}', 1784918498784, 1784918504248);
INSERT INTO part VALUES ('prt_f956eec980014YS2vTs3X79XE4', 'msg_f956edde0001iABPXwtVvo21W3', 'ses_06aa3d3c2ffe6RYymjXr7w8SZQ', '{"id":"prt_f956eec980014YS2vTs3X79XE4","callID":"call_SF3eZnfEQQw5D0jw2HJPMz3B","sessionID":"ses_06aa3d3c2ffe6RYymjXr7w8SZQ","messageID":"msg_f956edde0001iABPXwtVvo21W3","type":"tool","tool":"live-tool-2"}', 1784918502552, 1784918504088);
