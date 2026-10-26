package project

import (
	"os"
	"path/filepath"
	"testing"
)

// Explicit tells a name a .backsight.toml set from one git or the
// directory gave: doctor's remotes check does not warn on the first
// (decision 0045, 0067 block 4 review rules).
func TestExplicitIsTrueOnlyForANameFromBacksightToml(t *testing.T) {
	home, r := fixture(t)
	repo := filepath.Join(home, "work", "billing")
	named := filepath.Join(home, "notes", "named")
	_ = os.MkdirAll(filepath.Join(named, "sub"), 0o700)
	write(t, filepath.Join(named, ".backsight.toml"), "project = \"journal\"\n")
	optOut := filepath.Join(home, "notes", "private")
	write(t, filepath.Join(optOut, ".backsight.toml"), "retain = false\n") // no project key
	home2 := filepath.Join(home, ".claude", "x")
	write(t, filepath.Join(home2, ".backsight.toml"), "project = \"other\"\n") // transverse wins
	cases := map[string]bool{
		repo:                         false, // git main worktree
		filepath.Join(home, "notes"): false, // basename
		named:                        true,
		filepath.Join(named, "sub"):  true, // a parent's file
		optOut:                       false,
		home2:                        false,
		home:                         false,
	}
	for cwd, want := range cases {
		if got := r.Explicit(cwd); got != want {
			t.Errorf("%s: %v, want %v", cwd, got, want)
		}
	}
	// The repository renamed: project = its old name in its own file.
	write(t, filepath.Join(repo, ".backsight.toml"), "project = \"billing-old\"\n")
	if !r.Explicit(filepath.Join(repo, "internal")) || r.Project(repo) != "billing-old" {
		t.Errorf("rename: explicit %v, project %q", r.Explicit(repo), r.Project(repo))
	}
}
