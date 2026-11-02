-- Metrics index schema, migration 0002: the local collection records
-- (<state>/metrics/<family>-YYYY-MM.jsonl) and one show_sets row per
-- journal shows line. Additive: every row of 0001's tables is kept.
-- Types as 0001's header; days (YYYY-MM-DD, UTC or local as the family
-- says) are TEXT; bucket, a UTC day start, is unix ms. A value the line
-- did not give is NULL, never 0.

-- cursors gains the four kinds of the collection families. SQLite cannot
-- change a CHECK in place: the table is copied whole into its new shape.
ALTER TABLE cursors RENAME TO cursors_0001;
CREATE TABLE cursors (
  source_file TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('telemetry','journal','server_daily','doctor_history','memory_docs','scan_history')),
  size INTEGER,
  "offset" INTEGER,
  head_len INTEGER,
  head_sha256 TEXT,
  ingested_at INTEGER,
  last_ts INTEGER,
  missing INTEGER NOT NULL DEFAULT 0
);
INSERT INTO cursors(source_file, kind, size, "offset", head_len, head_sha256, ingested_at, last_ts, missing)
  SELECT source_file, kind, size, "offset", head_len, head_sha256, ingested_at, last_ts, missing FROM cursors_0001;
DROP TABLE cursors_0001;

-- One row per journal shows line (its items are shows rows): the shown
-- set's flags, set_changed when the boost changed which memories.
CREATE TABLE show_sets (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  session TEXT,
  turn TEXT,
  agent TEXT,
  path TEXT,
  flags TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX show_sets_ts ON show_sets(ts);

-- server-daily snapshot: the server's totals on a local day.
CREATE TABLE server_snapshots (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  day TEXT,
  ok INTEGER,
  error TEXT,
  world INTEGER,
  experience INTEGER,
  observation INTEGER,
  documents INTEGER,
  links INTEGER,
  observations INTEGER,
  ops_pending INTEGER,
  ops_processing INTEGER,
  ops_completed INTEGER,
  ops_failed INTEGER,
  ops_other INTEGER,
  consolidation_pending INTEGER,
  consolidation_failed INTEGER,
  last_consolidated_at INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX server_snapshots_day ON server_snapshots(day);

CREATE TABLE server_snapshot_projects (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  day TEXT,
  project TEXT NOT NULL,
  memories INTEGER,
  PRIMARY KEY (source_file, "offset", project)
) WITHOUT ROWID;
CREATE INDEX server_snapshot_projects_day ON server_snapshot_projects(day);

-- server-daily day: one past full UTC day the server held.
CREATE TABLE server_days (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  bucket INTEGER NOT NULL,
  world INTEGER,
  experience INTEGER,
  observation INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX server_days_bucket ON server_days(bucket);

CREATE TABLE server_day_llm (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  bucket INTEGER,
  operation TEXT NOT NULL,
  input INTEGER,
  output INTEGER,
  cached INTEGER,
  thoughts INTEGER,
  requests INTEGER,
  PRIMARY KEY (source_file, "offset", operation)
) WITHOUT ROWID;
CREATE INDEX server_day_llm_bucket ON server_day_llm(bucket);

CREATE TABLE server_day_llm_status (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  bucket INTEGER,
  operation TEXT NOT NULL,
  status TEXT NOT NULL,
  n INTEGER,
  PRIMARY KEY (source_file, "offset", operation, status)
) WITHOUT ROWID;
CREATE INDEX server_day_llm_status_bucket ON server_day_llm_status(bucket);

-- doctor-history run: one row per check.
CREATE TABLE doctor_runs (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  check_id TEXT NOT NULL,
  status TEXT,
  PRIMARY KEY (source_file, "offset", check_id)
) WITHOUT ROWID;
CREATE INDEX doctor_runs_ts ON doctor_runs(ts);

-- memory-docs memory: a memory of a listed day; document and session only
-- for a session's document.
CREATE TABLE memory_docs (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  memory_id TEXT,
  outcome TEXT,
  doc_kind TEXT,
  document TEXT,
  session TEXT,
  created_day TEXT,
  fact_type TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX memory_docs_memory_id ON memory_docs(memory_id);
CREATE INDEX memory_docs_document ON memory_docs(document);
CREATE INDEX memory_docs_session ON memory_docs(session);
CREATE INDEX memory_docs_created_day ON memory_docs(created_day);

-- memory-docs listed: a UTC day's list read in full; complete 1 when the
-- server held nothing created on or before it.
CREATE TABLE memory_days (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  day TEXT NOT NULL,
  n INTEGER,
  complete INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX memory_days_day ON memory_days(day);

-- memory-docs op: a retain operation of a session's document.
CREATE TABLE doc_ops (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  operation_id TEXT,
  document TEXT,
  session TEXT,
  status TEXT,
  created_at INTEGER,
  updated_at INTEGER,
  retries INTEGER,
  units INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX doc_ops_operation_id ON doc_ops(operation_id);
CREATE INDEX doc_ops_document ON doc_ops(document);

-- scan-history scan: a complete secrets scan's counts.
CREATE TABLE scans (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  count INTEGER,
  live INTEGER,
  forgotten INTEGER,
  pages INTEGER,
  ms INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX scans_ts ON scans(ts);

-- A line of a known family that this binary does not read: a newer schema
-- or an unknown kind (a newer binary's), kept aside, never an error.
CREATE TABLE metrics_other (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  family TEXT,
  kind TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX metrics_other_ts ON metrics_other(ts);

-- The latest line of a memory, by ts then line: a memory listed on two
-- days is the later line's, which is not always the later day.
CREATE VIEW memory_doc AS
  SELECT memory_id, outcome, doc_kind, document, session, created_day, fact_type, ts
  FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY memory_id ORDER BY ts DESC, source_file DESC, "offset" DESC) AS rn
        FROM memory_docs WHERE memory_id IS NOT NULL)
  WHERE rn = 1;

-- The latest listed line of each day.
CREATE VIEW memory_day AS
  SELECT day, n, complete, ts
  FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY day ORDER BY ts DESC, source_file DESC, "offset" DESC) AS rn FROM memory_days)
  WHERE rn = 1;

-- One row: complete_day, the latest day listed complete (NULL: the record
-- does not cover the bank yet); covered_until, the first day on or after
-- it whose next day is not listed (NULL when not complete).
CREATE VIEW memory_coverage AS
  WITH RECURSIVE
    c(complete_day) AS (SELECT max(day) FROM memory_day WHERE complete = 1),
    walk(day) AS (
      SELECT complete_day FROM c WHERE complete_day IS NOT NULL
      UNION ALL
      SELECT date(walk.day, '+1 day') FROM walk
      WHERE EXISTS (SELECT 1 FROM memory_days m WHERE m.day = date(walk.day, '+1 day'))
    )
  SELECT (SELECT complete_day FROM c) AS complete_day, (SELECT max(day) FROM walk) AS covered_until;

-- The latest line of each retain operation.
CREATE VIEW doc_op AS
  SELECT operation_id, document, session, status, created_at, updated_at, retries, units, ts
  FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY operation_id ORDER BY ts DESC, source_file DESC, "offset" DESC) AS rn
        FROM doc_ops WHERE operation_id IS NOT NULL)
  WHERE rn = 1;

-- The latest day line of each UTC bucket; its llm rows share its
-- (source_file, "offset").
CREATE VIEW server_day AS
  SELECT bucket, world, experience, observation, ts, source_file, "offset"
  FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY bucket ORDER BY ts DESC, source_file DESC, "offset" DESC) AS rn FROM server_days)
  WHERE rn = 1;
