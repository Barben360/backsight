-- Metrics index schema, migration 0007: two read indexes, and
-- document_status redefined over the columns it returns. No table or row
-- changes, and the view returns the same rows: an index migrated from 0006
-- equals a fresh build.
--
-- tel_prompt_calendar covers the days calendar's grouped read of 13 months
-- of prompts (time range, project filter, and the reason, decision,
-- injected and session it counts): the read stays in the index instead of
-- visiting each wide row of tel_prompt (WITHOUT ROWID). Other prompt reads
-- over a time range that need only these columns are covered too.
--
-- tel_retain_document holds tel_retain by document, latest event first
-- (the order of document_status's window, primary key last), with the
-- status and time taken each event carries. document_status's window
-- reads it in order, with no sort and no visit of the table's rows, once
-- its inner select names only the columns the view returns (0001's took
-- every column); the backlog age's per-document reads stay in it too.
CREATE INDEX tel_prompt_calendar ON tel_prompt(ts, project, reason, decision, injected, session);
CREATE INDEX tel_retain_document ON tel_retain(document, ts DESC, source_file DESC, "offset" DESC, status, ms);

DROP VIEW document_status;
CREATE VIEW document_status AS
  SELECT document, status, ts, ms
  FROM (SELECT document, status, ts, ms, ROW_NUMBER() OVER (PARTITION BY document ORDER BY ts DESC, source_file DESC, "offset" DESC) AS rn FROM tel_retain)
  WHERE rn = 1;
