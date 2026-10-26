// Package project names the project of a session from its working
// directory and tells whether the user opted it out of retain (SPEC §4,
// §13, decisions 0045 and 0050).
package project

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// MaxRelated caps the projects a .backsight.toml relates (decision 0071
// C3): the first ones are kept.
const MaxRelated = 10

// Settings is a repository's .backsight.toml.
type Settings struct {
	Project string   `toml:"project"`
	Retain  *bool    `toml:"retain"`
	Related []string `toml:"related"` // projects recall searches with this one
}

// Resolver caches git answers per directory.
type Resolver struct {
	home       string
	transverse []string
	roots      map[string]gitRoots
}

type gitRoots struct{ top, main, common string } // empty when not in a repository

func New(home string, transverse []string) *Resolver {
	return &Resolver{home: filepath.Clean(home), transverse: transverse, roots: map[string]gitRoots{}}
}

func (r *Resolver) git(dir string) gitRoots {
	if g, ok := r.roots[dir]; ok {
		return g
	}
	var g gitRoots
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir").Output()
	if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); err == nil && len(lines) == 2 {
		g.top = lines[0]
		common := lines[1]
		g.common = common
		if filepath.Base(common) == ".git" {
			g.main = filepath.Dir(common)
		} else {
			g.main = g.top
		}
	}
	r.roots[dir] = g
	return g
}

// view is what the .backsight.toml files above a directory say.
type view struct {
	project string   // the nearest non-empty project
	related []string // the nearest file's related, nil when none sets it
	optOut  string   // the first file that sets retain = false
	broken  string   // a file that cannot be read, with the reason
}

// settings reads every .backsight.toml from cwd up to the repository top,
// or up to the home directory (else the filesystem root) when git gives no
// top: a parent's retain = false is never hidden by a nearer file.
func (r *Resolver) settings(cwd string) view {
	var v view
	stop := r.git(cwd).top
	if stop == "" && within(cwd, r.home) {
		stop = r.home
	}
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, ".backsight.toml")
		raw, err := os.ReadFile(p)
		switch {
		case err == nil:
			var s Settings
			if err := toml.Unmarshal(raw, &s); err != nil {
				if v.broken == "" {
					v.broken = p + " cannot be read (" + err.Error() + ")"
				}
			} else {
				if v.project == "" && s.Project != "" {
					v.project = s.Project
				}
				if v.related == nil && s.Related != nil {
					v.related = s.Related
				}
				if v.optOut == "" && s.Retain != nil && !*s.Retain {
					v.optOut = p + " sets retain = false"
				}
			}
		case !errors.Is(err, fs.ErrNotExist):
			if v.broken == "" {
				v.broken = p + " cannot be read (" + err.Error() + ")"
			}
		}
		if dir == stop || dir == filepath.Dir(dir) {
			return v
		}
	}
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// Project names the session run in cwd.
func (r *Resolver) Project(cwd string) string {
	name, _ := r.Named(cwd)
	return name
}

// Named is Project, and whether the name is strong: a transverse path, a
// .backsight.toml project or a git repository's main worktree. A weak name
// is only cwd's basename, which a session's later directories do not get to
// use (decision 0071, C2a).
func (r *Resolver) Named(cwd string) (name string, strong bool) {
	cwd = filepath.Clean(cwd)
	claude := filepath.Join(r.home, ".claude")
	if cwd == r.home || cwd == claude || strings.HasPrefix(cwd, claude+string(filepath.Separator)) {
		return r.transverse[0], true
	}
	if v := r.settings(cwd); v.project != "" {
		return v.project, true
	}
	if g := r.git(cwd); g.main != "" {
		return filepath.Base(g.main), true
	}
	return filepath.Base(cwd), false
}

// Related lists the projects the nearest .backsight.toml that sets related
// names, without blanks or duplicates, the first MaxRelated only.
func (r *Resolver) Related(cwd string) []string {
	if strings.TrimSpace(cwd) == "" {
		return nil
	}
	var out []string
	for _, p := range r.settings(filepath.Clean(cwd)).related {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(out, p) && len(out) < MaxRelated {
			out = append(out, p)
		}
	}
	return out
}

