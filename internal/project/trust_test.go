package project

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A .backsight.toml git tracks came with the repository: anyone who
// published it chose its project and related, so only what protects
// (retain = false) is honoured (decision 0072 M5).
func TestTrackedBacksightTomlKeepsOnlyWhatProtects(t *testing.T) {
	home, _ := fixture(t)
	repo := filepath.Join(home, "work", "billing")
	toml := filepath.Join(repo, ".backsight.toml")
	write(t, toml, "project = \"victim\"\nrelated = [\"acme-internal\", \"dotfiles\"]\n")
	git(t, repo, "add", ".backsight.toml")
	r := New(home, []string{".claude"})
	sub := filepath.Join(repo, "internal", "export")
	if got := r.Project(sub); got != "billing" {
		t.Errorf("a tracked project key named the session %q, want the git name billing", got)
	}
	if name, strong := r.Named(repo); name != "billing" || !strong {
		t.Errorf("Named = %q %v, want billing true (from git)", name, strong)
	}
	if got := r.Related(sub); len(got) != 0 {
		t.Errorf("a tracked related was honoured: %q", got)
	}
	if r.Explicit(repo) {
		t.Error("a tracked project key must not count as an explicit name")
	}
	if got := r.Ignored(sub); !slices.Equal(got, []string{toml}) {
		t.Errorf("Ignored = %q, want [%s]", got, toml)
	}
	// the same file committed, then also the case where only HEAD has it
	git(t, repo, "commit", "-q", "-m", "toml")
	r = New(home, []string{".claude"})
	if got := r.Project(repo); got != "billing" {
		t.Errorf("committed project key honoured: %q", got)
	}
	git(t, repo, "rm", "-q", "--cached", ".backsight.toml")
	r = New(home, []string{".claude"})
	if got := r.Project(repo); got != "billing" {
		t.Errorf("a file still in HEAD (removal not committed) was trusted: %q", got)
	}
	git(t, repo, "commit", "-q", "-m", "untrack")
	r = New(home, []string{".claude"})
	if got := r.Project(repo); got != "victim" {
		t.Errorf("once untracked the file is the user's own: %q, want victim", got)
	}
	if got := r.Ignored(repo); len(got) != 0 {
		t.Errorf("an untracked file listed as ignored: %q", got)
	}
}

func TestTrackedRetainFalseIsHonoured(t *testing.T) {
	home, _ := fixture(t)
	repo := filepath.Join(home, "work", "billing")
	write(t, filepath.Join(repo, ".backsight.toml"), "project = \"victim\"\nretain = false\n")
	git(t, repo, "add", ".backsight.toml")
	git(t, repo, "commit", "-q", "-m", "opt out")
	r := New(home, []string{".claude"})
	if out, why := r.OptedOut(filepath.Join(repo, "internal"), nil); !out || !strings.Contains(why, "retain = false") {
		t.Errorf("a tracked retain = false must opt out: %v %q", out, why)
	}
	if got := r.Project(repo); got != "billing" {
		t.Errorf("project = %q next to the opt-out, want billing", got)
	}
	// a broken tracked file still fails closed
	write(t, filepath.Join(repo, ".backsight.toml"), "retain = maybe\n")
	r = New(home, []string{".claude"})
	if out, _ := r.OptedOut(repo, nil); !out {
		t.Error("a broken tracked file must fail closed")
	}
}

func TestUntrackedBacksightTomlIsFullyHonoured(t *testing.T) {
	home, r := fixture(t)
	repo := filepath.Join(home, "work", "billing")
	// listed in .git/info/exclude: git ignores it, the user keeps it
	write(t, filepath.Join(repo, ".git", "info", "exclude"), ".backsight.toml\n")
	write(t, filepath.Join(repo, ".backsight.toml"), "project = \"billing-api\"\nrelated = [\"atlas\"]\n")
	if got := r.Project(repo); got != "billing-api" {
		t.Errorf("project = %q, want billing-api", got)
	}
	if got := r.Related(repo); !slices.Equal(got, []string{"atlas"}) {
		t.Errorf("related = %q, want [atlas]", got)
	}
	if !r.Explicit(repo) {
		t.Error("an untracked project key is explicit")
	}
	if got := r.Ignored(repo); len(got) != 0 {
		t.Errorf("Ignored = %q, want none", got)
	}
}

// A file the user trusted becomes tracked (git add): the next resolver
// (the next hook) stops honouring its project and related.
func TestFileThatBecomesTrackedStopsBeingTrusted(t *testing.T) {
	home, r := fixture(t)
	repo := filepath.Join(home, "work", "billing")
	write(t, filepath.Join(repo, ".backsight.toml"), "project = \"billing-api\"\nrelated = [\"atlas\"]\n")
	if got := r.Project(repo); got != "billing-api" {
		t.Fatalf("untracked: project = %q", got)
	}
	git(t, repo, "add", ".backsight.toml")
	r = New(home, []string{".claude"})
	if got := r.Project(repo); got != "billing" {
		t.Errorf("tracked now: project = %q, want billing", got)
	}
	if got := r.Related(repo); len(got) != 0 {
		t.Errorf("tracked now: related = %q", got)
	}
}

