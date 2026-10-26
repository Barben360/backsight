package attribution

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/project"
)

// Repos names the project a path is evidence for. Only the user's own
// repositories count (decision 0072): a third-party clone (a package
// build, a framework checkout, a plugin manager's clone) is never a
// project. A repository is own when
//   - it is the session's launch repository (same main checkout, or a
//     clone of the same remote), or
//   - its remote's owner (host and every path segment but the last, such
//     as github.com/acme) is an own owner: the launch repository's, one
//     of projects.own_owners, or one of the owners this machine learnt
//     from the launch repositories of its retained sessions (Owners).
//
// A clone whose main checkout lies in a throwaway place (the temporary
// directory, ~/.cache) is one repository with its kept checkout: it counts
// for the project of the checkout with the same remote that the session
// launched in or the same turn names, under that checkout's name, and
// only if that checkout is not kept out of memory; with no such checkout
// it counts for nothing (a review clone named "bs" is not a project).
// Worktrees are named by their main checkout as usual, wherever they live.
// A directory kept out of memory (privacy.exclude_paths, retain = false)
// is never evidence, nor is a clone of one.
type Repos struct {
	res          *project.Resolver
	home         string
	transverse   []string
	exclude      []string
	launch       string
	launchMain   string
	launchRemote string
	launchOut    bool // the launch directory is kept out of memory
	owners       map[string]bool
	throwaway    []string
	// gone: directories the session's own commands created (git worktree
	// add, git clone) and that are removed since.
	gone  map[string]resolved
	cache map[string]resolved
	// Deadline, when set, stops resolving paths (no git process) once
	// passed: the caller then falls back.
	Deadline time.Time
}

// resolved is what a path names: an own repository (project, main), or a
// throwaway clone to name through its kept checkout (clone, remote).
type resolved struct {
	project, main, remote string
	clone                 bool
}

// NewRepos builds the namer of one session launched in launchDir.
// owners are the own remote owners besides the launch repository's
// (projects.own_owners, and those Owners learnt), exclude
// privacy.exclude_paths: a directory kept out of memory is never evidence,
// so its project's name is never sent.
func NewRepos(res *project.Resolver, home string, transverse []string, launchDir string, owners, exclude []string) *Repos {
	r := &Repos{res: res, home: filepath.Clean(home), transverse: transverse, exclude: exclude, owners: map[string]bool{},
		gone: map[string]resolved{}, cache: map[string]resolved{}}
	r.throwaway = []string{filepath.Join(r.home, ".cache"), "/tmp", filepath.Clean(os.TempDir())}
	for _, o := range owners {
		if o = OwnerOf(o); o != "" {
			r.owners[o] = true
		}
	}
	if strings.TrimSpace(launchDir) != "" {
		r.launch = res.Project(launchDir)
		r.launchMain = res.Main(launchDir)
		r.launchRemote = res.Remote(launchDir)
		r.launchOut, _ = res.OptedOut(launchDir, exclude)
		if o := Owner(r.launchRemote); o != "" && !r.launchOut {
			r.owners[o] = true
		}
	}
	return r
}

// OwnerOf normalises an owner as written in projects.own_owners
// ("github.com/Acme", "https://github.com/acme/") to the Owner form.
func OwnerOf(o string) string { return Owner(project.NormaliseRemote(o) + "/x") }

// LaunchOwner is the remote owner a retained launch directory teaches: ""
// for one kept out of memory, outside git, in a throwaway place, without a
// remote owner (a host alone is no owner), or whose repository the user's
// own git identity never committed to (a checkout of someone else's code).
func LaunchOwner(res *project.Resolver, home, launchDir string, exclude []string) string {
	if strings.TrimSpace(launchDir) == "" {
		return ""
	}
	if out, _ := res.OptedOut(launchDir, exclude); out {
		return ""
	}
	main := res.Main(launchDir)
	if main == "" || (&Repos{home: filepath.Clean(home), throwaway: []string{filepath.Join(filepath.Clean(home), ".cache"), "/tmp", filepath.Clean(os.TempDir())}}).under(main) {
		return ""
	}
	o := Owner(res.Remote(launchDir))
	if !strings.Contains(o, "/") || !res.Authored(launchDir) {
		return ""
	}
	return o
}

// Deny removes owners from the own ones (projects.deny_owners): their
// repositories are not the user's, whatever was learnt or configured.
// The launch repository itself stays its session's.
func (r *Repos) Deny(owners []string) {
	for _, o := range owners {
		delete(r.owners, OwnerOf(o))
	}
}

// Launch is the session's launch project, "" when its directory is kept
// out of memory (its name is then never a mention to move to).
func (r *Repos) Launch() string {
	if r.launchOut {
		return ""
	}
	return r.launch
}

// Transverse says whether name is a transverse alias.
func (r *Repos) Transverse(name string) bool { return slices.Contains(r.transverse, name) }

// Owner is a normalised remote's owner: everything before its last path
// segment ("github.com/acme" of "github.com/acme/tool"), "" without one.
func Owner(remote string) string {
	i := strings.LastIndex(remote, "/")
	if i <= 0 {
		return ""
	}
	return remote[:i]
}

// Project is the own project path is evidence for, "" when none: a
// transverse path, a plain directory, a third-party repository, a
// directory kept out of memory, a clone with no kept checkout known. kept
// are the checkouts known by remote (Kept); nil knows only the launch one.
func (r *Repos) Project(path string, kept map[string]Checkout) string {
	p, _ := r.ProjectRoot(path, kept)
	return p
}

