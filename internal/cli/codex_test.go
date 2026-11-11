package cli

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/session"
)

// The hook command line of the Codex contract selects Codex and its event
// through `backsight hook`'s own parsing, and the memory server takes the
// contract's --harness.
func TestCodexCommandLinesReachCodex(t *testing.T) {
	c := harness.Codex{}
	for _, e := range c.Events() {
		args := c.HookArgs(e.Event)
		if args[0] != "hook" {
			t.Fatalf("%v does not start with hook", args)
		}
		a, event, ok := hookArgs(args[1:])
		if !ok || reflect.TypeOf(a) != reflect.TypeOf(c) || event != e.Event {
			t.Errorf("%v: adapter %T, event %q, ok %v", args, a, event, ok)
		}
	}
	mcpArgs := c.MCPArgs()
	cmd := newMCPCmd()
	if mcpArgs[0] != "mcp" || cmd.ParseFlags(mcpArgs[1:]) != nil || cmd.Flags().Lookup("harness").Value.String() != harnessid.Codex {
		t.Errorf("mcp %v: --harness %q", mcpArgs, cmd.Flags().Lookup("harness").Value.String())
	}
}

// A Codex hook, through the real supervisor and child, reads Codex's
// payload (the recorded startup) and answers in the shape Codex reads; it
// records its session by id, never under the pid the child is given; the
// same payload to Claude Code's hook is answered by Claude Code's.
func TestACodexHookAnswersThroughTheSupervisor(t *testing.T) {
	home := hookHome(t)
	childFromTestBinary(t, "")
	t.Setenv(claudePIDEnv, "31337")
	raw, _ := json.Marshal(cxPayload(t, home, "SessionStart.startup", nil))
	args := harness.Codex{}.HookArgs(harness.EventSessionStart)
	code, out := runHookCmd(t, string(raw), args[1:]...)
	var h hookOut
	if code != 0 || json.Unmarshal([]byte(out), &h) != nil || h.Specific.Event != "SessionStart" || !strings.HasPrefix(h.Specific.Context, instructions.Guide) {
		t.Fatalf("exit %d, printed %q", code, out)
	}
	if l, ok := session.ReadLiveSession(pathsOf(home).SessionsDir(), cxRoot); !ok || l.Cwd != filepath.Join(home, "billing") {
		t.Errorf("no record by session id: %+v %v", l, ok)
	}
	if _, ok := session.ReadLive(pathsOf(home).SessionsDir(), 31337); ok {
		t.Error("a Codex hook recorded a session under a process id")
	}
	if code, out := runHookCmd(t, string(raw), harness.EventSessionStart); code != 0 || !strings.Contains(out, `"hookEventName":"SessionStart"`) {
		t.Errorf("Claude Code's hook: exit %d, printed %q", code, out)
	}
}
