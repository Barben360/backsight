package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

// wireCodexRecord records Codex as wired (what client install --codex
// writes).
func wireCodexRecord(t *testing.T, home string) {
	t.Helper()
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.ClientFor(harnessid.Codex).PluginVersion = "1.0.0+abc"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// codexHookArgs are the backsight events of Codex's hooks, once each.
func codexHookArgs() []string {
	var out []string
	for _, e := range codex.Events() {
		if !slices.Contains(out, e.Event) {
			out = append(out, e.Event)
		}
	}
	return out
}

// verify --codex runs each of Codex's hooks through the real supervisor and
// the real memory server in Codex's mode, its calls naming their session
// in _meta, then the round trip; Claude Code's own hook (the failure one)
// is not among them.
func TestVerifyCodexRunsCodexsHooksItsServerAndARoundTrip(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	wireCodexRecord(t, home)
	asShim(t)
	fastVerify(t)
	documentUnits[verifyDoc] = 2
	recallAnswer = `[{"id":"m1","text":"The user chose a verification codeword.","type":"world","scores":{"reranker":0.9}}]`
	appendConfig(t, home, "[recall]\nfailure_pitfalls = true")

	code, out, errOut := run(t, "verify", "--codex", "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	checks := parseVerify(t, out)
	for _, arg := range codexHookArgs() {
		if c := checks["hook "+arg]; c.Status != "ok" {
			t.Errorf("hook %s: %+v", arg, c)
		}
	}
	if len(codexHookArgs()) != 8 {
		t.Errorf("Codex's hooks %v", codexHookArgs())
	}
	if c, ran := checks["hook post-tool-use-failure"]; ran {
		t.Errorf("ran a hook Codex does not wire: %+v", c)
	}
	if c := checks["mcp"]; c.Status != "ok" || !strings.Contains(c.Summary, "and 4 tools") || !strings.Contains(c.Summary, "session read from its _meta") {
		t.Errorf("mcp: %+v", c)
	}
	if checks["round_trip"].Status != "ok" || retained.Load() != 1 {
		t.Errorf("round trip: %+v", checks["round_trip"])
	}
	var env struct {
		Data verifyResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if env.Data.Client != "codex" {
		t.Errorf("client %q", env.Data.Client)
	}
	if left, _ := filepath.Glob(filepath.Join(pathsOf(home).SessionsDir(), "backsight-verify*")); len(left) > 0 {
		t.Errorf("synthetic session state left behind: %v", left)
	}
	// Claude Code's verify is as it was: its hooks, no client named.
	code, out, _ = run(t, "verify", "--no-round-trip", "--json")
	if c := parseVerify(t, out); code != output.OK || c["hook post-tool-use-failure"].Status != "ok" || strings.Contains(out, `"client"`) {
		t.Errorf("Claude Code's verify: exit %d %s", code, out)
	}
}

// The shim runs as Codex runs it: `hook --harness codex <event>` with a
// Codex-shaped event on stdin, and `mcp --harness codex`.
func TestVerifyCodexRunsTheShimAsCodex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a sh script stands for the shim")
	}
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	wireCodexRecord(t, home)
	logDir := t.TempDir()
	shim := filepath.Join(t.TempDir(), "backsight")
	// The server's run ends at once: no handshake, nothing waited for.
	script := "#!/bin/sh\nn=$(ls " + logDir + " | wc -l)\necho \"$*\" > " + logDir + "/$n.args\n[ \"$1\" = mcp ] && exit 0\ncat > " + logDir + "/$n.in\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	withShim(t, shim)
	code, out, _ := run(t, "verify", "--codex", "--no-round-trip", "--json")
	checks := parseVerify(t, out)
	if code != output.Blocked || checks["mcp"].Status != "fail" || checks["hook stop"].Status != "ok" {
		t.Fatalf("exit %d %s", code, out)
	}
	ins := map[string]map[string]any{}
	var mcpRan bool
	files, _ := filepath.Glob(filepath.Join(logDir, "*.args"))
	for _, f := range files {
		args, _ := os.ReadFile(f)
		words := strings.Fields(string(args))
		if strings.Join(words, " ") == "mcp --harness codex" {
			mcpRan = true
			continue
		}
		if len(words) != 4 || words[0] != "hook" || words[1] != "--harness" || words[2] != "codex" {
			t.Errorf("shim run as %q", args)
			continue
		}
		raw, _ := os.ReadFile(strings.TrimSuffix(f, ".args") + ".in")
		var in map[string]any
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Errorf("%s: %v %s", words[3], err, raw)
		}
		ins[words[3]] = in
	}
	if !mcpRan {
		t.Error("the server was not started as mcp --harness codex")
	}
	if len(ins) != len(codexHookArgs()) {
		t.Errorf("hooks run %d, want %d", len(ins), len(codexHookArgs()))
	}
	if in := ins["user-prompt-submit"]; in["turn_id"] == nil || in["model"] == nil || in["hook_event_name"] != "UserPromptSubmit" {
		t.Errorf("UserPromptSubmit %v", in)
	}
	if in := ins["pre-tool-use"]; in["tool_name"] != "request_user_input" || in["tool_use_id"] == nil {
		t.Errorf("PreToolUse %v", in)
	}
	post := ins["post-tool-use"]
	resp, _ := post["tool_response"].(map[string]any)
	if post["tool_name"] != "mcp__backsight__memory_search" || resp["content"] == nil {
		t.Errorf("PostToolUse %v", post)
	}
	if in := ins["session-start"]; in["source"] != "startup" || !strings.Contains(in["transcript_path"].(string), "rollout-") {
		t.Errorf("SessionStart %v", in)
	}
}

