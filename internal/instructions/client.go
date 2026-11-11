package instructions

import (
	"fmt"
	"strings"

	"github.com/Barben360/backsight/internal/harnessid"
)

// The words that differ from one coding tool to the next, by harness id
// (internal/harnessid; "" is Claude Code). Claude Code runs a plugin skill
// as /backsight:<name> and reads its arguments, its allowed tools and its
// `!` lines; Codex lists the same skills as backsight:<name>, runs one the
// user names as $backsight:<name> (or picks one by its description), and
// reads SKILL.md as plain instructions: no arguments, no pre-approved
// commands, no command run before the model reads it (decision 0085 D1,
// block 7 https://github.com/Barben360/backsight/issues/282).

// isCodex: the words are Codex's.
func isCodex(client string) bool { return client == harnessid.Codex }

// SkillRef is how the user invokes backsight's skill name in client:
// "/backsight:pause" in Claude Code, "$backsight:pause" in Codex.
func SkillRef(client, name string) string {
	if isCodex(client) {
		return "$backsight:" + name
	}
	return "/backsight:" + name
}

// FirstSessionNoticeFor is FirstSessionNotice in client's words: the
// pause skill as client names it.
func FirstSessionNoticeFor(client, server, provider string) string {
	if !isCodex(client) {
		return FirstSessionNotice(server, provider)
	}
	return fmt.Sprintf("👋 Memory is on: this session is stored on %s and processed by %s; `%s` stops it for this session, `%s project` for this project.",
		server, provider, SkillRef(client, "pause"), SkillRef(client, "pause"))
}

// UpdateLineFor is UpdateLine in client's words: the update skill as
// client names it.
func UpdateLineFor(client, running, latest string, newer int) string {
	line := UpdateLine(running, latest, newer)
	if !isCodex(client) {
		return line
	}
	return strings.Replace(line, "`"+SkillRef(harnessid.ClaudeCode, "update")+"`", "`"+SkillRef(client, "update")+"`", 1)
}

// DriftLineFor is DriftLine for client: its name and the command that
// wires it again.
func DriftLineFor(client, what string) string {
	if !isCodex(client) {
		return DriftLine(what)
	}
	return EmojiWarning + " Codex wiring drifted (" + what + ") · run `backsight client install --codex`"
}
