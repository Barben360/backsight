package attribution

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/project"
)

// Repos names the project a path is evidence for. Only the user's own
// repositories count (decision 0072): a third-party clone (a package
// build, a framework checkout, a plugin manager's clone) is never a
// project. A repository is own when
//   - it is the session's launch repository (same project name), or
//   - its remote's owner (host and every path segment but the last, such
//     as github.com/acme) is the launch repository's, or
//   - its remote's owner is listed in projects.own_owners.
//
// A clone whose main checkout lies in a throwaway place (the temporary
// directory, ~/.cache) counts for the launch project when it is a clone of
// it, for the repository its remote names when that remote is own, else
// for nothing: a review clone named "bs" is not a project. Worktrees are
// named by their main checkout as usual, wherever they live.
//
// Every input is the session's own (its launch directory, its commands)
// or the configuration: never what other sessions did, so a later run
// answers the same.
type Repos struct {
	res          *project.Resolver
	home         string
	transverse   []string
	exclude      []string
	launch       string
	launchRemote string
	owners       map[string]bool
	throwaway    []string
	// gone: directories the session's own commands created (git worktree
	// add, git clone) and that are removed since, by the project they
	// were evidence for when the turn ran.
	gone  map[string]string
	cache map[string]string
}

// NewRepos builds the namer of one session launched in launchDir.
// owners are projects.own_owners, exclude privacy.exclude_paths: a
// directory kept out of memory is never evidence, so its project's name
// is never sent.
func NewRepos(res *project.Resolver, home string, transverse []string, launchDir string, owners, exclude []string) *Repos {
	r := &Repos{res: res, home: filepath.Clean(home), transverse: transverse, exclude: exclude, owners: map[string]bool{},
		gone: map[string]string{}, cache: map[string]string{}}
	r.throwaway = []string{filepath.Join(r.home, ".cache"), "/tmp", filepath.Clean(os.TempDir())}
	for _, o := range owners {
		if o = Owner(project.NormaliseRemote(o) + "/x"); o != "" {
			r.owners[o] = true
		}
	}
	if strings.TrimSpace(launchDir) != "" {
		r.launch = res.Project(launchDir)
		r.launchRemote = res.Remote(launchDir)
		if o := Owner(r.launchRemote); o != "" {
			r.owners[o] = true
		}
	}
	return r
}

// Launch is the session's launch project.
func (r *Repos) Launch() string { return r.launch }

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
// transverse path, a plain directory, a third-party repository.
func (r *Repos) Project(path string) string {
	p := filepath.Clean(path)
	if v, ok := r.cache[p]; ok {
		return v
	}
	v := r.project(p)
	r.cache[p] = v
	return v
}

func (r *Repos) project(p string) string {
	claude := filepath.Join(r.home, ".claude")
	if p == r.home || p == claude || strings.HasPrefix(p, claude+string(filepath.Separator)) {
		return "" // the transverse pool: no repository evidence
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
			return r.goneProject(p) // removed since the turn ran: its own commands made it
		}
		return ""
	}
	if r.Transverse(name) {
		return ""
	}
	if out, _ := r.res.OptedOut(d, r.exclude); out {
		return ""
	}
	main := r.res.Main(d)
	if main != "" && r.under(main) {
		return r.cloneProject(main)
	}
	if name == r.launch || r.owners[Owner(r.res.Remote(d))] {
		return name
	}
	return ""
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

// cloneProject names a throwaway clone by its remote: the launch project
// for a clone of it, the remote's repository for another own remote.
func (r *Repos) cloneProject(main string) string {
	remote := r.res.Remote(main)
	switch {
	case remote == "":
		return ""
	case remote == r.launchRemote:
		return r.launch
	case r.owners[Owner(remote)]:
		return r.res.RepoName(main)
	}
	return ""
}

func (r *Repos) goneProject(p string) string {
	for d := p; d != filepath.Dir(d); d = filepath.Dir(d) {
		if v, ok := r.gone[d]; ok {
			return v
		}
	}
	return ""
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
		if p := r.Project(repo); p != "" {
			r.gone[filepath.Clean(dst)] = p
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
		switch {
		case remote == r.launchRemote:
			r.gone[dst] = r.launch
		case r.owners[Owner(remote)]:
			name := strings.TrimSuffix(strings.TrimRight(m[1], "/"), ".git")
			if i := strings.LastIndexAny(name, "/:"); i >= 0 {
				name = name[i+1:]
			}
			r.gone[dst] = name
		}
	}
}