// A tracked nearer file is ignored as if its keys were absent: the user's
// untracked file further up still applies, and a linked worktree checks
// its own index.
func TestTrackedNearerFileDoesNotHideAnUntrackedOne(t *testing.T) {
	home, _ := fixture(t)
	repo := filepath.Join(home, "work", "billing")
	write(t, filepath.Join(repo, ".backsight.toml"), "project = \"billing-api\"\nrelated = [\"atlas\"]\n")
	inner := filepath.Join(repo, "internal", ".backsight.toml")
	write(t, inner, "project = \"victim\"\nrelated = []\n")
	git(t, repo, "add", "internal/.backsight.toml")
	r := New(home, []string{".claude"})
	cwd := filepath.Join(repo, "internal", "export")
	if got := r.Project(cwd); got != "billing-api" {
		t.Errorf("project = %q, want billing-api from the untracked root file", got)
	}
	if got := r.Related(cwd); !slices.Equal(got, []string{"atlas"}) {
		t.Errorf("related = %q, want [atlas]: a tracked related = [] must not clear it", got)
	}
	if got := r.Ignored(cwd); !slices.Equal(got, []string{inner}) {
		t.Errorf("Ignored = %q, want [%s]", got, inner)
	}
	// the linked worktree has its own index: the file staged in the main
	// checkout is untracked there
	wt := filepath.Join(home, "wt", "billing-fix")
	write(t, filepath.Join(wt, ".backsight.toml"), "project = \"billing-api\"\n")
	if got := r.Project(wt); got != "billing-api" {
		t.Errorf("worktree project = %q, want billing-api", got)
	}
}

func TestNoGitRepositoryBehavesAsUntracked(t *testing.T) {
	home, r := fixture(t)
	dir := filepath.Join(home, "notes", "sub")
	_ = os.MkdirAll(dir, 0o700)
	write(t, filepath.Join(home, "notes", ".backsight.toml"), "project = \"journal\"\nrelated = [\"atlas\"]\n")
	if got := r.Project(dir); got != "journal" {
		t.Errorf("project = %q, want journal", got)
	}
	if got := r.Related(dir); !slices.Equal(got, []string{"atlas"}) {
		t.Errorf("related = %q, want [atlas]", got)
	}
	if got := r.Ignored(dir); len(got) != 0 {
		t.Errorf("Ignored = %q outside git", got)
	}
}

// The git question is asked once per resolver, and only when a file sets
// project or related: a repository without one pays nothing.
func TestTrackedCheckIsCachedAndSkippedWhenNotNeeded(t *testing.T) {
	home, r := fixture(t)
	repo := filepath.Join(home, "work", "billing")
	calls := 0
	r.trackedProbe = func(dir string, rel []string) map[string]bool {
		calls++
		return gitTracked(dir, rel)
	}
	r.Project(repo)
	r.Related(repo)
	if calls != 0 {
		t.Errorf("no .backsight.toml: %d git checks", calls)
	}
	write(t, filepath.Join(repo, ".backsight.toml"), "retain = false\n")
	r.OptedOut(repo, nil)
	if calls != 0 {
		t.Errorf("a file with only retain needs no git check: %d", calls)
	}
	write(t, filepath.Join(repo, ".backsight.toml"), "project = \"x\"\n")
	r.Project(repo)
	r.Related(repo)
	r.Explicit(repo)
	r.OptedOut(repo, nil)
	if calls != 1 {
		t.Errorf("%d git checks for one file, want 1", calls)
	}
}

// A repository without a commit yet: HEAD is unborn, the index decides.
func TestTrackedCheckWithoutAnyCommit(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "fresh")
	_ = os.MkdirAll(repo, 0o700)
	git(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, ".backsight.toml"), "project = \"fresh-api\"\n")
	if got := New(home, []string{".claude"}).Project(repo); got != "fresh-api" {
		t.Errorf("untracked in an unborn repository: %q, want fresh-api", got)
	}
	git(t, repo, "add", ".backsight.toml")
	if got := New(home, []string{".claude"}).Project(repo); got != "fresh" {
		t.Errorf("staged in an unborn repository: %q, want fresh", got)
	}
}

// A cwd reached through a symlink still stops at the repository top: the
// git question never names a path outside the repository.
func TestSymlinkedCwdIntoARepositoryKeepsItsUntrackedFile(t *testing.T) {
	home, r := fixture(t)
	repo := filepath.Join(home, "work", "billing")
	write(t, filepath.Join(repo, ".backsight.toml"), "project = \"billing-api\"\n")
	write(t, filepath.Join(home, ".backsight.toml"), "related = [\"atlas\"]\n") // above the repository: never read
	link := filepath.Join(home, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skip(err)
	}
	cwd := filepath.Join(link, "internal")
	if got := r.Project(cwd); got != "billing-api" {
		t.Errorf("project = %q, want billing-api", got)
	}
	if got := r.Related(cwd); len(got) != 0 {
		t.Errorf("a file above the repository was read: %q", got)
	}
}
