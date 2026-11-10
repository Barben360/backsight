package cli

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
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

// Codex is off: a hook run for it, through the real supervisor and child,
// answers nothing and records nothing, whatever its input.
func TestACodexHookDoesNothingYet(t *testing.T) {
	home := hookHome(t)
	childFromTestBinary(t, "")
	in := `{"session_id":"cx1","source":"startup","cwd":"` + jsonPathOf(filepath.Join(home, "billing")) + `"}`
	args := harness.Codex{}.HookArgs(harness.EventSessionStart)
	if code, out := runHookCmd(t, in, args[1:]...); code != 0 || out != "" {
		t.Errorf("exit %d, printed %q", code, out)
	}
	if _, ok := session.ReadLiveSession(pathsOf(home).SessionsDir(), "cx1"); ok {
		t.Error("a Codex hook recorded a session")
	}
	// The same input as Claude Code's is answered.
	if code, out := runHookCmd(t, in, harness.EventSessionStart); code != 0 || out == "" {
		t.Errorf("Claude Code's hook: exit %d, printed %q", code, out)
	}
}
