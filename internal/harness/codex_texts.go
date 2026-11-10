package harness

// Codex's skills and user-facing strings: block 7
// (https://github.com/Barben360/backsight/issues/282), 0085 D1 (texts:
// AGENTS.md for retro targets, $backsight:… and implicit skills, the
// /hooks review) and D4 (pause reads CODEX_SESSION_ID). Stubs until then.

// Texts are the user-visible words about Codex. Block 7; empty until then.
func (Codex) Texts() Texts { return Texts{} }

// Skills are backsight's skill files for Codex, by path relative to the
// plugin's root. Block 7; none until then.
func (Codex) Skills(shim string) map[string][]byte { return nil }
