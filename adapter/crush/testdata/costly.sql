-- Synthetic sessions on Crush's REAL schema, one per behaviour the adapter has
-- to get right. No content is real and none is needed: every field this fixture
-- exercises is a counter, an id, a model name or a timestamp.
--
-- Rows are inserted messages-first, for the trigger reason documented in
-- live.sql: inserting a message updates its session, and Crush's own
-- update_sessions_updated_at would then overwrite the timestamps asserted here.
--
--   sess-paid-single  root, one model, cost 0.0123456       -> 12346 micro-USD
--   sess-parent       root, cost 2.5 (INCLUDES the child)   -> 2500000
--   sess-child        parent=sess-parent, cost 0.5          -> nothing (already in the parent)
--   sess-multi        root, two (provider,model) pairs      -> 1000000, provider unknown
--   sess-zero         root, 15290 assigned tokens, cost 0.0 -> nothing
--   sess-subcent      root, cost 0.0000004                  -> nothing (rounds below one micro-USD)
--   sess-nomodel      root, no messages at all              -> 250000, provider unknown
--   sess-ms           root, MILLISECOND timestamps          -> 1000
--   sess-orphan       parent row missing, still a child     -> nothing
--   sess-gp           root, cost 3.0                        -> 3000000, provider unknown
--   sess-gp-child     parent=sess-gp                        -> nothing
--   sess-gp-grand     parent=sess-gp-child, different model -> nothing, but its model
--                                                              reaches sess-gp

INSERT INTO messages VALUES('m-single-1','sess-paid-single','assistant','[]','claude-sonnet-4-5',1786800100,1786800100,1786800100,'anthropic',0);

INSERT INTO messages VALUES('m-parent-1','sess-parent','assistant','[]','claude-sonnet-4-5',1786810200,1786810200,1786810200,'anthropic',0);
INSERT INTO messages VALUES('m-child-1','sess-child','assistant','[]','claude-sonnet-4-5',1786810300,1786810300,1786810300,'anthropic',0);

INSERT INTO messages VALUES('m-multi-1','sess-multi','assistant','[]','claude-sonnet-4-5',1786820100,1786820100,1786820100,'anthropic',0);
INSERT INTO messages VALUES('m-multi-2','sess-multi','assistant','[]','gpt-5',1786820200,1786820200,1786820200,'openai',0);

INSERT INTO messages VALUES('m-zero-1','sess-zero','assistant','[]','gemma4:31b-cloud',1786830050,1786830050,1786830050,'ollama',0);
INSERT INTO messages VALUES('m-subcent-1','sess-subcent','assistant','[]','claude-haiku-4-5',1786840050,1786840050,1786840050,'anthropic',0);
INSERT INTO messages VALUES('m-ms-1','sess-ms','assistant','[]','claude-sonnet-4-5',1786860100000,1786860100000,1786860100000,'anthropic',0);

INSERT INTO messages VALUES('m-gp-1','sess-gp','assistant','[]','claude-sonnet-4-5',1786880050,1786880050,1786880050,'anthropic',0);
INSERT INTO messages VALUES('m-gp-grand-1','sess-gp-grand','assistant','[]','gpt-5',1786880250,1786880250,1786880250,'openai',0);

-- A user message names no model; it must not count towards the pair set.
INSERT INTO messages VALUES('m-single-0','sess-paid-single','user','[]','',1786800050,1786800050,NULL,NULL,0);

INSERT INTO sessions VALUES('sess-paid-single',NULL,'t',2,190000,512,0.0123456,1786800600,1786800000,NULL,NULL);
INSERT INTO sessions VALUES('sess-parent',NULL,'t',4,90000,100,2.5,1786810900,1786810000,NULL,NULL);
INSERT INTO sessions VALUES('sess-child','sess-parent','t',2,40000,60,0.5,1786810800,1786810100,NULL,NULL);
INSERT INTO sessions VALUES('sess-multi',NULL,'t',4,50000,80,1.0,1786820500,1786820000,NULL,NULL);
INSERT INTO sessions VALUES('sess-zero',NULL,'t',2,15290,8,0.0,1786830100,1786830000,NULL,NULL);
INSERT INTO sessions VALUES('sess-subcent',NULL,'t',2,1000,10,0.0000004,1786840100,1786840000,NULL,NULL);
INSERT INTO sessions VALUES('sess-nomodel',NULL,'t',0,0,0,0.25,1786850100,1786850000,NULL,NULL);
INSERT INTO sessions VALUES('sess-ms',NULL,'t',2,3000,40,0.001,1786860600000,1786860000000,NULL,NULL);
INSERT INTO sessions VALUES('sess-orphan','sess-missing','t',2,7000,30,0.75,1786870100,1786870000,NULL,NULL);
INSERT INTO sessions VALUES('sess-gp',NULL,'t',6,120000,300,3.0,1786880900,1786880000,NULL,NULL);
INSERT INTO sessions VALUES('sess-gp-child','sess-gp','t',4,60000,150,1.2,1786880800,1786880100,NULL,NULL);
INSERT INTO sessions VALUES('sess-gp-grand','sess-gp-child','t',2,30000,70,0.4,1786880700,1786880200,NULL,NULL);
