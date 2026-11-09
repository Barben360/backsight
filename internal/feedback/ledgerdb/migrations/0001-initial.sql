CREATE TABLE memories (
  memory_id        TEXT PRIMARY KEY,
  shown            INTEGER NOT NULL DEFAULT 0,
  credits          REAL    NOT NULL DEFAULT 0,
  credits_at       INTEGER NOT NULL DEFAULT 0,  -- unix ns, 0 = zero time
  misses           REAL    NOT NULL DEFAULT 0,
  misses_at        INTEGER NOT NULL DEFAULT 0,
  last_shown       INTEGER NOT NULL DEFAULT 0,
  last_credited    INTEGER NOT NULL DEFAULT 0,
  wrong            INTEGER NOT NULL DEFAULT 0,
  hidden_since     INTEGER,                     -- NULL: not hidden
  hidden_verdict   TEXT    NOT NULL DEFAULT '',
  hidden_note      TEXT    NOT NULL DEFAULT '',
  unhidden_at      INTEGER NOT NULL DEFAULT 0,
  imported_shown   INTEGER NOT NULL DEFAULT 0,
  imported_credits REAL    NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX memories_hidden ON memories(hidden_since) WHERE hidden_since IS NOT NULL;
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE ledger_cursors (file TEXT PRIMARY KEY, offset INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE settled_keys (key TEXT PRIMARY KEY) WITHOUT ROWID;
