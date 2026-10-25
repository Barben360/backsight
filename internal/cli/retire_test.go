package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

// referenceSettings is shaped like the author's settings.json before the
// cut-over: reference hooks beside the user's own (rtk), one group mixing
// both, and a reference permission rule.
const referenceSettings = `{
  "permissions": {
    "allow": ["mcp__user-memory__memory_search", "Bash(git *)"]
  },
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "rtk hook claude"}]},
      {"matcher": "AskUserQuestion", "hooks": [{"type": "command", "command": "python3 \"$HOME/.claude/hooks/memory-search.py\""}]}
    ],
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "python3 \"$HOME/.claude/hooks/memory-search.py\" plugin-sessionstart \"/home/u/.hindsight/coding-agents/dist/claude-sessionstart-hook.js\""}]}
    ],
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "node \"/home/u/.hindsight/coding-agents/dist/claude-hook.js\""}, {"type": "command", "command": "echo mine"}]}
    ]
  },
  "enabledPlugins": {"superpowers@claude-plugins-official": true}
}
`

const referenceClaudeJSON = `{
  "numStartups": 12,
  "mcpServers": {
    "vfs": {"command": "vfs", "args": ["mcp"]},
    "hindsight": {"command": "node", "args": ["/home/u/.hindsight/coding-agents/dist/mcp-server.js"], "env": {"HINDSIGHT_API_TOKEN": "tok"}},
    "user-memory": {"command": "python3", "args": ["/home/u/.claude/hooks/hindsight-memory-mcp.py"]}
  }
}
`

func referenceProfile(t *testing.T) (settings, state string) {
	t.Helper()
	home := isolate(t)
	settings, state = filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude.json")
	_ = os.MkdirAll(filepath.Dir(settings), 0o700)
	_ = os.WriteFile(settings, []byte(referenceSettings), 0o600)
	_ = os.WriteFile(state, []byte(referenceClaudeJSON), 0o600)
	return settings, state
}