// ProjectRoot is Project and the main checkout that names it.
func (r *Repos) ProjectRoot(path string, kept map[string]Checkout) (string, string) {
	v := r.resolve(path)
	if !v.clone {
		return v.project, v.main
	}
	c, ok := kept[v.remote]
	if !ok && v.remote == r.launchRemote && r.launchMain != "" && !r.launchOut {
		c, ok = Checkout{Project: r.launch, Main: r.launchMain}, true
	}
	if !ok {
		return "", ""
	}
	return c.Project, c.Main
}

// Checkout is a kept (not throwaway) own repository: what a clone of the
// same remote counts for.
type Checkout struct {
	Project, Main string
}

// Kept lists, by remote, the own repositories paths name directly: one
// repository, one project name, whatever directory a clone of it is in.
func (r *Repos) Kept(paths []string) map[string]Checkout {
	out := map[string]Checkout{}
	for _, p := range paths {
		if v := r.resolve(p); !v.clone && v.project != "" && v.remote != "" {
			if _, ok := out[v.remote]; !ok {
				out[v.remote] = Checkout{Project: v.project, Main: v.main}
			}
		}
	}
	return out
}

func (r *Repos) resolve(path string) resolved {
	p := filepath.Clean(path)
	if v, ok := r.cache[p]; ok {
		return v
	}
	if !r.Deadline.IsZero() && time.Now().After(r.Deadline) {
		return resolved{}
	}
	v := r.lookup(p)
	r.cache[p] = v
	return v
}

func (r *Repos) lookup(p string) resolved {
	claude := filepath.Join(r.home, ".claude")
	if p == r.home || p == claude || strings.HasPrefix(p, claude+string(filepath.Separator)) {
		return resolved{} // the transverse pool: no repository evidence
	}
	d := p
	for d != filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			break
		}
		d = filepath.Dir(d)
	}
	name, strong := "", false
	if d != r.home {
		name, strong = r.res.Named(d)
	}
	if !strong {
		if _, err := os.Lstat(p); err != nil {
			return r.goneDir(p) // removed since the turn ran: its own commands made it
		}
		return resolved{}
	}
	if r.Transverse(name) {
		return resolved{}
	}
	if out, _ := r.res.OptedOut(d, r.exclude); out {
		return resolved{}
	}
	main, remote := r.res.Main(d), r.res.Remote(d)
	if main != "" && r.under(main) {
		if remote == "" {
			return resolved{}
		}
		return resolved{clone: true, remote: remote}
	}
	if r.own(main, remote) {
		return resolved{project: name, main: main, remote: remote}
	}
	return resolved{}
}

// own: the launch repository (its main checkout, or its remote), or an
// own owner's.
func (r *Repos) own(main, remote string) bool {
	switch {
	case main != "" && main == r.launchMain:
		return true
	case remote != "" && remote == r.launchRemote:
		return true
	}
	return remote != "" && r.owners[Owner(remote)]
}

// under: dir lies in a throwaway place. Under the home directory only
// ~/.cache is one (a home inside the temporary directory, as in tests,
// keeps its repositories).
func (r *Repos) under(dir string) bool {
	roots := r.throwaway
	if dir == r.home || strings.HasPrefix(dir, r.home+string(filepath.Separator)) {
		roots = roots[:1]
	}
	for _, t := range roots {
		if t != "" && (dir == t || strings.HasPrefix(dir, t+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

func (r *Repos) goneDir(p string) resolved {
	for d := p; d != filepath.Dir(d); d = filepath.Dir(d) {
		if v, ok := r.gone[d]; ok {
			return v
		}
	}
	return resolved{}
}

var (
	wtAddRe = regexp.MustCompile(`(?:git\s+-C\s+(\S+)\s+)?worktree\s+add\s+((?:-[bB]\s+\S+\s+|--\S+\s+|-f\s+)*)(\S+)`)
	cloneRe = regexp.MustCompile(`git\s+clone\s+(?:--?\S+\s+)*(\S+)\s+(\S+)`)
)

// Learn reads one shell command of the session, run in cwd: a worktree it
// adds or a clone it makes is remembered with its project, so a later
// path into it still counts once it is removed. Worktrees and clones are
// usually removed when their work ends, before the session is flushed.
func (r *Repos) Learn(cmd, cwd string) {
	if !strings.Contains(cmd, "worktree") && !strings.Contains(cmd, "clone") {
		return
	}
	for _, m := range wtAddRe.FindAllStringSubmatchIndex(cmd, -1) {
		repo := cwd
		if m[2] >= 0 {
			repo = expand(cmd[m[2]:m[3]], r.home)
		} else {
			// the last absolute cd before the command
			for _, c := range cdRe.FindAllStringSubmatch(cmd[:m[0]], -1) {
				if absLike(c[2]) {
					if e := expand(c[2], r.home); e != "" {
						repo = e
					}
				}
			}
		}
		dst := cmd[m[6]:m[7]]
		if absLike(dst) {
			dst = expand(dst, r.home)
		} else {
			base := repo
			if base == "" {
				base = cwd
			}
			dst = filepath.Join(base, dst)
		}
		if dst == "" || repo == "" {
			continue
		}
		if _, err := os.Stat(repo); err != nil {
			continue
		}
		// a worktree of a repository is that repository: same answer,
		// exclusions and clones included
		if v := r.resolve(repo); v.project != "" || v.clone {
			r.gone[filepath.Clean(dst)] = v
		}
	}
	for _, m := range cloneRe.FindAllStringSubmatch(cmd, -1) {
		if !absLike(m[2]) {
			continue
		}
		dst := expand(m[2], r.home)
		remote := project.NormaliseRemote(m[1])
		if dst == "" || remote == "" {
			continue
		}
		// named, once removed, through its kept checkout like a live clone
		r.gone[dst] = resolved{clone: true, remote: remote}
	}
}
