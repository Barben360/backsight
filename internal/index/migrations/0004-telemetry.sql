-- Metrics index schema, migration 0004: what the efficiency audit could
-- not measure. Additive: every row of the earlier tables is kept; a line
-- ingested before has NULL in the new columns, as a line written before
-- the fields does. Types as 0001's header.

-- tel_tool: a memory_search's queries and how many of them had none of
-- their hits among those returned (not "found nothing": a query's hits
-- may all rank below the others'), and every tool's answer size.
ALTER TABLE tel_tool ADD COLUMN queries_n INTEGER;
ALTER TABLE tel_tool ADD COLUMN queries_none_returned INTEGER;
ALTER TABLE tel_tool ADD COLUMN result_chars INTEGER;

-- One row per end-of-turn block, written when the blocked turn ended
-- again: what the agent then declared (used, confirmed, corrected, none)
-- or refused, the paths shown when it blocked (sorted, comma-joined), how
-- many memories it listed, and whether a subagent was asked.
CREATE TABLE tel_stop_block (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v TEXT,
  outcome TEXT,
  paths TEXT,
  shown INTEGER,
  subagent INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_stop_block_ts ON tel_stop_block(ts);
