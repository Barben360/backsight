package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/testtool"
)

// withTarget generates the plugin for goos.
func withTarget(t *testing.T, goos string) {
	t.Helper()
	old := targetOS
	targetOS = goos
	t.Cleanup(func() { targetOS = old })
}

// On Windows the skills' commands run in Claude Code's Bash tool (Git
// Bash) or PowerShell: both run C:/…/current.exe, and Git Bash reads a
// backslash inside quotes as itself only by luck. The skills carry the
// shim with forward slashes; the hooks and the MCP server, exec form,
// keep the native path.
func TestSkillsShimUsesForwardSlashesOnWindows(t *testing.T) {
	withTarget(t, "windows")
	shim := `C:\Users\Jé & Co\AppData\Local\backsight\bin\current.exe`
	dir := t.TempDir()
	if _, err := Generate(dir, shim, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	word := `'C:/Users/Jé & Co/AppData/Local/backsight/bin/current.exe'`
	skills, _ := filepath.Glob(filepath.Join(dir, "plugins", "backsight", "skills", "*", "SKILL.md"))
	if len(skills) < 5 {
		t.Fatalf("skills %q", skills)
	}
	shown := 0
	for _, s := range skills {
		raw, _ := os.ReadFile(s)
		if strings.Contains(string(raw), `C:\`) || strings.Contains(string(raw), `Co\AppData`) {
			t.Errorf("%s carries a backslash path:\n%s", s, raw)
		}
		shown += strings.Count(string(raw), word)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "plugins", "backsight", "skills", "pause", "SKILL.md"))
	if fm := frontmatter(t, "pause", string(raw)); fm["allowed-tools"] != "Bash("+word+" pause*)" || !strings.Contains(string(raw), "`"+word+" pause --project`") {
		t.Errorf("pause skill:\n%s", raw)
	}
	if shown < 10 {
		t.Errorf("the forward-slash shim appears %d times", shown)
	}
	var hooks hooksFile
	read(t, filepath.Join(dir, "plugins", "backsight", "hooks", "hooks.json"), &hooks)
	for ev, groups := range hooks.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Command != shim {
					t.Errorf("%s hook command %q, want the native path %q", ev, h.Command, shim)
				}
			}
		}
	}
	var mcp struct {
		Servers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	read(t, filepath.Join(dir, "plugins", "backsight", ".mcp.json"), &mcp)
	if mcp.Servers["memory"].Command != shim {
		t.Errorf("MCP command %q, want the native path", mcp.Servers["memory"].Command)
	}
	// Off Windows a backslash is part of a name: the shim is as given.
	withTarget(t, "linux")
	dir = t.TempDir()
	if _, err := Generate(dir, `/srv/a\b/current`, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "plugins", "backsight", "skills", "pause", "SKILL.md")); !strings.Contains(string(raw), `'/srv/a\b/current' pause`) {
		t.Errorf("a Linux shim was rewritten:\n%s", raw)
	}
}

// The skill's command line, as Claude Code's Bash tool runs it (Git Bash
// on Windows), starts the shim from a directory with a space and an &.
func TestSkillsShimWithSpacesRunsUnderGitBash(t *testing.T) {
	bash := testtool.Bash(t)
	fake := testtool.Write(t, filepath.Join(t.TempDir(), "Jé & Co", "bin"), "current", testtool.Fake{})
	dir := t.TempDir()
	if _, err := Generate(dir, fake, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "plugins", "backsight", "skills", "pause", "SKILL.md"))
	m := regexp.MustCompile("run `([^`]*) pause --project`").FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("no command line in the pause skill:\n%s", raw)
	}
	line := m[1] + " pause --project"
	if runtime.GOOS == "windows" && strings.Contains(line, `\`) {
		t.Fatalf("a backslash path reaches the shell: %s", line)
	}
	if out, err := exec.Command(bash, "-c", line).CombinedOutput(); err != nil {
		t.Fatalf("bash -c %s: %v\n%s", line, err, out)
	}
	if got := testtool.Calls(t, fake); len(got) != 1 || strings.Join(got[0], " ") != "pause --project" {
		t.Errorf("the shim ran with %q, want once with pause --project", got)
	}
}