func TestClientRetireRemovesOnlyReferencePieces(t *testing.T) {
	settings, state := referenceProfile(t)
	sys := withSys(t, "linux")
	sys.out = retroTimer(os.Getenv("HOME"))
	code, out, errOut := run(t, "client", "retire", "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if strings.Contains(out, "tok\\\"") || strings.Contains(out, "HINDSIGHT_API_TOKEN") {
		t.Errorf("a removed MCP server's token reached the output:\n%s", out)
	}
	var s struct {
		Permissions struct{ Allow []string } `json:"permissions"`
		Hooks       map[string][]struct {
			Matcher string                     `json:"matcher"`
			Hooks   []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	raw, _ := os.ReadFile(settings)
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("settings.json broken: %v\n%s", err, raw)
	}
	if strings.Join(s.Permissions.Allow, ",") != "Bash(git *)" {
		t.Errorf("permissions %v", s.Permissions.Allow)
	}
	if _, ok := s.Hooks["SessionStart"]; ok {
		t.Error("an event holding only reference hooks must go")
	}
	if pre := s.Hooks["PreToolUse"]; len(pre) != 1 || pre[0].Hooks[0].Command != "rtk hook claude" {
		t.Errorf("PreToolUse %+v: the user's rtk hook must stay alone", pre)
	}
	if ups := s.Hooks["UserPromptSubmit"]; len(ups) != 1 || len(ups[0].Hooks) != 1 || ups[0].Hooks[0].Command != "echo mine" {
		t.Errorf("a mixed group must keep the user's command: %+v", ups)
	}
	if !s.EnabledPlugins["superpowers@claude-plugins-official"] {
		t.Error("an unrelated plugin was touched")
	}
	var cj struct {
		NumStartups int                        `json:"numStartups"`
		MCPServers  map[string]json.RawMessage `json:"mcpServers"`
	}
	raw, _ = os.ReadFile(state)
	_ = json.Unmarshal(raw, &cj)
	if len(cj.MCPServers) != 1 || cj.MCPServers["vfs"] == nil || cj.NumStartups != 12 {
		t.Errorf("~/.claude.json: %s", raw)
	}
	if !contains(sys.calls, "systemctl --user disable --now memory-retro.timer") {
		t.Errorf("the retro timer was not stopped: %v", sys.calls)
	}
}

func TestClientRetirePlanWritesNothing(t *testing.T) {
	settings, state := referenceProfile(t)
	sys := withSys(t, "linux")
	sys.out = retroTimer(os.Getenv("HOME"))
	code, out, _ := run(t, "client", "retire", "--plan")
	if code != output.OK || !strings.Contains(out, "would remove") || !strings.Contains(out, "mcpServers.user-memory") || !strings.Contains(out, "would stop systemd user timer") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if a, _ := os.ReadFile(settings); string(a) != referenceSettings {
		t.Error("--plan wrote settings.json")
	}
	if a, _ := os.ReadFile(state); string(a) != referenceClaudeJSON {
		t.Error("--plan wrote ~/.claude.json")
	}
	if contains(sys.calls, "systemctl --user disable --now memory-retro.timer") {
		t.Error("--plan stopped the timer")
	}
}

func TestClientRetireIsUndoneByConfigRestore(t *testing.T) {
	settings, state := referenceProfile(t)
	withSys(t, "linux")
	_, out, _ := run(t, "client", "retire", "--json")
	var env struct {
		Data retireResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if env.Data.Run == "" {
		t.Fatalf("no run recorded\n%s", out)
	}
	if code, out, errOut := run(t, "client", "config", "restore", env.Data.Run); code != output.OK {
		t.Fatalf("restore: exit %d %s %s", code, out, errOut)
	}
	if a, _ := os.ReadFile(settings); string(a) != referenceSettings {
		t.Errorf("settings.json not restored byte for byte:\n%s", a)
	}
	if a, _ := os.ReadFile(state); string(a) != referenceClaudeJSON {
		t.Error("~/.claude.json not restored byte for byte")
	}
	// Retiring again after nothing is wired changes nothing.
	_ = os.WriteFile(settings, []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"rtk hook claude"}]}]}}`), 0o600)
	_ = os.WriteFile(state, []byte(`{"mcpServers":{}}`), 0o600)
	if code, out, _ := run(t, "client", "retire"); code != output.OK || !strings.Contains(out, "no reference piece") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// The retro timer is machine-wide and serves the default profile: a setup
// of another profile (the dev install's) must leave it running.
func TestClientRetireLeavesTheRetroTimerToTheDefaultProfile(t *testing.T) {
	home := isolate(t)
	sys := withSys(t, "linux")
	sys.out = retroTimer(os.Getenv("HOME"))
	other := filepath.Join(home, ".claude-dev")
	_ = os.MkdirAll(other, 0o700)
	code, out, _ := run(t, "client", "retire", "--config-dir", other)
	if code != output.OK || !strings.Contains(out, "no reference piece") {
		t.Errorf("exit %d\n%s", code, out)
	}
	for _, c := range sys.calls {
		if strings.Contains(c, "memory-retro.timer") {
			t.Errorf("another profile's retire touched the retro timer: %v", sys.calls)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", other)
	if code, out, _ := run(t, "client", "retire", "--plan"); code != output.OK || strings.Contains(out, "timer") {
		t.Errorf("CLAUDE_CONFIG_DIR profile: exit %d\n%s", code, out)
	}
}

func TestClientRetireRemovesRulesOnlyForServersItRemoves(t *testing.T) {
	settings, state := referenceProfile(t)
	withSys(t, "linux")
	// An HTTP hindsight server (no command) is not the reference plugin:
	// it stays, and so do its rules. A project-level user-memory server
	// goes, with its bare rule.
	_ = os.WriteFile(settings, []byte(`{"permissions":{"allow":["mcp__hindsight__recall","mcp__user-memory","mcp__user-memory__memory_search","mcp__vfs__search"]}}`), 0o600)
	_ = os.WriteFile(state, []byte(`{"mcpServers":{"hindsight":{"type":"http","url":"https://h.example/mcp"}},
  "projects":{"/home/u/app":{"mcpServers":{"user-memory":{"command":"python3","args":["/home/u/.claude/hooks/hindsight-memory-mcp.py"]},"db":{"command":"pg-mcp"}}}}}`), 0o600)
	if code, out, errOut := run(t, "client", "retire"); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	_, allow, _ := claudeSettings(t, settings)
	if strings.Join(allow, ",") != "mcp__hindsight__recall,mcp__vfs__search" {
		t.Errorf("permissions %v", allow)
	}
	raw, _ := os.ReadFile(state)
	if strings.Contains(string(raw), "user-memory") || !strings.Contains(string(raw), `"db"`) || !strings.Contains(string(raw), "https://h.example/mcp") {
		t.Errorf("~/.claude.json: %s", raw)
	}
}

// retroTimer fakes systemctl for an enabled retro timer whose service runs
// home's default profile's script, as on the author's machine.
func retroTimer(home string) map[string]string {
	return map[string]string{
		"systemctl --user is-enabled memory-retro.timer": "enabled\n",
		"systemctl --user show -p ExecStart memory-retro.service": "ExecStart={ path=/usr/bin/python3 ; argv[]=/usr/bin/python3 " +
			filepath.Join(home, ".claude", "hooks", "memory-retro.py") + " run ; ignore_errors=no }\n",
	}
}

// A HOME moved for a test install has the default profile's path, but the
// machine's real timer runs another profile's script: it is left alone.
func TestClientRetireLeavesTheTimerToTheProfileThatRunsIt(t *testing.T) {
	isolate(t)
	sys := withSys(t, "linux")
	sys.out = retroTimer("/home/the-real-user")
	if code, out, _ := run(t, "client", "retire"); code != output.OK || !strings.Contains(out, "no reference piece") {
		t.Errorf("exit %d\n%s", code, out)
	}
	for _, c := range sys.calls {
		if strings.Contains(c, "memory-retro.timer") && !strings.Contains(c, "is-enabled") {
			t.Errorf("the timer was touched: %v", sys.calls)
		}
	}
}
