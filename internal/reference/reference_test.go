package reference

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/claudecode"
)

func put(t *testing.T, home, rel, content string) {
	t.Helper()
	p := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The author's machine on 2026-10-24, reduced to what discovery reads.
func authorHome(t *testing.T) string {
	home := t.TempDir()
	put(t, home, ".hindsight/coding-agent.json", `{"apiUrl":"http://127.0.0.1:8888","apiToken":"fakevalue","bankId":"coding-agent::memoire","observationScopes":"combined"}`)
	put(t, home, ".claude/settings.json", `{"hooks":{
 "SessionStart":[{"hooks":[{"type":"command","command":"python3 \"$HOME/.claude/hooks/memory-search.py\" plugin-sessionstart \"/h/.hindsight/coding-agents/dist/claude-session-start.js\""}]}],
 "UserPromptSubmit":[{"hooks":[{"type":"command","command":"node \"/h/.hindsight/coding-agents/dist/claude-hook.js\""},{"type":"command","command":"python3 \"$HOME/.claude/hooks/memory-search.py\""}]}],
 "PreToolUse":[{"matcher":"AskUserQuestion","hooks":[{"type":"command","command":"python3 \"$HOME/.claude/hooks/memory-search.py\""}]}],
 "Stop":[{"hooks":[{"type":"command","command":"node \"/h/.hindsight/coding-agents/dist/claude-stop-hook.js\""}]}],
 "Notification":[{"hooks":[{"type":"command","command":"notify-send done"}]}]}}`)
	put(t, home, ".claude.json", `{"mcpServers":{
 "hindsight":{"command":"node","args":["/h/.hindsight/coding-agents/dist/mcp-server.js"]},
 "user-memory":{"command":"python3","args":["/h/.claude/hooks/hindsight-memory-mcp.py"],"env":{"TOKEN":"fakevalue"}},
 "vfs":{"command":"vfs","args":["mcp"]}}}`)
	put(t, home, ".claude/hooks/memory-retro.py", "# retro")
	put(t, home, ".config/systemd/user/memory-retro.timer", "[Timer]")
	return home
}

func at(home string) claudecode.Location {
	return claudecode.Locate(home, func(string) string { return "" })
}

// Claude Code files under $CLAUDE_CONFIG_DIR are found there, not in ~/.claude.
func TestDiscoverFollowsClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	put(t, home, "cc/settings.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"node /h/.hindsight/coding-agents/dist/claude-stop-hook.js"}]}]}}`)
	put(t, home, "cc/.claude.json", `{"mcpServers":{"hindsight":{"command":"node","args":["/h/.hindsight/coding-agents/dist/mcp-server.js"]}}}`)
	loc := claudecode.Locate(home, func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return filepath.Join(home, "cc")
		}
		return ""
	})
	in, err := Discover(home, loc)
	if err != nil || in == nil {
		t.Fatalf("%+v %v", in, err)
	}
	got := ids(in)
	if got["npm-plugin-hook|settings.json Stop"] != "M4" || got["npm-plugin-mcp|.claude.json mcpServers.hindsight"] != "M4" || len(got) != 2 {
		t.Errorf("%v", got)
	}
}

func ids(in *Install) map[string]string {
	m := map[string]string{}
	for _, p := range in.Pieces {
		m[p.ID+"|"+p.Where] = p.ReplacedIn
	}
	return m
}

func TestDiscoverAuthorMachine(t *testing.T) {
	home := authorHome(t)
	in, err := Discover(home, at(home))
	if err != nil || in == nil {
		t.Fatalf("%v %v", in, err)
	}
	if in.URL != "http://127.0.0.1:8888" || in.Bank != "coding-agent::memoire" || in.Token != "fakevalue" || in.ObservationScopes != "combined" {
		t.Errorf("%+v", in)
	}
	want := map[string]string{
		"npm-plugin-hook|settings.json SessionStart":            "M4",
		"python-search-hook|settings.json SessionStart":         "M4",
		"npm-plugin-hook|settings.json UserPromptSubmit":        "M4",
		"python-search-hook|settings.json UserPromptSubmit":     "M4",
		"python-search-hook|settings.json PreToolUse":           "M4",
		"npm-plugin-hook|settings.json Stop":                    "M4",
		"npm-plugin-mcp|.claude.json mcpServers.hindsight":      "M4",
		"python-memory-mcp|.claude.json mcpServers.user-memory": "M4",
		"python-retro|.claude/hooks/memory-retro.py":            "M4",
		"python-retro-timer|systemd memory-retro.timer":         "M4",
	}
	got := ids(in)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("missing or wrong %s → %q (got %q)", k, v, got[k])
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected pieces: %v", got)
	}
}

