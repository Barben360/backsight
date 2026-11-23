-- Metrics index schema, migration 0008: where a shown memory came from.
-- Additive: every row of the earlier tables is kept. The source column
-- (a journal item's `source`: the memory's source words, sorted, joined
-- by "+", such as note+session) is added to shows and recalls; a row
-- ingested before has NULL, as a line written before the field has no
-- source, so an index migrated from 0007 equals a fresh build. Types as
-- 0001's header.

ALTER TABLE shows ADD COLUMN source TEXT;
ALTER TABLE recalls ADD COLUMN source TEXT;
