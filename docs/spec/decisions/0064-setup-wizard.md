# 0064 — `setup`: questions, adoption, state, undo and verify

- **Date**: 2026-10-25
- **Status**: accepted (agent, under the author's delegation; open to veto)
- **Decided by**: agent, M4 block 3 (plan 0063)

## Decision

### Questions (`setup --questions --json`)
Each question has `id`, `prompt`, `options[{value,label,cost,caveat}]`, `default`, `recommended`, `asked` (false when an answer or the config already settles it) and `source` (`default`, `answer`, `config`, `saved`).

| id | options (recommended first) | asked when |
|---|---|---|
| `provider` | `deepseek` (cheapest measured, E7), `anthropic`, `openai`, `ollama` (private, lower quality, E13), `claude-code` (subscription, terms caveat, 0032), any other Hindsight provider id (listed, untested) | always |
| `model` | the provider's preset: `deepseek-v4-flash`, `claude-haiku-5-5`, `gpt-5.4-mini`, `qwen3:8b`, `claude-haiku-5-5` (pinned so the CLI default does not run Opus-class) | always, defaulted |
| `base_url` | free text | `ollama` (default `http://host.docker.internal:11434`), optional for `anthropic`/`openai` |
| `ack_subscription` | `yes` | `claude-code` only: the terms warning of C07 (e) must be acknowledged |
| `retention` | `3650`, `keep`, or a number of days (0041) | always |
| `backups` | `on`, `off` (0023) | always |
| `reference` | `retire`, `keep` (§11.5) | reference pieces detected |
| `backfill` | `yes`, `no`, with the `--plan` numbers in `cost` (0037) | sessions to send exist; `no` is recommended on the `claude-code` lane (0043) |
| `consent` | `yes`, `no` | always, last: the plan the answers give is shown with it |

Hidden answers with defaults: `api_port` (8888), `ui_port` (9999). There is no `embeddings` question while only the multilingual preset renders (0049).

### Adopting an existing install
When `config.toml` exists, its provider, model, base URL, ports, retention and backup setting are the answers (`source: config`). An `--answer` that disagrees is a usage error (exit 2) naming the key to edit. This is how the M4 cut-over adopts the dev install (0063 Q1): the server step finds it healthy, the bank step finds its hash, nothing is reinstalled.

### State and resume
`<state>/setup.json` holds the run: id, the answers (never secrets), each step's status, and what undo needs (config-editor run ids, whether setup created the plugin entry, installed the scheduler entries, started the server). Each step's start and end is appended to `<state>/journal.jsonl`, the config editor's journal. `--resume` reuses the saved answers under any new `--answer` and skips the steps already done, except `verify`. Every step is idempotent, so a plain rerun is also safe.

### Output
- Text mode prints one line per step as it ends. Interactive mode (a terminal on stdin and stdout, no `--json`, no `--non-interactive`) asks the missing questions with their options and reads a missing secret without echo, as `secret set` does.
- With `--json`, stdout carries the one envelope as for every command, and progress lines `{"event":"progress","step","pct","eta_s"}` go to **stderr**, one JSON object per line, so an agent parses stdout whole.
- Missing answers or secrets without a terminal: exit 10 with `questions[]` (full schema), `secrets[]`, `actions[]` and `resume`.

### Undo (`setup --undo [--plan] [--force]`)
In reverse order: the config-editor runs setup made are restored (a file edited since is refused unless `--force`), the scheduler entries setup installed are removed, the plugin and marketplace setup registered are removed, and a server setup started is stopped. `config.toml`, `secrets.toml` and the database volume are kept: the volume only opens with the password in `secrets.toml`. Removing them is `uninstall --purge-local`/`--delete-data` (§11.9).

### Verify (`backsight verify`, setup step 8)
- Every plugin hook runs through the shim with a synthetic stdin: exit 0, empty or valid JSON output, within its deadline.
- The MCP server answers `initialize` and lists the three memory tools.
- Round trip in a **scratch bank** `backsight-verify-<random>`: one fixed document is retained, recalled, and the bank is deleted. Nothing touches the user's bank (no stray fact, observation or entity), and the cost is one small extraction.

### Backfill
`backfill=yes` queues the backfill with its default cap (1.5 × the estimate); `jobs tick` runs it in the background. Setup never runs it in the foreground.

### CLAUDE.md
Setup writes no CLAUDE.md block by default: the SessionStart guide alone reached 14/14 search-first in E6 (0020). The block and the removal of a reference CLAUDE.md section (0042: known text, else `claude_md_section=remove`) come with the retirement of reference pieces.

## Why
0022 fixes the contract but not the question set, the adoption rule the cut-over needs (0063 Q1), where resume state lives, what undo may remove, or how verify avoids writing into the user's memory. Progress on stderr keeps `--json` stdout one envelope, which every other command already guarantees.
