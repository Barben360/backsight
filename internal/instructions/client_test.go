package instructions

import (
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/harnessid"
)

// Each coding tool names backsight's skills its own way.
func TestSkillRefPerClient(t *testing.T) {
	for client, want := range map[string]string{"": "/backsight:pause", harnessid.ClaudeCode: "/backsight:pause", harnessid.Codex: "$backsight:pause"} {
		if got := SkillRef(client, "pause"); got != want {
			t.Errorf("SkillRef(%q) = %q, want %q", client, got, want)
		}
	}
}

// The session's lines in Codex name its skills and its install command;
// Claude Code's stay exactly what they were.
func TestSessionLinesPerClient(t *testing.T) {
	for _, client := range []string{"", harnessid.ClaudeCode} {
		if got, want := FirstSessionNoticeFor(client, "s", "p"), FirstSessionNotice("s", "p"); got != want {
			t.Errorf("%q notice %q, want %q", client, got, want)
		}
		if got, want := UpdateLineFor(client, "0.1.0", "0.3.0", 3), UpdateLine("0.1.0", "0.3.0", 3); got != want {
			t.Errorf("%q update line %q, want %q", client, got, want)
		}
		if got, want := DriftLineFor(client, "plugin"), DriftLine("plugin"); got != want {
			t.Errorf("%q drift line %q, want %q", client, got, want)
		}
	}
	cases := map[string]struct{ got, want string }{
		"notice": {FirstSessionNoticeFor(harnessid.Codex, "http://srv:8888", "openai"),
			"👋 Memory is on: this session is stored on http://srv:8888 and processed by openai; `$backsight:pause` stops it for this session, `$backsight:pause project` for this project."},
		"update": {UpdateLineFor(harnessid.Codex, "0.1.0", "0.3.0", 3),
			"⬆️ backsight 0.1.0 → 0.3.0 (3 newer) · update here: `$backsight:update` · or in a terminal: `backsight update apply`"},
		"drift": {DriftLineFor(harnessid.Codex, "shim /x missing"), "⚠️ Codex wiring drifted (shim /x missing) · run `backsight client install --codex`"},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("Codex %s line\n got %q\nwant %q", name, c.got, c.want)
		}
	}
}

// Codex's skills are Claude Code's, by name, description and order, with
// Codex's words: no argument hint, no allowed tools, none of Claude Code's
// syntax.
func TestCodexSkillsMirrorClaudes(t *testing.T) {
	claude, codex := Skills("/s"), SkillsFor(harnessid.Codex, "/s")
	if len(claude) != len(codex) {
		t.Fatalf("%d Codex skills, %d Claude Code's", len(codex), len(claude))
	}
	for i, c := range codex {
		cl := claude[i]
		if c.Name != cl.Name || c.Description != strings.ReplaceAll(cl.Description, "/backsight:", "$backsight:") {
			t.Errorf("skill %d: %q %q, Claude Code's %q %q", i, c.Name, c.Description, cl.Name, cl.Description)
		}
		if c.ArgumentHint != "" || c.AllowedTools != "" {
			t.Errorf("%s: argument hint %q, allowed tools %q", c.Name, c.ArgumentHint, c.AllowedTools)
		}
		for _, claudeOnly := range []string{"$ARGUMENTS", "!`", "/backsight:", "/reload-plugins", "CLAUDE.md", "Claude"} {
			if strings.Contains(c.Body, claudeOnly) {
				t.Errorf("%s holds %q:\n%s", c.Name, claudeOnly, c.Body)
			}
		}
	}
}
