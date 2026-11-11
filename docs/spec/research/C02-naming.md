# C02 — Naming (checked 2026-10-23)

Legend: **V** = VERIFIED, **I** = INFERRED. Facts only, no legal conclusion.

## Anthropic rules on "Claude" in third-party names

| Rule | Text (verbatim or close) | Source | |
|---|---|---|---|
| Trademark Guidelines (effective 2024-08-01) | "You may only use our trademarks as specifically permitted by us and only in materials we approve beforehand." / "You may not use our trademarks in a manner that implies Anthropic's sponsorship or endorsement." No specific clause on repo/product names | https://www.anthropic.com/legal/trademark-guidelines | V |
| Claude Code name & logo | "you can't use the Claude Code or Anthropic names or logos as part of your own product, feature, or company name, in your own logo, or in a way that suggests Anthropic built, endorses, or is partnered with your product." Plain-text statements of fact ("runs Claude Code") are allowed | https://code.claude.com/docs/en/legal-and-compliance ("Using the Claude Code name and logo") | V |
| Agent SDK branding | Allowed: "Claude Agent", "{YourAgentName} Powered by Claude". Not permitted: "Claude Code" or "Claude Code Agent", Claude Code-like ASCII art. "Your product should maintain its own branding and not appear to be Claude Code or any Anthropic product." | https://code.claude.com/docs/en/agent-sdk/overview ("Branding guidelines") | V |
| Plugin `name` validator | Error: starts with `claude-`, `anthropic-`, `anthropics-`, `cc-plugin-`; equals `claude`, `claude-code`…; `official` next to claude. **Warning**: "Has `claude` … as a whole word anywhere else, such as `mcp-for-claude`" → message `Plugin name "<name>" reads as one of Anthropic's own`. Claude Code still installs/loads it | https://code.claude.com/docs/en/plugins/manifest-reference (`name`) | V |
| Marketplace reserved names | Official names (`claude-code-plugins`, `claude-plugins-official`, …), impersonations (`official-claude-plugins`, `claude-plugins-v2`), `claudeai-*` prefix | https://code.claude.com/docs/en/plugins/marketplace-reference ("Reserved names") | V |

→ `hindsight-claude` as a **plugin name** = validator *warning*, not error (`claude` is a whole word, not a prefix) (V by rule table). `claude plugin init/tag` refuse only error-level names (V).

## Vectorize position on "Hindsight"

| Item | Finding | Source | |
|---|---|---|---|
| Mark | README: "Hindsight™" (™, no ®) | https://github.com/vectorize-io/hindsight/blob/main/README.md l.24 | V |
| Published brand/trademark policy for Hindsight | None found (no TRADEMARK file, no docs page, web search empty) | repo tree; web search 2026-10-23 | V (absence) |
| General ToU | Vectorize AI, Inc. claims trademarks/logos as IP | https://blog.vectorize.io/terms | V |
| USPTO | A "HINDSIGHT" mark exists (serial 97135067, AR/metaverse software, owner not identified) — unrelated class/owner unknown | https://trademarks.justia.com/971/35/hindsight-97135067.html | V (link) / owner I |
| Name collision | Unrelated company "Hindsight" (usehindsight.com, win-loss analysis) | https://tostracker.app/document/usehindsight | V |
| Vectorize's own naming pattern | `hindsight-claude-agent-sdk` (Python pkg dir `hindsight_claude_agent_sdk`), `hindsight-coding-agents`, `hindsight-claude-hook` bin, `hindsight-memory` (Claude Code plugin name cited in their code) | https://github.com/vectorize-io/hindsight/tree/main/hindsight-integrations/claude-agent-sdk ; npm package.json `bin`; container `claude_code_llm.py` l.38-46 | V |
| Third parties already using "hindsight-*" | `pi-hindsight` (npm), `tcsenpai/hindcode`, `mage0535/hermes-memory-installer` (209★), several `*-hindsight-*` repos — no visible takedowns | npm/jsDelivr; GitHub search | V (existence), I (tolerance) |

## Existing "hindsight-claude"-like names

