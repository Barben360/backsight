-- Metrics index schema, migration 0001. Derived from telemetry JSONL and the
-- feedback journal; no free text in any column. Times: unix ms UTC.

CREATE TABLE tel_prompt (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  n INTEGER,
  decision TEXT,
  reason TEXT,
  plen INTEGER,
  lang TEXT,
  above INTEGER,
  injected INTEGER,
  ids_n INTEGER,
  top REAL,
  ms INTEGER,
  nudged INTEGER,
  hinted INTEGER,
  detector TEXT,
  chars INTEGER,
  empty INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_prompt_ts ON tel_prompt(ts);

CREATE TABLE tel_outcome (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  n INTEGER,
  ids_n INTEGER,
  searched INTEGER,
  credited INTEGER,
  credit_source TEXT,
  contradicted INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_outcome_ts ON tel_outcome(ts);

CREATE TABLE tel_reply (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  n INTEGER,
  credit INTEGER,
  searched INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_reply_ts ON tel_reply(ts);

CREATE TABLE tel_briefing (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  ids_n INTEGER,
  chars INTEGER,
  source TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_briefing_ts ON tel_briefing(ts);

CREATE TABLE tel_briefing_outcome (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  used_n INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_briefing_outcome_ts ON tel_briefing_outcome(ts);

CREATE TABLE tel_tool (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  name TEXT,
  ms INTEGER,
  hits INTEGER,
  ok INTEGER,
  live INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_tool_ts ON tel_tool(ts);

CREATE TABLE tel_tool_use (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  n INTEGER,
  name TEXT,
  ids_n INTEGER,
  subagent INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_tool_use_ts ON tel_tool_use(ts);

CREATE TABLE tel_credit (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  verdict TEXT,
  ids_n INTEGER,
  ok INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_credit_ts ON tel_credit(ts);

CREATE TABLE tel_credit_check (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  outcome TEXT,
  shown INTEGER,
  flags TEXT,
  after_block INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_credit_check_ts ON tel_credit_check(ts);

CREATE TABLE tel_retire (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  action TEXT,
  ids_n INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_retire_ts ON tel_retire(ts);

CREATE TABLE tel_feedback_settle (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  shows INTEGER,
  credits INTEGER,
  misses INTEGER,
  unknown INTEGER,
  late INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_feedback_settle_ts ON tel_feedback_settle(ts);

CREATE TABLE tel_feedback_error (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  reason TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_feedback_error_ts ON tel_feedback_error(ts);

CREATE TABLE tel_gate (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  n INTEGER,
  decision TEXT,
  reason TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_gate_ts ON tel_gate(ts);

CREATE TABLE tel_retain (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  document TEXT,
  items INTEGER,
  status TEXT,
  ms INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_retain_ts ON tel_retain(ts);

CREATE TABLE tel_hook_failure (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  hook TEXT,
  kind TEXT,
  ms INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_hook_failure_ts ON tel_hook_failure(ts);

CREATE TABLE tel_job (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  name TEXT,
  status TEXT,
  ms INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_job_ts ON tel_job(ts);

CREATE TABLE tel_attribution (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  turn INTEGER,
  subagent INTEGER,
  base INTEGER,
  applied INTEGER,
  path_rule TEXT,
  source TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_attribution_ts ON tel_attribution(ts);

CREATE TABLE tel_store (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  result TEXT,
  kind TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_store_ts ON tel_store(ts);

CREATE TABLE tel_dashboard (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  action TEXT,
  ms INTEGER,
  pages INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_dashboard_ts ON tel_dashboard(ts);

CREATE TABLE tel_config (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  v INTEGER,
  sha256 TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX tel_config_ts ON tel_config(ts);

CREATE TABLE tel_ids (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  kind TEXT,
  pos INTEGER NOT NULL DEFAULT 0,
  memory_id TEXT NOT NULL,
  PRIMARY KEY (source_file, "offset", pos)
) WITHOUT ROWID;
CREATE INDEX tel_ids_ts ON tel_ids(ts);
CREATE INDEX tel_ids_memory_id ON tel_ids(memory_id);

CREATE TABLE tel_prompt_scopes (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  scope TEXT,
  ok INTEGER,
  ms INTEGER,
  raw INTEGER,
  PRIMARY KEY (source_file, "offset", scope)
) WITHOUT ROWID;
CREATE INDEX tel_prompt_scopes_ts ON tel_prompt_scopes(ts);

CREATE TABLE tel_config_values (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  key TEXT,
  value REAL,
  PRIMARY KEY (source_file, "offset", key)
) WITHOUT ROWID;
CREATE INDEX tel_config_values_ts ON tel_config_values(ts);

CREATE TABLE events (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  kind TEXT,
  session TEXT,
  project TEXT,
  binary_version TEXT,
  fields TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX events_ts ON events(ts);

CREATE TABLE turns (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  turn INTEGER,
  agent TEXT,
  project TEXT,
  n INTEGER,
  machine TEXT,
  binary_version TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX turns_ts ON turns(ts);
CREATE INDEX turns_session_turn ON turns(session, turn);

CREATE TABLE shows (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  pos INTEGER NOT NULL DEFAULT 0,
  ts INTEGER,
  session TEXT,
  key TEXT,
  turn INTEGER,
  agent TEXT,
  path TEXT,
  memory_id TEXT,
  rank INTEGER,
  reranker REAL,
  boost REAL,
  rank_without_boost INTEGER,
  score_without_boost REAL,
  project TEXT,
  PRIMARY KEY (source_file, "offset", pos)
) WITHOUT ROWID;
CREATE INDEX shows_ts ON shows(ts);
CREATE INDEX shows_memory_id ON shows(memory_id);
CREATE INDEX shows_session_turn ON shows(session, turn);

CREATE TABLE settles (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  pos INTEGER NOT NULL DEFAULT 0,
  key TEXT,
  memory_id TEXT,
  show_at INTEGER,
  outcome TEXT,
  settle_ts INTEGER,
  PRIMARY KEY (source_file, "offset", pos)
) WITHOUT ROWID;
CREATE INDEX settles_settle_ts ON settles(settle_ts);
CREATE INDEX settles_memory_id ON settles(memory_id);

CREATE TABLE credits (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  pos INTEGER NOT NULL DEFAULT 0,
  ts INTEGER,
  session TEXT,
  turn INTEGER,
  agent TEXT,
  project TEXT,
  memory_id TEXT,
  verdict TEXT,
  PRIMARY KEY (source_file, "offset", pos)
) WITHOUT ROWID;
CREATE INDEX credits_ts ON credits(ts);
CREATE INDEX credits_memory_id ON credits(memory_id);
CREATE INDEX credits_session_turn ON credits(session, turn);

CREATE TABLE stops (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  turn INTEGER,
  agent TEXT,
  outcome TEXT,
  stop_check TEXT,
  flags TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX stops_ts ON stops(ts);
CREATE INDEX stops_session_turn ON stops(session, turn);

CREATE TABLE retirements (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  pos INTEGER NOT NULL DEFAULT 0,
  ts INTEGER,
  session TEXT,
  kind TEXT,
  memory_id TEXT,
  verdict TEXT,
  outcome TEXT,
  note_id TEXT,
  PRIMARY KEY (source_file, "offset", pos)
) WITHOUT ROWID;
CREATE INDEX retirements_ts ON retirements(ts);
CREATE INDEX retirements_memory_id ON retirements(memory_id);

CREATE TABLE recalls (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  pos INTEGER NOT NULL DEFAULT 0,
  ts INTEGER,
  session TEXT,
  turn INTEGER,
  agent TEXT,
  path TEXT,
  memory_id TEXT,
  rank INTEGER,
  rank_without_boost INTEGER,
  reranker REAL,
  boost REAL,
  score_without_boost REAL,
  flags TEXT,
  PRIMARY KEY (source_file, "offset", pos)
) WITHOUT ROWID;
CREATE INDEX recalls_ts ON recalls(ts);
CREATE INDEX recalls_memory_id ON recalls(memory_id);

CREATE TABLE imports (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  pos INTEGER NOT NULL DEFAULT 0,
  ts INTEGER,
  source TEXT,
  key TEXT,
  memory_id TEXT,
  at INTEGER,
  outcome TEXT,
  rank INTEGER,
  PRIMARY KEY (source_file, "offset", pos)
) WITHOUT ROWID;
CREATE INDEX imports_ts ON imports(ts);
CREATE INDEX imports_memory_id ON imports(memory_id);

CREATE TABLE journal_other (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  ts INTEGER,
  session TEXT,
  kind TEXT,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;
CREATE INDEX journal_other_ts ON journal_other(ts);

CREATE TABLE ledger (
  memory_id TEXT PRIMARY KEY,
  shown INTEGER,
  credits REAL,
  credits_at INTEGER,
  misses REAL,
  misses_at INTEGER,
  last_shown INTEGER,
  last_credited INTEGER,
  wrong INTEGER,
  hidden_since INTEGER,
  unhidden_at INTEGER,
  imported_shown INTEGER,
  imported_credits REAL
);

CREATE TABLE local_status (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  ingested_at INTEGER,
  notes_queued INTEGER,
  notes_oldest_at INTEGER,
  quarantined INTEGER,
  quarantine_oldest_at INTEGER,
  sessions_waiting INTEGER,
  reach_last_ok INTEGER,
  reach_failing_since INTEGER,
  breaker_failures INTEGER,
  breaker_open_until INTEGER,
  backup_created_at INTEGER,
  backup_verified_at INTEGER,
  backup_ok INTEGER,
  update_checked_at INTEGER,
  update_newer INTEGER,
  update_check_failed INTEGER,
  backfill_sessions INTEGER,
  backfill_paused INTEGER
);

CREATE TABLE cursors (
  source_file TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('telemetry','journal')),
  size INTEGER,
  "offset" INTEGER,
  head_len INTEGER,
  head_sha256 TEXT,
  ingested_at INTEGER,
  last_ts INTEGER,
  missing INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE ingest_errors (
  source_file TEXT NOT NULL,
  "offset" INTEGER NOT NULL,
  reason TEXT NOT NULL,
  at INTEGER,
  PRIMARY KEY (source_file, "offset")
) WITHOUT ROWID;

CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT
);

CREATE VIEW turn_final_stop AS
  SELECT session, turn, agent, CASE outcome WHEN 'blocked' THEN 'undeclared' ELSE outcome END AS outcome, ts
  FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY session, turn, agent ORDER BY source_file DESC, "offset" DESC) AS rn FROM stops)
  WHERE rn = 1;

CREATE VIEW document_status AS
  SELECT document, status, ts, ms
  FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY document ORDER BY ts DESC, source_file DESC, "offset" DESC) AS rn FROM tel_retain)
  WHERE rn = 1;
