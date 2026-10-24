package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/hook"
)

// childFromTestBinary makes the supervisor's child this test binary, which
// TestMain turns into `backsight hook-child <event>`.
func childFromTestBinary(t *testing.T, chaos string) {
	t.Helper()
	exe, _ := os.Executable()
	old := hookChildCommand
	hookChildCommand = func(ctx context.Context, event string) *exec.Cmd {
		c := exec.CommandContext(ctx, exe, "-test.run=^$", "hook-child", event)
		c.Env = append(os.Environ(), "BACKSIGHT_TEST_HOOK_CHILD=1", hook.ChaosEnv+"="+chaos)
		return c
	}
	t.Cleanup(func() { hookChildCommand = old })
}

func runHookCmd(t *testing.T, input string, args ...string) (int, string) {
	t.Helper()
	old := hookStdin
	hookStdin = strings.NewReader(input)
	t.Cleanup(func() { hookStdin = old })
	var out, errb bytes.Buffer
	code := Execute(append([]string{"hook"}, args...), &out, &errb)
	return code, out.String() + errb.String()
}

// Whatever the child does, `backsight hook` exits 0 and prints nothing but
// valid hook JSON; failures land in telemetry (SPEC §6.1, chaos per event).
func TestHookAlwaysExitsZero(t *testing.T) {
	home := isolate(t)
	events := []string{"session-start", "user-prompt-submit", "pre-tool-use", "post-tool-use", "subagent-start", "subagent-stop", "stop", "session-end"}
	for _, chaos := range []string{"panic", "fatal", "kill", "exit2"} {
		childFromTestBinary(t, chaos)
		for _, ev := range events {
			if code, out := runHookCmd(t, `{"session_id":"s1","source":"resume"}`, ev); code != 0 || out != "" {
				t.Errorf("%s/%s: exit %d, printed %q", chaos, ev, code, out)
			}
		}
	}
	files, _ := filepath.Glob(filepath.Join(pathsOf(home).TelemetryDir(), "*.jsonl"))
	var all string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		all += string(b)
	}
	for _, kind := range []string{`"kind":"panic"`, `"kind":"fatal"`, `"kind":"killed"`, `"kind":"exit"`} {
		if !strings.Contains(all, kind) || !strings.Contains(all, `"event":"hook_failure"`) {
			t.Errorf("telemetry lacks %s:\n%s", kind, all)
		}
	}
}

func TestHookWithoutAnEventOrWithJunkArgsExitsZero(t *testing.T) {
	isolate(t)
	childFromTestBinary(t, "")
	for _, args := range [][]string{nil, {"no-such-event"}, {"--bogus-flag", "x"}} {
		if code, out := runHookCmd(t, "not json at all", args...); code != 0 || out != "" {
			t.Errorf("%v: exit %d %q", args, code, out)
		}
	}
}
