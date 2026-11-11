# E9 — `kind:*` tags from an entity-label group + the `retro` strategy (Hindsight 1.1 sandbox)

Date: 2026-10-24. Sandbox only (port 18888, copy of the live DB). **V** = observed.

Setup: bank config PATCH adding an entity-label group `{"key": "kind", "tag": true, "type": "multi-values", "optional": true, values decision/correction/convention/pitfall/state/lesson with one-line descriptions}` and a strategy `retro` (`retain_extraction_mode: custom`, French instructions starting with the language rule, durable-only rules, "label each fact with its kind", chunk 30 000). One condensed session (my-garage, 2026-10-08) retained async with tags `project:e9test, source:retro, harness:claude-code`, `observation_scopes: combined`.

Results:
- **V** 5 experience facts + 1 world fact, all in French (the language line inside the strategy instructions fixes E7's English output).
- **V** every fact carries `kind:<value>` tags (one fact got two: `kind:pitfall,kind:decision`), next to the request's tags.
- **V** 6 observations consolidated within ~1 min, each inheriting its sources' `kind:*` tags.
- Content matched the `claude -p` retro of E7 (same decision, pitfalls and final state) plus one convention and one lesson.

Consequences: `kind:` filtering is available server-side for the briefing (open items, pitfalls), recall framing and stats, with no client-side classification. The language rule must live in every custom strategy text.