// A server that does not read each call's session from its _meta (the
// shim starting Claude Code's server) fails the check.
func TestVerifyCodexFailsAServerBlindToMeta(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a sh script stands for the shim")
	}
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	wireCodexRecord(t, home)
	asShim(t)
	exe, _ := os.Executable()
	shim := filepath.Join(t.TempDir(), "backsight")
	script := "#!/bin/sh\nif [ \"$1\" = mcp ]; then exec '" + exe + "' mcp; fi\nexec '" + exe + "' \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	withShim(t, shim)
	code, out, _ := run(t, "verify", "--codex", "--no-round-trip", "--json")
	c := parseVerify(t, out)["mcp"]
	if code != output.Blocked || c.Status != "fail" || !strings.Contains(c.Summary, "_meta") {
		t.Errorf("exit %d %+v", code, c)
	}
}

// Broken hooks and server fail, named; with feedback off the server has no
// memory_credit and the session is not checked, which is no failure.
func TestVerifyCodexCatchesBrokenHooksAndFeedbackOff(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	wireCodexRecord(t, home)
	if _, err := os.Stat("/bin/false"); err != nil {
		t.Skip("/bin/false missing")
	}
	withShim(t, "/bin/false")
	code, out, _ := run(t, "verify", "--codex", "--no-round-trip", "--json")
	checks := parseVerify(t, out)
	if code != output.Blocked || checks["hook session-start"].Status != "fail" || !strings.Contains(checks["hook session-start"].Summary, "exit:") || checks["mcp"].Status != "fail" {
		t.Errorf("exit %d %s", code, out)
	}
	asShim(t)
	appendConfig(t, home, "[feedback]\nenabled = false")
	code, out, _ = run(t, "verify", "--codex", "--no-round-trip", "--json")
	if c := parseVerify(t, out)["mcp"]; code != output.OK || c.Status != "ok" || !strings.Contains(c.Summary, "and 3 tools") || !strings.Contains(c.Summary, "not checked") {
		t.Errorf("feedback off: exit %d %+v", code, c)
	}
}

// verify --codex needs Codex wired; it takes neither --live nor Claude
// Code's --config-dir.
func TestVerifyCodexRefusals(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	asShim(t)
	code, out, _ := run(t, "verify", "--codex", "--json")
	if code != output.NeedsUser || !strings.Contains(out, "backsight client install --codex") {
		t.Errorf("not wired: exit %d %s", code, out)
	}
	wireCodexRecord(t, home)
	if code, out, _ := run(t, "verify", "--codex", "--live", "--json"); code != output.Usage || !strings.Contains(out, "drop --codex") {
		t.Errorf("--live: exit %d %s", code, out)
	}
	if code, _, _ := run(t, "verify", "--codex", "--config-dir", home); code != output.Usage {
		t.Errorf("--config-dir: exit %d", code)
	}
}
