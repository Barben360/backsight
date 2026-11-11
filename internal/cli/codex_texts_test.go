package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

// The lines a Codex session shows (block 7, 0085 D1): Codex's first-session
// notice, drift line and update line, in Codex's words (its $backsight:
// skills, its install command), never Claude Code's.
func TestCodexSessionStartBannerSpeaksCodex(t *testing.T) {
	home := hookHome(t)
	// Codex wired by an older binary: its plugins drift; a release waits.
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.ClientFor(harnessid.Codex).PluginVersion = "0.0.0-older"
		s.UpdateCheck = &state.UpdateCheck{CheckedAt: time.Now(), Installed: buildinfo.Version, Latest: "99.0.0", Newer: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(cxAnswer(t, cxHook(t, harness.EventSessionStart, cxPayload(t, home, "SessionStart.startup", nil))).SystemMessage, "\n")
	if len(lines) != 4 {
		t.Fatalf("banner %q", lines)
	}
	if !strings.HasSuffix(lines[0], "; `$backsight:pause` stops it for this session, `$backsight:pause project` for this project.") {
		t.Errorf("first-session notice %q", lines[0])
	}
	if lines[1] != "🧠 backsight · 🗂️ billing" {
		t.Errorf("status %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "⚠️ Codex wiring drifted (Codex plugin 0.0.0-older is not this binary's") ||
		!strings.HasSuffix(lines[2], ") · run `backsight client install --codex`") {
		t.Errorf("drift %q", lines[2])
	}
	if !strings.HasSuffix(lines[3], " → 99.0.0 · update here: `$backsight:update` · or in a terminal: `backsight update apply`") {
		t.Errorf("update %q", lines[3])
	}
	for _, l := range lines {
		if strings.Contains(l, "/backsight:") || strings.Contains(l, "Claude") {
			t.Errorf("Claude Code's words in Codex's banner: %q", l)
		}
	}
}

// update apply's last step names what to restart: Claude Code and Codex
// when both are wired, Codex alone when Claude Code is not.
func TestUpdateApplyNamesCodexToRestart(t *testing.T) {
	for name, want := range map[string]string{
		"both":  "installed; restart Claude Code (or /reload-plugins) and Codex to load their plugins",
		"codex": "installed; restart Codex to load its plugins",
	} {
		t.Run(name, func(t *testing.T) {
			home, _, _ := installedAt(t, "0.1.0")
			withCodex(t, home)
			codexHomeWith(t, home, codexStart)
			installCodex(t)
			if name == "codex" {
				if err := state.Update(pathsOf(home), "test", func(s *state.State) error { s.PluginVersion = ""; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			src := t.TempDir()
			signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
			code, out, _ := run(t, "update", "apply", "--source", src, "--json")
			if code != output.OK || !strings.Contains(out, want) {
				t.Errorf("exit %d, want %q in\n%s", code, want, out)
			}
			if name == "codex" && strings.Contains(out, "restart Claude Code") {
				t.Errorf("Codex alone, told to restart Claude Code:\n%s", out)
			}
		})
	}
}

// uninstall that removed Codex's plugins tells the user to restart Codex.
func TestUninstallNamesCodexToRestart(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	withCodex(t, home)
	codexHomeWith(t, home, codexStart)
	installCodex(t)
	code, out, errOut := run(t, "uninstall", "--json")
	if code != output.OK || !strings.Contains(out, "restart Codex: its open sessions still run the removed plugins") {
		t.Errorf("exit %d %s %s", code, out, errOut)
	}
}

// A memory tool refused because this PC joined another server meanwhile
// tells a Codex agent to restart Codex, not Claude Code.
func TestCodexMemoryServerNamesCodexToRestart(t *testing.T) {
	home := codexHome(t, map[string]string{codexRoot: "billing"})
	showIn(t, home, codexRoot, creditA)
	rawFact(creditA)
	cs := codexMCP(t, home)
	if err := state.Update(pathsOf(home), "connect", func(s *state.State) error { s.SendsGeneration++; return nil }); err != nil {
		t.Fatal(err)
	}
	out, isErr := callMeta(t, cs, codexMeta(codexRoot, codexRoot), "memory_credit", verdict("wrong", "aaaaaaaa"))
	if !isErr || !strings.Contains(out, "joined another server since this session started (restart Codex: its memory tools then use that server)") ||
		strings.Contains(out, "Claude") {
		t.Errorf("%v %s", isErr, out)
	}
}
