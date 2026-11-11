# 0039 — Facts and agent-facing texts in English; multilingual models kept

- **Date**: 2026-10-24
- **Status**: accepted
- **Decided by**: author (§20 Q5, following the agent's recommendation from E19)

## Decision
- Every strategy text and bank mission tells the extractor to write facts **in English**, whatever the conversation language, keeping identifiers, paths, commands and quoted user wording verbatim.
- One **English instruction set** (guide, reminder, nudge, detector lines, gate reason, recall framing, tool descriptions, subagent text). Claude keeps answering in the user's language; the guide says so.
- Default embedding/reranker preset stays **multilingual** (prompts are searched raw, in whatever language the user types). The English preset remains an advanced answer (`--answer embeddings=english`) for users who only write English; it is never asked.
- Setup no longer asks for a language; `language` leaves `config.toml` (schema stays 1: the key was never released).
- The provider check expects ≥ 1 well-formed fact **in English**.
- The detector keeps French and English patterns (it reads the user's prompts, not the facts).

## Why
E19 on the author's data: English facts +0.047 nDCG@3 with the multilingual models (significant); English-only models cut useful injections on French prompts from 37 % to 10 %; the author writes both languages. One instruction set halves the texts to test and size (E6). E20 (M1) re-checks the fact-language effect inside Hindsight.