| Name | Where | What | |
|---|---|---|---|
| `Codestz/claude-hindsight` (21★, 2026-09) | GitHub | Observability tool for Claude Code transcripts — **not** Vectorize Hindsight | V |
| `claude-hindsight` 0.4.0 | npm | "Local dashboard for Claude Code: project briefings and CLAUDE.md instruction audits" | V |
| `ThePlenkov/hindsight-claude` | GitHub | empty repo (size 0, 2026-04-04) | V |
| `sabarishraja/Claude-hindsight`, `laszlolaszlo/hindsight-claude_code-intergartion` | GitHub | 0★ hobby repos | V |
| `gcswan/hindsight-cc` | GitHub | exists | V |
| `hindsight-claude` | npm | free (E404) | V |
| `github.com/Barben360/hindsight-claude` | GitHub | free (404); `Barben360/hindsight-claude-spec` exists | V |

## Go module path

| Option | Notes | |
|---|---|---|
| `github.com/Barben360/hindsight-claude` | Must match repo path exactly; module paths are case-sensitive, proxy escapes capitals (`!barben360`) — works, but every import must use `Barben360` consistently | V (Go module reference https://go.dev/ref/mod#goproxy-protocol) |
| vanity `go.<domain>/hindsight-claude` | Decouples from GitHub owner/case; needs a domain + `go-import` meta | I |
| Major versions | v2+ need `/v2` suffix — plan v0/v1 for a long time | V (https://go.dev/ref/mod#major-version-suffixes) |
| Binary name ≠ module | e.g. module `…/hindsight-claude`, binary `hsc`/`hindsight-claude` | I |

## Alternative names (if "hindsight-claude" is judged risky)

Availability = GitHub name search + npm on 2026-10-23.

| Name | Why | GitHub | npm |
|---|---|---|---|
| `hindkeep` | No third-party mark at all; short CLI; "keeps hindsight"; no collision | free | free |
| `hindsight-keeper` | Descriptive, keeps upstream discoverability, avoids "Claude" entirely (no plugin warning) | free | free |
| `hindsight-harbor` | Same pattern, evokes "self-hosted home" for the memory | free | free |
| `hindsight-pilot` | Same pattern; "pilots" install/update/heal | free | free |
| `<name> for Claude Code` as tagline only | Name avoids "Claude"; compatibility stated in plain text, which the legal page allows ("runs Claude Code") | — | — |

Risk notes per pattern (I): names with "Claude" → plugin-validator warning + reliance on the "plain text, no endorsement" allowance; names with "Hindsight" → no published Vectorize policy, upstream itself and community use `hindsight-*` freely; neither → zero mark exposure, loses search discoverability.

## Implications for hindsight-claude

- "Claude Code" (two words together) in the product name is explicitly not allowed by Anthropic's Claude Code legal page; "claude" alone is not forbidden by any page found, but the trademark guidelines require prior approval for any use of marks and forbid implied endorsement.
- As a Claude Code plugin id, `hindsight-claude` passes with a *warning* ("reads as one of Anthropic's own"); a plugin name starting with `claude-` would be an error.
- `claude-hindsight` is already taken on npm and GitHub by an unrelated Claude Code tool → confusion risk with the inverted name.
- No Vectorize trademark policy exists; ™ claim only. Vectorize itself uses `hindsight-claude-*` names.
- `github.com/Barben360/hindsight-claude` and npm `hindsight-claude` are free today.
- A neutral product name + "for Claude Code" descriptor removes both mark questions at the cost of discoverability.

## Second round (2026-10-24, the author asked for a name without "claude")

Exact-name GitHub repos / npm / Homebrew formula, checked 2026-10-24.

| Name | Idea | GitHub exact | npm | brew |
|---|---|---|---|---|
| `backsight` | Surveying term: the sighting taken back to a known point before moving on — hindsight used as a tool | 6 repos, max 1★ (one unrelated agent-verification harness) | free | free |
| `hindkeep` | "keeps hindsight" | 0 | free | free |
| `recallkeep` | keeps recall working | 0 | free | free |
| `hindmind` | | 0 | free | free |
| `keepsight` | | 1 repo, 0★ | free | free |
| `aidememoire` | French/English "memory aid" | 11 repos, max 1★ | free | free |
| `hindsight-keeper`, `hindsight-harbor` | upstream discoverability, unofficial-looking risk | 0 | free | free |
| taken or crowded | `cairn`, `engram`, `waymark`, `trailmark`, `lorekeep`, `souvenir`, `recollect`, `mnemos`, `tidemark`, `carnet`, `rearview`, `keepr` | many | taken | — |