func TestDiscoverNothing(t *testing.T) {
	home := t.TempDir()
	in, err := Discover(home, at(home))
	if err != nil || in != nil {
		t.Errorf("empty home: %+v %v", in, err)
	}
}

// Wiring without the plugin config is still a reference install to report.
func TestDiscoverHooksWithoutPluginConfig(t *testing.T) {
	home := t.TempDir()
	put(t, home, ".claude/settings.json", `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"python3 ~/.claude/hooks/hindsight-multibank.py"}]}]}}`)
	in, err := Discover(home, at(home))
	if err != nil || in == nil || ids(in)["legacy-multibank-hook|settings.json UserPromptSubmit"] != "M4" || in.URL != "" {
		t.Errorf("%+v %v", in, err)
	}
}

func TestDiscoverReportsBrokenFiles(t *testing.T) {
	home := t.TempDir()
	put(t, home, ".claude/settings.json", `{"hooks": `)
	if _, err := Discover(home, at(home)); err == nil || !strings.Contains(err.Error(), "settings.json") {
		t.Errorf("want an error naming settings.json, got %v", err)
	}
}

func TestTokenNeverPrinted(t *testing.T) {
	home := authorHome(t)
	in, err := Discover(home, at(home))
	if err != nil || in == nil {
		t.Fatalf("%v %v", in, err)
	}
	js, _ := json.Marshal(in)
	for _, out := range []string{fmt.Sprintf("%v %+v %#v", in, in, *in), string(js)} {
		if strings.Contains(out, "fakevalue") {
			t.Errorf("token leaked: %s", out)
		}
	}
}

// doctor reads Pieces[0]; the order must be stable.
func TestDiscoverOrder(t *testing.T) {
	home := authorHome(t)
	in, _ := Discover(home, at(home))
	for i := 1; i < len(in.Pieces); i++ {
		a, b := in.Pieces[i-1], in.Pieces[i]
		if a.ID+a.Where > b.ID+b.Where {
			t.Errorf("pieces out of order at %d: %v then %v", i, a, b)
		}
	}
	if in.Pieces[0].ID != "npm-plugin-hook" {
		t.Errorf("first piece %v", in.Pieces[0])
	}
}

func TestDiscoverReportsBrokenStateAndPluginConfig(t *testing.T) {
	for rel, want := range map[string]string{".claude.json": ".claude.json", ".hindsight/coding-agent.json": "coding-agent.json"} {
		home := t.TempDir()
		put(t, home, rel, `{"broken": `)
		if _, err := Discover(home, at(home)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want an error naming it, got %v", rel, err)
		}
	}
}

func TestDiscoverPluginConfigWithoutURL(t *testing.T) {
	home := t.TempDir()
	put(t, home, ".hindsight/coding-agent.json", `{"bankId":"b"}`)
	in, err := Discover(home, at(home))
	if err != nil || in == nil || in.URL != "" || len(in.Pieces) != 0 {
		t.Errorf("a config without URL is still an install, with no server: %+v %v", in, err)
	}
}

func TestDiscoverCollapsesDuplicates(t *testing.T) {
	home := t.TempDir()
	put(t, home, ".claude/settings.json", `{"hooks":{"UserPromptSubmit":[
 {"hooks":[{"type":"command","command":"python3 ~/.claude/hooks/memory-search.py"}]},
 {"hooks":[{"type":"command","command":"python3 ~/.claude/hooks/memory-search.py --again"}]}]}}`)
	in, _ := Discover(home, at(home))
	if in == nil || len(in.Pieces) != 1 {
		t.Errorf("one piece per id and place: %+v", in)
	}
}

func TestReferenceMatchers(t *testing.T) {
	for cmd, want := range map[string]bool{
		`python3 "$HOME/.claude/hooks/memory-search.py"`:                   true,
		`node "/home/u/.hindsight/coding-agents/dist/claude-stop-hook.js"`: true,
		`rtk hook claude`: false,
		`/home/u/.local/share/backsight/bin/current hook user-prompt-submit`: false,
	} {
		if HookCommand(cmd) != want {
			t.Errorf("HookCommand(%q) != %v", cmd, want)
		}
	}
	if !MCPServer("node /home/u/.hindsight/coding-agents/dist/mcp-server.js") || !MCPServer("python3 /home/u/.claude/hooks/hindsight-memory-mcp.py") || MCPServer("vfs mcp") {
		t.Error("MCPServer misclassifies")
	}
	if !Permission("mcp__user-memory__memory_search") || !Permission("mcp__hindsight__hindsight_ingest_document") || Permission("mcp__plugin_backsight_memory__memory_search") || Permission("Bash(git *)") {
		t.Error("Permission misclassifies")
	}
}
