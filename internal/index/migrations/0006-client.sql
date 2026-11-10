-- Metrics index schema, migration 0006: which coding tool a line comes from.
-- Additive: every row of the earlier tables is kept. The client column
-- (the `client` field of a telemetry or journal line: claude-code, codex)
-- is added to each table fed by one line of a telemetry or journal month
-- file; a row ingested before has 'claude-code', as a line written before
-- the field is read as Claude Code. Types as 0001's header.

ALTER TABLE tel_prompt ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_outcome ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_reply ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_briefing ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_briefing_outcome ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_tool ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_tool_use ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_credit ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_credit_check ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_stop_block ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_retire ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_feedback_settle ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_feedback_error ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_gate ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_retain ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_hook_failure ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_job ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_attribution ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_store ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_dashboard ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE tel_config ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE events ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE turns ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE show_sets ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE shows ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE settles ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE credits ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE stops ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE retirements ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE recalls ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE imports ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE journal_other ADD COLUMN client TEXT NOT NULL DEFAULT 'claude-code';
