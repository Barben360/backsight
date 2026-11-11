# E8 — Project briefing at SessionStart (no LLM)

Date: 2026-10-24. Live bank (read only), projects `ref-stack` and `my-garage`. **V** = observed.

Two deterministic candidates:
- **A** — the 5 most recently updated observations of the project: `GET /memories/list?type=observation&tags=project:<p>&tags_match=any_strict&time_field=updated_at&limit=5` (one call, no reranker).
- **B** — a recall whose query is the git state (branch + last 6 commit subjects), project + transverse scopes, score ≥ 0.3.

| Project | A | B |
|---|---|---|
| my-garage | **V** exactly "where we left off": the 2026-10-23 incident, the new rule it produced (`preserveResourcesOnDeletion`), the data-loss window, and an open item ("restent à vérifier … l'intégrité applicative") | **V** good: the rule and the incident, plus the image-updater decision |
| ref-stack | **V** the latest decisions (reranker cache, rejected options, "performance is not the priority") | **V** nothing ≥ 0.3: English emoji commit subjects vs French facts |

Reading: A is cheap, deterministic, language-independent and surfaces open items; B depends on commit wording. Other API facts found on the way (**V**, 1.1.0 OpenAPI): `PATCH /memories/{id}` accepts `state: "invalidated" | "valid"` + `reason` (soft retire, excluded from recall and consolidation, reversible) but **no tags** field; `GET /entities` accepts `tags` filters (entity index per project through the API, no direct DB access).

Decision input: briefing = A, startup only, ≤ 1 200 chars, titled as "where you left off on <project>", plus open items once retro facts carry `kind:` tags. Success metric (telemetry): a briefed memory id is cited, searched or read within the session.
