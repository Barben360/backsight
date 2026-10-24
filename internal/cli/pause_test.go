package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/session"
)

func TestHooksRecordTheLiveSessionUnderClaudesPID(t *testing.T) {
	home := hookHome(t)
	t.Setenv(claudePIDEnv, "31337")
	fire(t, "user-prompt-submit", map[string]any{"session_id": "s1", "prompt": "ok", "cwd": filepath.Join(home, "billing")})
	l, ok := session.ReadLive(pathsOf(home).SessionsDir(), 31337)
	if !ok || l.SessionID != "s1" || l.Project != "billing" || l.Cwd != filepath.Join(home, "billing") {
		t.Errorf("%+v %v", l, ok)
	}
	c := hookChildCommand(t.Context(), "stop")
	found := false
	for _, kv := range c.Env {
		if strings.HasPrefix(kv, claudePIDEnv+"=") {
			found = true
		}
	}
	if !found {
		t.Error("the supervisor does not pass Claude Code's pid to the child")
	}
}

func TestPauseAndResumeTheSession(t *testing.T) {
	home := isolate(t)
	t.Setenv("BACKSIGHT_SESSION", "s1")
	if code, out, _ := run(t, "pause"); code != output.OK || !strings.Contains(out, "session s1: paused") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if p, _ := session.Paused(pathsOf(home).SessionsDir(), "s1"); !p {
		t.Error("not paused")
	}
	if _, out, _ := run(t, "pause"); !strings.Contains(out, "already paused") {
		t.Errorf("second pause: %s", out)
	}
	run(t, "resume")
	if p, _ := session.Paused(pathsOf(home).SessionsDir(), "s1"); p {
		t.Error("still paused")
	}
	t.Setenv("BACKSIGHT_SESSION", "")
	if code, _, _ := run(t, "pause"); code != output.Usage {
		t.Errorf("no session: exit %d", code)
	}
}

func TestPauseProjectEditsTheExcludePaths(t *testing.T) {
	home := devInstall(t, true)
	dir := filepath.Join(home, "secret-project")
	_ = os.MkdirAll(dir, 0o700)
	t.Chdir(dir)
	if code, out, _ := run(t, "pause", "--project"); code != output.OK || !strings.Contains(out, "privacy.exclude_paths updated") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	if !strings.Contains(string(raw), `exclude_paths = ["`+dir+`"]`) {
		t.Fatalf("config:\n%s", raw)
	}
	if code, _, errOut := run(t, "store", "--kind", "decision", "secret plan"); code != output.Blocked || !strings.Contains(errOut, "excluded from memory") {
		t.Errorf("store inside a paused project: exit %d %q", code, errOut)
	}
	run(t, "resume", "--project")
	raw, _ = os.ReadFile(cfg)
	if strings.Contains(string(raw), dir) {
		t.Errorf("not removed:\n%s", raw)
	}
}
