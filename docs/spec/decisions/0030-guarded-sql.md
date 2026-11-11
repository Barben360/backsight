# 0030 — Guarded SQL is allowed only for rebuild and retag

- **Date**: 2026-10-24
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, after Sonnet reviews and experiments

## Decision
The binary talks to Hindsight through its HTTP API, except two repairs with no endpoint: `server rebuild-observations` (re-scope facts to `[["project:*"]]`, give untagged facts the transverse tag, then clear and re-consolidate) and `server retag` (C13 rule). Both run through `docker exec psql` under the maintenance lock, after a verified dump, dry-run by default, only on alembic revisions in an allow-list shipped with each release.

## Why
E12 shows the rebuild needs one UPDATE on stored scopes (scopes are stamped at retain time); `PATCH /memories` has no tags field (E8). Guarding by revision keeps the SQL from running against a schema it was not written for (Hindsight migrates at every start, C03).
