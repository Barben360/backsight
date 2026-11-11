package harness

// Codex's skills and user-facing strings: block 7
// (https://github.com/Barben360/backsight/issues/282), 0085 D1 (texts:
// AGENTS.md for retro targets, $backsight:… and implicit skills, the
// /hooks review) and D4 (pause reads CODEX_SESSION_ID).
//
// Until block 7 they are a plain copy of Claude Code's: Codex is off, so
// no user reads them, and block 3 ships its plugin with them as they are.
// Block 7 rewrites both for Codex.

// Texts are the user-visible words about Codex: Claude Code's until block 7.
func (Codex) Texts() Texts { return Claude{}.Texts() }

// Skills are backsight's skill files for Codex, by path relative to the
// plugin's root: Claude Code's until block 7.
func (Codex) Skills(shim string) map[string][]byte { return Claude{}.Skills(shim) }