// Root is the directory a project-wide setting applies to: the main
// worktree of cwd's repository (every worktree under it), else cwd.
func (r *Resolver) Root(cwd string) string {
	if g := r.git(cwd); g.main != "" {
		return g.main
	}
	return filepath.Clean(cwd)
}

// OptedOut reports whether sessions in cwd must not be retained, and why.
// An unreadable .backsight.toml, or no directory at all, fails closed.
func (r *Resolver) OptedOut(cwd string, exclude []string) (bool, string) {
	if strings.TrimSpace(cwd) == "" {
		return true, "no working directory recorded"
	}
	cwd = filepath.Clean(cwd)
	rcwd := resolved(cwd)
	// A linked worktree lives outside its main checkout: excluding the
	// project (its main checkout, what pause --project writes) covers it.
	main := ""
	if len(exclude) > 0 {
		if m := r.Root(cwd); m != cwd {
			main = m
		}
	}
	for _, e := range exclude {
		switch {
		case e == "~":
			e = r.home
		case strings.HasPrefix(e, "~/"):
			e = filepath.Join(r.home, e[2:])
		}
		e = filepath.Clean(e)
		for _, ex := range []string{e, resolved(e)} {
			for _, c := range []string{cwd, rcwd} {
				if under(c, ex) {
					return true, "privacy.exclude_paths contains " + e
				}
			}
			if main != "" && under(main, ex) {
				return true, "privacy.exclude_paths contains " + e + " (this worktree's main checkout)"
			}
		}
	}
	v := r.settings(cwd)
	switch {
	case v.broken != "":
		return true, v.broken + ": not retained until it is fixed"
	case v.optOut != "":
		return true, v.optOut
	}
	return false, ""
}

// resolved follows symlinks of the longest existing prefix of p.
func resolved(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolved(parent), filepath.Base(p))
}

// under compares paths the way the filesystem does: case-insensitively on
// macOS and Windows.
func under(p, dir string) bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		p, dir = strings.ToLower(p), strings.ToLower(dir)
	}
	return within(p, dir)
}

// Remote is the normalised URL of cwd's repository remote (origin, else
// the first one its config lists), "" outside git or without a remote.
// Session state records it per project so doctor can tell two
// repositories that share a project name (decision 0045). It reads the
// repository's config file: no extra git process in the hook.
func (r *Resolver) Remote(cwd string) string {
	g := r.git(filepath.Clean(cwd))
	if g.common == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(g.common, "config"))
	if err != nil {
		return ""
	}
	first, origin := "", ""
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.Join(strings.Fields(strings.Trim(line, "[]")), " "))
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "url") || !strings.HasPrefix(section, "remote ") {
			continue
		}
		u := strings.Trim(strings.TrimSpace(val), `"`)
		if first == "" {
			first = u
		}
		if section == `remote "origin"` && origin == "" {
			origin = u
		}
	}
	if origin != "" {
		return NormaliseRemote(origin)
	}
	return NormaliseRemote(first)
}

// NormaliseRemote reduces a git remote URL to host/path, lower case,
// without scheme, credentials, port, trailing slash or .git: the HTTPS and
// SSH forms of one repository compare equal, and no token is ever kept.
func NormaliseRemote(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	host, path := "", s
	colon := strings.Index(s, ":")
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		host, path = u.Hostname(), u.Path
	case colon > 0 && !strings.Contains(s[:colon], "/"):
		// scp-like: [user@]host:path
		h, p, _ := strings.Cut(s, ":")
		if i := strings.LastIndex(h, "@"); i >= 0 {
			h = h[i+1:]
		}
		host, path = h, p
	}
	path = strings.TrimSuffix(strings.TrimRight(path, "/"), ".git")
	path = strings.TrimRight(path, "/")
	out := strings.ToLower(path)
	if host != "" {
		out = strings.ToLower(host + "/" + strings.TrimLeft(path, "/"))
	}
	// An @ left over is a malformed URL's credentials (user:12/34@host,
	// user:tok@host/o/r): nothing is recorded rather than them.
	if strings.Contains(out, "@") {
		return ""
	}
	return out
}
