package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fixture: home with a repo "billing" (subdir, linked worktree) and a plain dir.
func fixture(t *testing.T) (home string, r *Resolver) {
	home = t.TempDir()
	repo := filepath.Join(home, "work", "billing")
	_ = os.MkdirAll(filepath.Join(repo, "internal", "export"), 0o700)
	git(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "README.md"), "x")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")
	git(t, repo, "worktree", "add", "-q", filepath.Join(home, "wt", "billing-fix"), "-b", "fix")
	_ = os.MkdirAll(filepath.Join(home, "notes"), 0o700)
	return home, New(home, []string{".claude", filepath.Base(home)})
}

func TestProjectNames(t *testing.T) {
	home, r := fixture(t)
	cases := map[string]string{
		filepath.Join(home, "work", "billing"):                       "billing",
		filepath.Join(home, "work", "billing", "internal", "export"): "billing",
		filepath.Join(home, "wt", "billing-fix"):                     "billing", // linked worktree → main worktree name
		filepath.Join(home, "notes"):                                 "notes",
		home:                                                         ".claude",
		filepath.Join(home, ".claude"):                               ".claude",
		filepath.Join(home, ".claude", "skills", "x"):                ".claude",
		filepath.Join(home, "gone", "deleted-dir"):                   "deleted-dir",
	}
	for cwd, want := range cases {
		if got := r.Project(cwd); got != want {
			t.Errorf("%s: %q, want %q", cwd, got, want)
		}
	}
}

func TestBacksightTomlOverridesAndOptsOut(t *testing.T) {
	home, r := fixture(t)
	repo := filepath.Join(home, "work", "billing")
	write(t, filepath.Join(repo, ".backsight.toml"), "project = \"billing-api\"\n")
	if got := r.Project(filepath.Join(repo, "internal", "export")); got != "billing-api" {
		t.Errorf("override from a subdirectory: %q", got)
	}
	if out, _ := r.OptedOut(repo, nil); out {
		t.Error("a project name alone is not an opt-out")
	}
	write(t, filepath.Join(repo, ".backsight.toml"), "retain = false\n")
	r = New(home, []string{".claude"}) // fresh cache
	if out, why := r.OptedOut(filepath.Join(repo, "internal"), nil); !out || why == "" {
		t.Errorf("retain = false must opt out: %v %q", out, why)
	}
}

func TestExcludePaths(t *testing.T) {
	home, r := fixture(t)
	ex := []string{filepath.Join(home, "work") + "/", "~/secret-client"}
	if out, _ := r.OptedOut(filepath.Join(home, "work", "billing"), ex); !out {
		t.Error("prefix exclusion missed")
	}
	if out, _ := r.OptedOut(filepath.Join(home, "secret-client", "app"), ex); !out {
		t.Error("~ in exclude_paths must expand to the home directory")
	}
	if out, _ := r.OptedOut(filepath.Join(home, "secret-clientele"), ex); out {
		t.Error("a sibling with a longer name is not under the excluded path")
	}
	if out, _ := r.OptedOut(filepath.Join(home, "notes"), ex); out {
		t.Error("unrelated path excluded")
	}
}

func TestBrokenBacksightTomlIsReported(t *testing.T) {
	home, r := fixture(t)
	write(t, filepath.Join(home, "notes", ".backsight.toml"), "retain = maybe\n")
	if out, why := r.OptedOut(filepath.Join(home, "notes"), nil); !out || why == "" {
		t.Errorf("an unreadable .backsight.toml must fail closed (no retain): %v %q", out, why)
	}
}

// A nearer file naming the project must not hide a parent's retain = false.
func TestParentOptOutIsNeverHidden(t *testing.T) {
	home, r := fixture(t)
	repo := filepath.Join(home, "work", "billing")
	write(t, filepath.Join(repo, ".backsight.toml"), "retain = false\n")
	write(t, filepath.Join(repo, "internal", ".backsight.toml"), "project = \"billing-core\"\n")
	if out, _ := r.OptedOut(filepath.Join(repo, "internal", "export"), nil); !out {
		t.Error("the repository's retain = false was hidden by a subdirectory file")
	}
	if got := r.Project(filepath.Join(repo, "internal", "export")); got != "billing-core" {
		t.Errorf("nearest project: %q", got)
	}
}

// Outside a repository (or when git fails) the walk still reaches the home
// directory.
func TestOptOutAboveCwdWithoutGit(t *testing.T) {
	home, r := fixture(t)
	write(t, filepath.Join(home, "clients", ".backsight.toml"), "retain = false\n")
	_ = os.MkdirAll(filepath.Join(home, "clients", "acme", "app"), 0o700)
	if out, why := r.OptedOut(filepath.Join(home, "clients", "acme", "app"), nil); !out || !strings.Contains(why, "retain = false") {
		t.Errorf("%v %q", out, why)
	}
}

func TestSymlinkedCwdIntoExcludedPath(t *testing.T) {
	home, r := fixture(t)
	secret := filepath.Join(home, "secret-client")
	_ = os.MkdirAll(secret, 0o700)
	link := filepath.Join(home, "shortcut")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip(err)
	}
	if out, _ := r.OptedOut(link, []string{secret}); !out {
		t.Error("a symlink into an excluded path escaped the exclusion")
	}
	if out, _ := r.OptedOut(secret, []string{link}); !out {
		t.Error("an excluded symlink did not cover its target")
	}
}

func TestBareTildeAndEmptyCwd(t *testing.T) {
	home, r := fixture(t)
	if out, _ := r.OptedOut(filepath.Join(home, "notes"), []string{"~"}); !out {
		t.Error("~ must exclude the whole home directory")
	}
	if out, why := r.OptedOut("", nil); !out || why == "" {
		t.Error("a record without working directory must not be sent")
	}
}
