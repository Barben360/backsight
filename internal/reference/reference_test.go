package reference

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	return home
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
	in, err := Discover(home)
	if err != nil || in == nil {
		t.Fatalf("%v %v", in, err)
	}
	if in.URL != "http://127.0.0.1:8888" || in.Bank != "coding-agent::memoire" || in.Token != "fakevalue" || in.ObservationScopes != "combined" {
		t.Errorf("%+v", in)
	}
	want := map[string]string{
		"npm-plugin-hook|settings.json SessionStart":            "M1",
		"python-search-hook|settings.json SessionStart":         "M1",
		"npm-plugin-hook|settings.json UserPromptSubmit":        "M1",
		"python-search-hook|settings.json UserPromptSubmit":     "M1",
		"python-search-hook|settings.json PreToolUse":           "M1",
		"npm-plugin-hook|settings.json Stop":                    "M2",
		"npm-plugin-mcp|.claude.json mcpServers.hindsight":      "M1",
		"python-memory-mcp|.claude.json mcpServers.user-memory": "M1",
		"python-retro|.claude/hooks/memory-retro.py":            "M3",
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
	in, err := Discover(t.TempDir())
	if err != nil || in != nil {
		t.Errorf("empty home: %+v %v", in, err)
	}
}

// Wiring without the plugin config is still a reference install to report.
func TestDiscoverHooksWithoutPluginConfig(t *testing.T) {
	home := t.TempDir()
	put(t, home, ".claude/settings.json", `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"python3 ~/.claude/hooks/hindsight-multibank.py"}]}]}}`)
	in, err := Discover(home)
	if err != nil || in == nil || ids(in)["legacy-multibank-hook|settings.json UserPromptSubmit"] != "M1" || in.URL != "" {
		t.Errorf("%+v %v", in, err)
	}
}

func TestDiscoverReportsBrokenFiles(t *testing.T) {
	home := t.TempDir()
	put(t, home, ".claude/settings.json", `{"hooks": `)
	if _, err := Discover(home); err == nil || !strings.Contains(err.Error(), "settings.json") {
		t.Errorf("want an error naming settings.json, got %v", err)
	}
}

func TestTokenNeverPrinted(t *testing.T) {
	in, _ := Discover(authorHome(t))
	js, _ := json.Marshal(in)
	for _, out := range []string{fmt.Sprintf("%v %+v %#v", in, in, *in), string(js)} {
		if strings.Contains(out, "fakevalue") {
			t.Errorf("token leaked: %s", out)
		}
	}
}
