package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hook"
)

func read(t *testing.T, p string, v any) {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

type hookEntry struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

type hooksFile struct {
	Hooks map[string][]struct {
		Matcher string      `json:"matcher"`
		Hooks   []hookEntry `json:"hooks"`
	} `json:"hooks"`
}

func TestGenerateWritesAMarketplaceWithOnePlugin(t *testing.T) {
	dir := t.TempDir()
	changed, err := Generate(dir, "/opt/backsight/bin/current", "1.2.3")
	if err != nil || !changed {
		t.Fatalf("changed %v err %v", changed, err)
	}
	var mkt struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name, Source string
		} `json:"plugins"`
	}
	read(t, filepath.Join(dir, ".claude-plugin", "marketplace.json"), &mkt)
	if mkt.Name != Marketplace || len(mkt.Plugins) != 1 || mkt.Plugins[0].Name != Name || mkt.Plugins[0].Source != "./plugins/backsight" {
		t.Errorf("marketplace %+v", mkt)
	}
	var pj struct {
		Name, Version string
		Author        struct{ Name string }
	}
	read(t, filepath.Join(dir, "plugins", "backsight", ".claude-plugin", "plugin.json"), &pj)
	if pj.Name != Name || !strings.HasPrefix(pj.Version, "1.2.3+") || pj.Author.Name == "" {
		t.Errorf("plugin.json %+v", pj)
	}
	var mcp struct {
		Servers map[string]struct {
			Command    string   `json:"command"`
			Args       []string `json:"args"`
			AlwaysLoad bool     `json:"alwaysLoad"`
		} `json:"mcpServers"`
	}
	read(t, filepath.Join(dir, "plugins", "backsight", ".mcp.json"), &mcp)
	if m := mcp.Servers[Server]; m.Command != "/opt/backsight/bin/current" || strings.Join(m.Args, " ") != "mcp" || !m.AlwaysLoad || len(mcp.Servers) != 1 {
		t.Errorf(".mcp.json %+v", mcp)
	}
}

// Every event of SPEC §6.2, in exec form on the shim, with a timeout above
// the supervisor's child deadline.
func TestHooksCoverEveryEventInExecForm(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(dir, "/opt/backsight/bin/current", "1.2.3"); err != nil {
		t.Fatal(err)
	}
	var hf hooksFile
	read(t, filepath.Join(dir, "plugins", "backsight", "hooks", "hooks.json"), &hf)
	want := map[string][]string{ // Claude Code event -> matchers
		"SessionStart":     {"startup", "resume|clear|compact|fork"},
		"UserPromptSubmit": {""},
		"PreToolUse":       {"AskUserQuestion"},
		"PostToolUse":      {"mcp__plugin_backsight_memory__.*"},
		"SubagentStart":    {""},
		"SubagentStop":     {""},
		"Stop":             {""},
		"SessionEnd":       {""},
	}
	if len(hf.Hooks) != len(want) {
		t.Errorf("%d events, want %d", len(hf.Hooks), len(want))
	}
	for ev, matchers := range want {
		groups := hf.Hooks[ev]
		if len(groups) != len(matchers) {
			t.Fatalf("%s: %d groups", ev, len(groups))
		}
		for i, g := range groups {
			if g.Matcher != matchers[i] || len(g.Hooks) != 1 {
				t.Errorf("%s group %d: %+v", ev, i, g)
				continue
			}
			h := g.Hooks[0]
			if h.Type != "command" || h.Command != "/opt/backsight/bin/current" || len(h.Args) != 2 || h.Args[0] != "hook" {
				t.Errorf("%s: %+v", ev, h)
			}
			input := `{"source":"` + strings.Split(matchers[i], "|")[0] + `"}`
			if deadline := hook.ChildDeadline(h.Args[1], []byte(input)); time.Duration(h.Timeout)*time.Second <= deadline && ev != "SessionEnd" {
				t.Errorf("%s: timeout %ds not above the child deadline %s", ev, h.Timeout, deadline)
			}
		}
	}
	if hf.Hooks["SessionEnd"][0].Hooks[0].Timeout != 1 {
		t.Error("SessionEnd: 1 s (its 1.5 s budget cannot be raised by a plugin, C06)")
	}
}

