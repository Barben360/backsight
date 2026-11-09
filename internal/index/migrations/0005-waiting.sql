-- Metrics index schema, migration 0005: which sessions wait. Additive: the
-- local_status row gains the ids of the sessions jobs tick last counted as
-- waiting, a JSON array of session ids (ids only, sorted); NULL when the
-- tick that wrote reach.json did not record them (an earlier version).
ALTER TABLE local_status ADD COLUMN waiting_sessions TEXT;
