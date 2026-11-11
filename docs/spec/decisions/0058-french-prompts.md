# 0058 — No French-specific recall floor; French measured apart

- **Date**: 2026-10-25
- **Status**: accepted (agent; open to veto)
- **Decided by**: agent, M3 block 3 (0056), from experiment E26

## Decision
- The prompt and gate recall keep one set of floors (0.5, and 0.6 for foreign projects) whatever the prompt's language.
- The `prompt` telemetry event carries `lang` (`fr|en`, by function-word count over the prompt's head). `stats` shows recalls and the share that injected something, per language.
- The French gap is re-measured in E24 (M4 cut-over), on prompts typed after 2026-10-24.

## Why
- E26 (outside E23's prompts, with 0057's reranker settings) finds no uniform French score shift that a floor could correct: losses are a tail of collapsed cross-lingual pairs.
- A 0.1 lower French floor recovers 7 of 120 English-strong hits for 3 noise hits on the global scope, and changes nothing on the prompt path for the available prompts.
- A language branch in the recall path is not worth carrying without evidence. The language field makes the next measurement cheap.