func TestGenerateIsIdempotentAndRewritesOnChange(t *testing.T) {
	dir := t.TempDir()
	_, _ = Generate(dir, "/a/current", "1.2.3")
	if changed, err := Generate(dir, "/a/current", "1.2.3"); err != nil || changed {
		t.Errorf("second run changed %v err %v", changed, err)
	}
	if changed, _ := Generate(dir, "/b/current", "1.2.3"); !changed {
		t.Error("a new shim path must rewrite")
	}
	_ = os.WriteFile(filepath.Join(dir, "plugins", "backsight", "stray.txt"), []byte("x"), 0o600)
	if _, err := Generate(dir, "/b/current", "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "backsight", "stray.txt")); err == nil {
		t.Error("files the generator does not own must not survive (the plugin is generated whole)")
	}
}

func TestDevBuildsGetAContentVersion(t *testing.T) {
	a, b := Version("dev", "/a/current"), Version("dev", "/b/current")
	if !strings.HasPrefix(a, "0.0.0-dev+") || a == b || Version("dev", "/a/current") != a {
		t.Errorf("%q %q", a, b)
	}
	// a release too: the shim path is in the generated files, so a moved data
	// directory must be an update Claude Code applies
	r1, r2 := Version("1.2.3", "/a/current"), Version("1.2.3", "/b/current")
	if !strings.HasPrefix(r1, "1.2.3+") || r1 == r2 || Version("1.2.3+meta", "/a") != "1.2.3+meta."+strings.TrimPrefix(Version("1.2.3", "/a"), "1.2.3+") {
		t.Errorf("%q %q", r1, r2)
	}
}

// Exact names and values: tests built from the code's own constants would
// agree with a typo (decision 0054 tool names, SPEC §6.2 timeouts).
func TestExactToolNamesEventsAndTimeouts(t *testing.T) {
	want := []string{"mcp__plugin_backsight_memory__memory_search", "mcp__plugin_backsight_memory__memory_read", "mcp__plugin_backsight_memory__memory_store"}
	if strings.Join(ToolNames, ",") != strings.Join(want, ",") {
		t.Errorf("tool names %v", ToolNames)
	}
	dir := t.TempDir()
	_, _ = Generate(dir, "/s", "1.2.3")
	var hf hooksFile
	read(t, filepath.Join(dir, "plugins", "backsight", "hooks", "hooks.json"), &hf)
	exact := map[string]string{ // event/matcher -> backsight event, timeout
		"SessionStart/startup": "session-start 10", "SessionStart/resume|clear|compact|fork": "session-start 5",
		"UserPromptSubmit/": "user-prompt-submit 25", "PreToolUse/AskUserQuestion": "pre-tool-use 10",
		"PostToolUse/mcp__plugin_backsight_memory__.*": "post-tool-use 2", "SubagentStart/": "subagent-start 5",
		"SubagentStop/": "subagent-stop 5", "Stop/": "stop 5", "SessionEnd/": "session-end 1",
	}
	n := 0
	for ev, groups := range hf.Hooks {
		for _, g := range groups {
			n++
			h := g.Hooks[0]
			got := h.Args[1] + " " + strconv.Itoa(h.Timeout)
			if exact[ev+"/"+g.Matcher] != got {
				t.Errorf("%s/%s: %s, want %s", ev, g.Matcher, got, exact[ev+"/"+g.Matcher])
			}
		}
	}
	if n != len(exact) {
		t.Errorf("%d groups, want %d", n, len(exact))
	}
	var mkt, pj map[string]any
	read(t, filepath.Join(dir, ".claude-plugin", "marketplace.json"), &mkt)
	read(t, filepath.Join(dir, "plugins", "backsight", ".claude-plugin", "plugin.json"), &pj)
	if mkt["owner"] == nil || mkt["description"] == nil || pj["description"] == nil {
		t.Errorf("fields Claude Code's schema wants: %v %v", mkt, pj)
	}
}

func TestANewBinaryVersionRewritesThePlugin(t *testing.T) {
	dir := t.TempDir()
	_, _ = Generate(dir, "/s", "1.2.3")
	if changed, _ := Generate(dir, "/s", "1.2.4"); !changed {
		t.Fatal("not rewritten")
	}
	var pj struct{ Version string }
	read(t, filepath.Join(dir, "plugins", "backsight", ".claude-plugin", "plugin.json"), &pj)
	if !strings.HasPrefix(pj.Version, "1.2.4+") {
		t.Errorf("version %s", pj.Version)
	}
	if ents, _ := os.ReadDir(filepath.Dir(dir)); len(ents) != 1 {
		t.Errorf("temporary trees left beside the marketplace: %v", ents)
	}
}
