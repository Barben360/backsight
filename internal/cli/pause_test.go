package cli

import (
	"os"
	"os/exec"
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

// From a subdirectory of a git repo: the repo root is excluded, the existing
// entries are kept, a second pause adds nothing, --plan writes nothing.
func TestPauseProjectKeepsTheListAndUsesTheRepoRoot(t *testing.T) {
	home := devInstall(t, true)
	repo := filepath.Join(home, "work", "billing")
	_ = os.MkdirAll(filepath.Join(repo, "internal"), 0o700)
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[privacy]\nexclude_paths = [\"/srv/a # b\"]\n")...), 0o600)
	t.Chdir(filepath.Join(repo, "internal"))
	before, _ := os.ReadFile(cfg)
	if code, out, _ := run(t, "pause", "--project", "--plan"); code != output.OK || !strings.Contains(out, "would be paused") {
		t.Fatalf("plan: exit %d\n%s", code, out)
	}
	if after, _ := os.ReadFile(cfg); string(after) != string(before) {
		t.Fatal("--plan wrote the config")
	}
	run(t, "pause", "--project")
	if _, out, _ := run(t, "pause", "--project"); !strings.Contains(out, "already paused") {
		t.Errorf("second pause: %s", out)
	}
	got, _ := os.ReadFile(cfg)
	root, _ := filepath.EvalSymlinks(repo)
	if !strings.Contains(string(got), `exclude_paths = ["/srv/a # b", "`) || strings.Count(string(got), root) != 1 {
		t.Errorf("config:\n%s\nroot %s", got, root)
	}
	if code, _, _ := run(t, "pause", "--plan"); code != output.Usage {
		t.Errorf("--plan without --project: exit %d", code)
	}
}

// Removing the project's entry does not lift a parent one: resume says the
// directory is still not recorded.
func TestResumeProjectSaysWhenStillExcluded(t *testing.T) {
	home := devInstall(t, true)
	dir := filepath.Join(home, "clients", "acme")
	_ = os.MkdirAll(dir, 0o700)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[privacy]\nexclude_paths = [\"~/clients\", \""+dir+"\"]\n")...), 0o600)
	t.Chdir(dir)
	code, out, _ := run(t, "resume", "--project")
	if code != output.OK || !strings.Contains(out, "still not recorded") || !strings.Contains(out, "contains "+filepath.Join(home, "clients")+" (") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestPauseProjectRefusesWithoutAConfig(t *testing.T) {
	home := isolate(t)
	t.Chdir(home)
	code, _, errOut := run(t, "pause", "--project")
	if _, err := os.Stat(filepath.Join(home, "backsight", "config", "config.toml")); code != output.Usage || !strings.Contains(errOut, "not set up") || err == nil {
		t.Errorf("exit %d %q, config written: %v", code, errOut, err == nil)
	}
}

// pause and resume leave the paused stretch in the state, for flushes that
// run after the resume.
func TestPauseResumeRecordAStretch(t *testing.T) {
	home := isolate(t)
	t.Setenv("BACKSIGHT_SESSION", "s1")
	run(t, "pause")
	run(t, "resume")
	st, err := sessions(home).Load("s1")
	if err != nil || st.Paused || len(st.PausedSpans) != 1 || st.PausedSpans[0].To.IsZero() {
		t.Errorf("%+v %v", st, err)
	}
}
