package harness

import (
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/plugin"
)

// Codex's skills and user-facing strings: block 7
// (https://github.com/Barben360/backsight/issues/282), 0085 D1 (texts:
// AGENTS.md for retro targets, $backsight:… and implicit skills, the
// /hooks review). The words that also live in the instruction set (the
// skills, the session's lines) are rendered there per harness id
// (instructions.SkillsFor, SkillRef, FirstSessionNoticeFor, DriftLineFor,
// UpdateLineFor), from the same source as Claude Code's.

// Texts are the user-visible words about Codex.
func (Codex) Texts() Texts {
	return Texts{
		ProfileFlag: "Codex's home (default: the one recorded when Codex was wired, else $CODEX_HOME, else ~/.codex)",
		InstallCmd:  "npm install -g @openai/codex",
		InstallWhy:  "backsight wires itself into Codex through its plugin commands",
	}
}

// Skills are backsight's skill files for Codex, by path relative to the
// plugin's root: the same skills as Claude Code's, in Codex's words
// (SKILL.md with a name and a description only, plain instructions).
func (Codex) Skills(shim string) map[string][]byte {
	return plugin.SkillFilesFor(harnessid.Codex, shim)
}
