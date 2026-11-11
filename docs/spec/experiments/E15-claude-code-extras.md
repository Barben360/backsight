# E15/E16 — More Claude Code facts for the integration

Date: 2026-10-24. Same isolated harness as E1 (local marketplace plugin, fake binary, `claude -p`, Haiku). **V** = observed.

| Check | Result |
|---|---|
| MCP **prompts** from a plugin server | **V** listed as slash command `mcp__plugin_hc-memory_memory__recall` |
| MCP tool annotation `readOnlyHint: true` | **V** does **not** skip permission: the call was denied ("haven't granted it yet") → allow rules stay necessary, for every tool we want frictionless |
| SessionStart writing `export X=…` to `CLAUDE_ENV_FILE` | **V** later Bash commands see `X` → the CLI fallback can know project and session without arguments |
| `cd` inside a Bash command | **V** the shell cwd resets after the command and **no CwdChanged hook fired** → project resolution from the hook's `cwd` is stable |
| **PostToolUseFailure** hook with `additionalContext` | **V** fired on a failing Bash command; the model quoted the injected text → the "failed command → pitfall" channel exists |
