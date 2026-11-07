-- Metrics index schema, migration 0003: the other PCs' daily figures
-- (<state>/metrics/machines.json, the documents fetched from the server),
-- re-parsed on ingest. Additive: every row of 0001's and 0002's tables is
-- kept. Nothing of a document is stored but its machine id, OS enum,
-- dates, integers and guarded binary versions. Times are unix ms unless
-- the column says ns; days are UTC dates (YYYY-MM-DD, TEXT).

-- The cache file as last ingested: the next catch-up reads it only when
-- its size or modification time differ.
CREATE TABLE pc_fetch (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  fetched_at INTEGER,
  size INTEGER,
  mtime INTEGER, -- unix ns
  sha256 TEXT,
  ingested_at INTEGER
);

-- One row per other PC whose document parsed.
CREATE TABLE pc_docs (
  machine TEXT PRIMARY KEY,
  os TEXT,
  first_seen TEXT,
  at INTEGER NOT NULL,
  binary_version TEXT,
  until_day TEXT
) WITHOUT ROWID;

-- One row per PC, day and version; version '' is the day's totals. noon is
-- the day's 12:00 UTC. flow_* are NULL on version rows (day level only).
CREATE TABLE pc_days (
  machine TEXT NOT NULL,
  day TEXT NOT NULL,
  version TEXT NOT NULL,
  noon INTEGER NOT NULL,
  prompts INTEGER NOT NULL,
  hit_n INTEGER NOT NULL,
  hit_d INTEGER NOT NULL,
  used_n INTEGER NOT NULL,
  used_d INTEGER NOT NULL,
  cov_n INTEGER NOT NULL,
  cov_d INTEGER NOT NULL,
  wrong_n INTEGER NOT NULL,
  wrong_d INTEGER NOT NULL,
  ctx_chars INTEGER NOT NULL,
  ctx_sessions INTEGER NOT NULL,
  flow_n INTEGER,
  flow_d INTEGER,
  lat0 INTEGER NOT NULL,
  lat1 INTEGER NOT NULL,
  lat2 INTEGER NOT NULL,
  lat3 INTEGER NOT NULL,
  lat4 INTEGER NOT NULL,
  lat5 INTEGER NOT NULL,
  lat6 INTEGER NOT NULL,
  lat7 INTEGER NOT NULL,
  lat8 INTEGER NOT NULL,
  PRIMARY KEY (machine, day, version)
) WITHOUT ROWID;
CREATE INDEX pc_days_version_noon ON pc_days(version, noon);

-- The documents that did not parse: the short id (or empty) and a reason
-- enum (too_large, unknown_version, not_valid), in cache order.
CREATE TABLE pc_ignored (
  pos INTEGER PRIMARY KEY,
  machine TEXT,
  reason TEXT
);
