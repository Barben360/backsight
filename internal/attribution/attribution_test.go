package attribution

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/testtool"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	testtool.RequireGit(t)
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repo makes a git repository at dir with origin remote (none when "").
func repo(t *testing.T, dir, remote string) string {
	t.Helper()
	_ = os.MkdirAll(filepath.Join(dir, "src"), 0o700)
	git(t, dir, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main\n"), 0o600)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "init")
	if remote != "" {
		git(t, dir, "remote", "add", "origin", remote)
	}
	return dir
}

// world: a home with the user's repositories alpha and beta (owner me), a
// third-party clone vendor (owner someone), a work repository (owner
// acme) and a plain directory.
type world struct {
	home                      string
	alpha, beta, vendor, acme string
	plain                     string
	res                       *project.Resolver
	transverse                []string
}

func newWorld(t *testing.T) world {
	home := t.TempDir()
	w := world{home: home, transverse: []string{".claude", filepath.Base(home)}}
	w.alpha = repo(t, filepath.Join(home, "src", "alpha"), "git@github.com:me/alpha.git")
	w.beta = repo(t, filepath.Join(home, "src", "beta"), "https://github.com/me/beta")
	w.vendor = repo(t, filepath.Join(home, "build", "vendor-tool"), "https://github.com/someone/vendor-tool.git")
	w.acme = repo(t, filepath.Join(home, "work", "billing"), "git@github.com:acme/billing.git")
	w.plain = filepath.Join(home, "notes")
	_ = os.MkdirAll(w.plain, 0o700)
	_ = os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o700)
	w.res = project.New(home, w.transverse)
	return w
}

func (w world) repos(launch string, owners ...string) *Repos {
	return NewRepos(w.res, w.home, w.transverse, launch, owners, nil)
}

func input(kv ...string) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	for i := 0; i+1 < len(kv); i += 2 {
		raw, _ := json.Marshal(kv[i+1])
		m[kv[i]] = raw
	}
	return m
}

func TestCallOfFindsThePathsATurnNames(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX paths in the tool calls (Windows: TestAttributionSeesWindowsPaths)")
	home := "/home/u"
	cases := []struct {
		tool  string
		in    map[string]json.RawMessage
		cwd   string
		paths []string
		write bool
	}{
		{"Read", input("file_path", "/home/u/src/a/x.go"), "/home/u", []string{"/home/u/src/a/x.go"}, false},
		{"Read", input("file_path", "x.go"), "/home/u/src/a", []string{"/home/u/src/a/x.go"}, false}, // a path key is relative to cwd
		{"Edit", input("file_path", "~/src/a/x.go", "old_string", "a", "new_string", "b"), "", []string{"/home/u/src/a/x.go"}, true},
		{"Grep", input("pattern", "foo", "path", "$HOME/src/b"), "", []string{"/home/u/src/b"}, false},
		{"Bash", input("command", "cd /home/u/src/a && go test ./..."), "/home/u", []string{"/home/u/src/a", "/home/u/src/a"}, false},
		{"Bash", input("command", "git -C ~/src/b status"), "/home/u", []string{"/home/u/src/b", "/home/u/src/b"}, false},
		{"Bash", input("command", "cd src/a && ls"), "/home/u", nil, false},            // a relative cd is no evidence
		{"Bash", input("command", "ls /usr/lib/go /etc/hosts"), "/home/u", nil, false}, // system paths are never projects
		{"Bash", input("command", "git commit -m x"), "/home/u/src/a", nil, true},
		{"Bash", input("command", "echo hi > /home/u/src/a/out.txt"), "", []string{"/home/u/src/a/out.txt"}, true},
		{"Bash", input("command", "gh pr create --fill"), "", nil, true},
		{"Bash", input("command", "sed -i s/a/b/ f"), "", nil, true},
		{"Bash", input("command", "cat a/home/u/x"), "", nil, false}, // glued to a word: not a path
		{"Glob", input("pattern", "/home/u/src/a/**/*.go"), "", []string{"/home/u/src/a/"}, false},
	}
	for _, c := range cases {
		got := CallOf(c.tool, c.in, c.cwd, home)
		slices.Sort(got.Paths)
		want := slices.Clone(c.paths)
		for i := range want {
			want[i] = filepath.Clean(want[i])
		}
		slices.Sort(want)
		if !slices.Equal(got.Paths, want) || got.Write != c.write {
			t.Errorf("%s %s: paths %q write %v, want %q %v", c.tool, c.in, got.Paths, got.Write, want, c.write)
		}
	}
}

