package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Decision 0072 M5: doctor names a tracked .backsight.toml whose project
// or related backsight ignores, and says how to apply them on this machine.
func TestBacksightTomlCheckNamesTrackedFilesAndTheLocalFix(t *testing.T) {
	f := newFixture(t)
	repo := filepath.Join(f.home, "work", "cloned")
	other := filepath.Join(f.home, "work", "live")
	for _, r := range []string{repo, other} {
		_ = os.MkdirAll(r, 0o700)
		gitIn(t, r, "init", "-q")
	}
	f.env.Cwd = repo
	if c := checkBacksightToml(f.env); c.Status != OK {
		t.Errorf("no .backsight.toml: %+v", c)
	}
	toml := filepath.Join(repo, ".backsight.toml")
	put(t, toml, "project = \"victim\"\nretain = false\n", 0o600)
	if c := checkBacksightToml(f.env); c.Status != OK {
		t.Errorf("untracked file: %+v", c)
	}
	gitIn(t, repo, "add", ".backsight.toml")
	c := checkBacksightToml(f.env)
	if c.Status != Info || !strings.Contains(c.Summary, toml) || !strings.Contains(c.Summary, "project and related are ignored") {
		t.Fatalf("tracked file: %+v", c)
	}
	if strings.Contains(c.Summary+c.Detail+c.Fix, "victim") {
		t.Errorf("the ignored name is printed: %+v", c)
	}
	for _, want := range []string{"git rm --cached", ".git/info/exclude", "retain = false"} {
		if !strings.Contains(c.Fix+c.Detail, want) {
			t.Errorf("fix/detail lacks %q: %+v", want, c)
		}
	}
	// a live session's directory counts too, once per file
	liveToml := filepath.Join(other, ".backsight.toml")
	put(t, liveToml, "related = [\"x\"]\n", 0o600)
	gitIn(t, other, "add", ".backsight.toml")
	put(t, filepath.Join(f.env.Dirs.SessionsDir(), "by-pid", "42.json"), `{"session_id":"s1","cwd":"`+other+`"}`, 0o600)
	put(t, filepath.Join(f.env.Dirs.SessionsDir(), "by-pid", "43.json"), `{"session_id":"s2","cwd":"`+repo+`"}`, 0o600)
	c = checkBacksightToml(f.env)
	if c.Status != Info || !strings.Contains(c.Summary, "2 .backsight.toml files") || !strings.Contains(c.Detail, liveToml) || strings.Count(c.Detail, toml) != 1 {
		t.Errorf("two files: %+v", c)
	}
}

func TestDoctorRunsTheBacksightTomlCheck(t *testing.T) {
	f := newFixture(t)
	f.env.Cwd = t.TempDir()
	expect(t, check(t, f.run(), "backsight-toml"), OK, "no .backsight.toml")
}