func TestOnlyOwnRepositoriesAreEvidence(t *testing.T) {
	w := newWorld(t)
	r := w.repos(w.alpha)
	cases := map[string]string{
		filepath.Join(w.alpha, "src", "main.go"):   "alpha", // the launch repository
		filepath.Join(w.beta, "src", "main.go"):    "beta",  // same remote owner as the launch repository
		filepath.Join(w.vendor, "src", "main.go"):  "",      // a third-party clone
		filepath.Join(w.acme, "src", "main.go"):    "",      // another owner, not configured
		filepath.Join(w.plain, "todo.md"):          "",      // a plain directory
		filepath.Join(w.home, ".claude", "skills"): "",      // the transverse pool
		filepath.Join(w.home, "missing", "x"):      "",      // nothing on disk, nothing learnt
	}
	for p, want := range cases {
		if got := r.Project(p, nil); got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
	if got := w.repos(w.alpha, "github.com/acme").Project(filepath.Join(w.acme, "src"), nil); got != "billing" {
		t.Errorf("projects.own_owners did not make acme's repository own: %q", got)
	}
	if got := w.repos(w.alpha, "https://GitHub.com/Acme/").Project(filepath.Join(w.acme, "src"), nil); got != "billing" {
		t.Errorf("an owner written as a URL is not normalised: %q", got)
	}
	// Launched in a plain directory: no owner is known, so only configured ones count.
	if got := w.repos(w.plain).Project(filepath.Join(w.beta, "src"), nil); got != "" {
		t.Errorf("a session launched outside any repository took %q as own", got)
	}
}

func TestAnExcludedRepositoryIsNeverEvidence(t *testing.T) {
	w := newWorld(t)
	r := NewRepos(w.res, w.home, w.transverse, w.alpha, nil, []string{w.beta})
	if got := r.Project(filepath.Join(w.beta, "src", "main.go"), nil); got != "" {
		t.Errorf("an excluded repository was evidence: %q", got)
	}
	_ = os.WriteFile(filepath.Join(w.alpha, ".backsight.toml"), []byte("retain = false\n"), 0o600)
	r = NewRepos(project.New(w.home, w.transverse), w.home, w.transverse, w.beta, nil, nil)
	if got := r.Project(filepath.Join(w.alpha, "src"), nil); got != "" {
		t.Errorf("a repository with retain = false was evidence: %q", got)
	}
}

// A throwaway clone is one repository with its kept checkout: it counts
// under the kept checkout's name (never the remote's), and for nothing
// when no kept checkout of that remote is known.
func TestThrowawayClonesCountForTheirKeptCheckout(t *testing.T) {
	w := newWorld(t)
	cache := filepath.Join(w.home, ".cache")
	kept := repo(t, filepath.Join(w.home, "src", "tool-main"), "git@github.com:me/Tool.git") // kept checkout named otherwise
	review := repo(t, filepath.Join(cache, "review-a1"), "git@github.com:me/alpha.git")
	bs := repo(t, filepath.Join(cache, "bs"), "https://github.com/me/tool")
	stranger := repo(t, filepath.Join(cache, "aur-pkg"), "https://aur.example.org/pkg/thing.git")
	r := w.repos(w.alpha)
	file := func(p string) string { return filepath.Join(p, "src", "main.go") }
	for p, want := range map[string]string{review: "alpha", bs: "", stranger: ""} {
		if got := r.Project(file(p), nil); got != want {
			t.Errorf("clone %s with only the launch checkout known: %q, want %q", filepath.Base(p), got, want)
		}
	}
	known := r.Kept([]string{file(kept), file(w.beta)})
	if got := r.Project(file(bs), known); got != "tool-main" {
		t.Errorf("a clone of a kept checkout: %q, want its name tool-main (one repository, one project)", got)
	}
	if got := r.Project(file(stranger), known); got != "" {
		t.Errorf("a third-party clone: %q", got)
	}
	// A linked worktree under ~/.cache is its main checkout's.
	wt := filepath.Join(cache, "wt-beta")
	git(t, w.beta, "worktree", "add", "-q", wt, "-b", "fix")
	if got := r.Project(filepath.Join(wt, "src"), nil); got != "beta" {
		t.Errorf("a worktree under ~/.cache: %q", got)
	}
	// Tally finds the kept checkout among the turn's own paths.
	v := Tally(calls(read(file(kept)), write(file(bs)), write(file(bs))), r)
	// git names a main checkout with long names: a 8.3 TEMP (C:\Users\RUNNER~1)
	// spells kept otherwise.
	if got := v.Projects(); len(got) != 1 || got[0] != "tool-main" || v.Roots["tool-main"] != fsutil.Normal(kept) {
		t.Errorf("tally %+v", v)
	}
}

// A clone of a repository kept out of memory never names nor receives it,
// live or removed since.
func TestAnExcludedRepositorysClonesAreNeverEvidence(t *testing.T) {
	w := newWorld(t)
	private := repo(t, filepath.Join(w.home, "src", "private-notes"), "git@github.com:me/private-notes.git")
	live := repo(t, filepath.Join(w.home, ".cache", "pn"), "git@github.com:me/private-notes.git")
	r := NewRepos(w.res, w.home, w.transverse, w.alpha, nil, []string{private})
	known := r.Kept([]string{filepath.Join(private, "src")})
	if got := r.Project(filepath.Join(live, "src", "main.go"), known); got != "" {
		t.Errorf("a live clone of an excluded repository: %q", got)
	}
	gone := filepath.Join(w.home, ".cache", "pn-gone")
	r.Learn("git clone git@github.com:me/private-notes.git "+gone, w.alpha)
	if got := r.Project(filepath.Join(gone, "notes.md"), known); got != "" {
		t.Errorf("a removed clone of an excluded repository: %q", got)
	}
	// launched in an excluded directory: its clones and its name are out too
	r = NewRepos(w.res, w.home, w.transverse, private, nil, []string{private})
	if got := r.Project(filepath.Join(live, "src", "main.go"), nil); got != "" || r.Launch() != "" {
		t.Errorf("clone %q, launch %q", got, r.Launch())
	}
}

// One repository is the launch one by its checkout or remote, not by its
// name: a third-party checkout named like the launch project is not own.
func TestALaunchNamesakeIsNotOwn(t *testing.T) {
	w := newWorld(t)
	namesake := repo(t, filepath.Join(w.home, "build", "alpha"), "https://github.com/someone/alpha.git")
	if got := w.repos(w.alpha).Project(filepath.Join(namesake, "src"), nil); got != "" {
		t.Errorf("a third-party checkout named alpha: %q", got)
	}
}

func TestRemovedWorktreesStillCountFromTheSessionsCommands(t *testing.T) {
	w := newWorld(t)
	gone := filepath.Join(w.home, ".cache", "dev", "wt-feature")
	r := w.repos(w.alpha)
	if got := r.Project(filepath.Join(gone, "src", "x.go"), nil); got != "" {
		t.Fatalf("before learning: %q", got)
	}
	r = w.repos(w.alpha)
	r.Learn("git -C "+w.beta+" fetch -q && git -C "+w.beta+" worktree add "+gone+" -b feat origin/main", w.alpha)
	r.Learn("cd "+w.alpha+" && git worktree add ../alpha-wt -b x", w.home)
	r.Learn("git clone git@github.com:me/alpha.git /tmp/review-alpha", w.home)
	r.Learn("git clone https://github.com/someone/vendor-tool /tmp/vendor-copy", w.home)
	for p, want := range map[string]string{
		filepath.Join(gone, "internal", "a.go"):                       "beta",
		filepath.Join(filepath.Dir(w.alpha), "alpha-wt", "README.md"): "alpha", // relative to the cd'd repository
		"/tmp/review-alpha/src/main.go":                               "alpha",
		"/tmp/vendor-copy/x":                                          "",
	} {
		if got := r.Project(p, nil); got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
}

// What Learn does not parse stays unknown: a relative git -C, a command
// built in a variable. The removed directory is then no evidence, never a
// guess.
func TestLearnMissesGiveNoEvidence(t *testing.T) {
	w := newWorld(t)
	r := w.repos(w.alpha)
	r.Learn("git -C ../beta worktree add /tmp/wt-rel -b x", w.alpha)
	r.Learn(`d=/tmp/wt-var; git -C `+w.beta+` worktree add "$d" -b y`, w.alpha)
	r.Learn("bash -c 'git clone git@github.com:me/beta.git /tmp/in-shell'", w.alpha)
	for _, p := range []string{"/tmp/wt-rel/a.go", "/tmp/wt-var/a.go"} {
		if got := r.Project(p, nil); got != "" {
			t.Errorf("%s: %q", p, got)
		}
	}
}

func calls(cs ...Call) []Call { return cs }

func read(p string) Call  { return Call{Paths: []string{p}} }
func write(p string) Call { return Call{Paths: []string{p}, Write: true} }
func bash(cwd string) Call {
	return Call{CWD: cwd, Bash: true}
}

func TestWritesWeighThreeReads(t *testing.T) {
	w := newWorld(t)
	// launched in ~ (transverse), the user's owner configured
	r2 := NewRepos(w.res, w.home, w.transverse, w.home, []string{"github.com/me"}, nil)
	a, b := filepath.Join(w.alpha, "src", "main.go"), filepath.Join(w.beta, "src", "main.go")
	// Two edits in alpha against three reads in beta: 6 to 3, alpha leads with 67 %.
	v := Tally(calls(write(a), write(a), read(b), read(b), read(b)), r2)
	if p, src := Decide(".claude", true, v, nil, Previous{}); p != "alpha" || src != SourceEvidence {
		t.Errorf("writes did not weigh three: %q (%s), votes %+v", p, src, v)
	}
	// Counted as reads, beta would lead 3 to 2 (60 %): the weight decides.
	v = Tally(calls(read(a), read(a), read(b), read(b), read(b)), r2)
	if p, _ := Decide(".claude", true, v, nil, Previous{}); p != "beta" {
		t.Errorf("reads only: %q, want beta", p)
	}
	// One call naming a repository twice is one vote.
	v = Tally(calls(Call{Paths: []string{a, filepath.Join(w.alpha, "go.mod")}}), r2)
	if got := v.Explicit; len(got) != 1 || got[0].N != 1 {
		t.Errorf("one call, two paths: %+v", got)
	}
}

func TestATransverseTurnMovesOnlyOnAClearMajority(t *testing.T) {
	w := newWorld(t)
	r := NewRepos(w.res, w.home, w.transverse, filepath.Join(w.home, ".claude"), []string{"github.com/me"}, nil)
	a, b := filepath.Join(w.alpha, "src"), filepath.Join(w.beta, "src")
	for _, c := range []struct {
		name  string
		calls []Call
		want  string
	}{
		{"three reads: under the 4 votes", calls(read(a), read(a), read(a)), ".claude"},
		{"four reads", calls(read(a), read(a), read(a), read(a)), "alpha"},
		{"split 3 to 3", calls(read(a), read(a), read(a), read(b), read(b), read(b)), ".claude"},
		{"third-party work stays transverse", calls(read(filepath.Join(w.vendor, "src")), write(filepath.Join(w.vendor, "src"))), ".claude"},
	} {
		v := Tally(c.calls, r)
		if p, _ := Decide(".claude", true, v, nil, Previous{}); p != c.want {
			t.Errorf("%s: %q, want %q", c.name, p, c.want)
		}
	}
}

func TestTheLaunchRepositoryIsOverriddenOnlyByClearEvidence(t *testing.T) {
	w := newWorld(t)
	r := w.repos(w.alpha)
	a, b := filepath.Join(w.alpha, "src"), filepath.Join(w.beta, "src")
	for _, c := range []struct {
		name     string
		calls    []Call
		mentions []string
		want     string
	}{
		{"one edit in beta (3 votes, 100 %)", calls(write(b)), nil, "beta"},
		{"two reads in beta: under 3 votes", calls(read(b), read(b)), nil, "alpha"},
		{"beta 3 against alpha's implicit 2 (60 %)", calls(write(b), bash(w.alpha), bash(w.alpha)), nil, "alpha"},
		{"beta 3 against alpha 1 (75 %)", calls(write(b), read(a)), nil, "beta"},
		{"one read in beta, the prompt names it, nothing for alpha", calls(read(b)), []string{"beta"}, "beta"},
		{"one read in beta, named, but alpha has a vote", calls(read(b), read(a)), []string{"beta"}, "alpha"},
		{"a third-party clone never overrides", calls(write(filepath.Join(w.vendor, "src"))), nil, "alpha"},
	} {
		v := Tally(c.calls, r)
		if p, _ := Decide("alpha", false, v, c.mentions, Previous{}); p != c.want {
			t.Errorf("%s: %q, want %q (votes %+v)", c.name, p, c.want, v)
		}
	}
}

func TestMentionsAndContinuityInTransverseSessions(t *testing.T) {
	if p, src := Decide(".claude", true, Votes{}, []string{"alpha"}, Previous{}); p != "alpha" || src != SourceMention {
		t.Errorf("a prompt naming one project, no path against it: %q %s", p, src)
	}
	v := Votes{Explicit: []Count{{"beta", 1}}}
	if p, _ := Decide(".claude", true, v, []string{"alpha"}, Previous{}); p != ".claude" {
		t.Errorf("a mention with a path against it moved the turn: %q", p)
	}
	if p, src := Decide(".claude", true, v, []string{"beta"}, Previous{}); p != "beta" || src != SourceMention {
		t.Errorf("one path and its mention: %q %s", p, src)
	}
	if p, src := Decide(".claude", true, v, []string{"alpha", "beta"}, Previous{}); p != "beta" || src != SourceBoth {
		t.Errorf("one path named among two mentions: %q %s", p, src)
	}
	if p, src := Decide(".claude", true, Votes{}, nil, Previous{Project: "alpha", Source: SourceEvidence}); p != "alpha" || src != SourceContinuity {
		t.Errorf("a turn with no path after an attributed one: %q %s", p, src)
	}
	if p, _ := Decide(".claude", true, Votes{}, nil, Previous{Project: "alpha", Source: SourceCWD}); p != ".claude" {
		t.Errorf("continuity from a launch directory: %q", p)
	}
	if p, _ := Decide("alpha", false, Votes{}, nil, Previous{Project: "beta", Source: SourceEvidence}); p != "alpha" {
		t.Errorf("continuity in a repository session: %q", p)
	}
	got := Mentions("Fix the alpha-cli and bet. Also gamma_x, deltas, Epsilon and .claude", []string{"alpha", "bet", "gamma", "delta", "epsilon", ".claude"})
	if !slices.Equal(got, []string{"epsilon"}) {
		t.Errorf("mentions: %q (short names, dot names and glued words are not mentions)", got)
	}
}

// CallOf reads the same input the same way whatever the map order: a
// tool call with many string inputs lists its paths in one order.
func TestCallOfIsIndependentOfInputOrder(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX paths in the tool calls (Windows: TestAttributionSeesWindowsPaths)")
	kv := []string{"command", "cd /home/u/a && ls", "file_path", "/home/u/b/x.go"}
	for i := range 20 {
		kv = append(kv, fmt.Sprintf("note%02d", i), fmt.Sprintf("see /home/u/r%02d/file", i))
	}
	first := CallOf("Bash", input(kv...), "/home/u", "/home/u")
	for range 50 {
		if got := CallOf("Bash", input(kv...), "/home/u", "/home/u"); !slices.Equal(got.Paths, first.Paths) {
			t.Fatalf("another order: %q, first %q", got.Paths, first.Paths)
		}
	}
	if len(first.Paths) != 23 {
		t.Errorf("paths %q", first.Paths)
	}
}
