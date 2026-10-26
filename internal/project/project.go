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

// Settings is a repository's .backsight.toml. A file git tracks (in the
// index or in HEAD) came with the repository, so whoever published it chose
// its keys: only what protects, retain = false, is honoured from it; its
// project and related are ignored (decision 0072 M5). An untracked file is
// the user's own and is honoured fully, and so is any file outside git.
type Settings struct {
	Project string   `toml:"project"`
	Retain  *bool    `toml:"retain"`
	Related []string `toml:"related"` // projects recall searches with this one
}

// Resolver caches git answers per directory, and per file whether git
// tracks it: one resolver lives for one hook or command, so a file that
// becomes tracked stops being trusted at the next one.
type Resolver struct {
	home       string
	transverse []string
	roots      map[string]gitRoots
	tracked    map[string]bool
	// trackedProbe asks git which of rel (paths relative to dir) it
	// tracks; nil means it could not tell. Tests count its calls.
	trackedProbe func(dir string, rel []string) map[string]bool
}

type gitRoots struct{ top, main, common string } // empty when not in a repository

func New(home string, transverse []string) *Resolver {
	return &Resolver{home: filepath.Clean(home), transverse: transverse, roots: map[string]gitRoots{}, tracked: map[string]bool{}, trackedProbe: gitTracked}
}

// Home is the home directory the resolver names projects under.
func (r *Resolver) Home() string { return r.home }

func (r *Resolver) git(dir string) gitRoots {
	if g, ok := r.roots[dir]; ok {
		return g
	}
	// git finds a repository through a .git entry in dir or above it: a
	// directory with none is answered without a process, and every
	// directory of one repository shares its top's answer (a turn's tool
	// calls name many directories, decision 0072).
	top := ""
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() { // git -C fails on anything else
		top = gitEntry(dir)
	}
	if top == "" {
		r.roots[dir] = gitRoots{}
		return gitRoots{}
	}
	if top != dir {
		g := r.git(top)
		r.roots[dir] = g
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

// gitEntry is the nearest directory from dir up that holds a .git entry
// (a repository, a linked worktree or a submodule), "" when none does.
func gitEntry(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if d == filepath.Dir(d) {
			return ""
		}
	}
}

// Main is the main worktree of dir's repository (a linked worktree's main
// checkout), "" outside git.
func (r *Resolver) Main(dir string) string { return r.git(filepath.Clean(dir)).main }

// view is what the .backsight.toml files above a directory say.
type view struct {
	project string   // the nearest non-empty project of a trusted file
	related []string // the nearest trusted file's related, nil when none sets it
	optOut  string   // the first file that sets retain = false
	broken  string   // a file that cannot be read, with the reason
	ignored []string // tracked files whose project or related were ignored, nearest first
}

// settings reads every .backsight.toml from cwd up to the repository top,
// or up to the home directory (else the filesystem root) when git gives no
// top: a parent's retain = false is never hidden by a nearer file. It is
// the one place that decides what a file is trusted for.
func (r *Resolver) settings(cwd string) view {
	var v view
	cwd = filepath.Clean(cwd)
	stop := r.git(cwd).top
	inGit := stop != ""
	if stop == "" && within(cwd, r.home) {
		stop = r.home
	}
	type file struct {
		path, rel string
		s         Settings
	}
	var claims []file // files that set project or related, nearest first
	rel := ".backsight.toml"
	for dir := cwd; ; dir, rel = filepath.Dir(dir), "../"+rel {
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
				if v.optOut == "" && s.Retain != nil && !*s.Retain {
					v.optOut = p + " sets retain = false"
				}
				if s.Project != "" || s.Related != nil {
					claims = append(claims, file{p, "./" + rel, s})
				}
			}
		case !errors.Is(err, fs.ErrNotExist):
			if v.broken == "" {
				v.broken = p + " cannot be read (" + err.Error() + ")"
			}
		}
		// A cwd reached through a symlink meets the (resolved) top only
		// once resolved: the walk never leaves the repository.
		if dir == stop || dir == filepath.Dir(dir) || (inGit && resolved(dir) == stop) {
			break
		}
	}
	if inGit && len(claims) > 0 {
		var ask []string
		for _, f := range claims {
			if _, ok := r.tracked[f.path]; !ok {
				ask = append(ask, f.rel)
			}
		}
		if len(ask) > 0 {
			got := r.trackedProbe(cwd, ask)
			for _, f := range claims {
				if _, ok := r.tracked[f.path]; !ok {
					// git could not tell: not trusted (fails closed)
					t, known := got[f.rel]
					r.tracked[f.path] = t || !known
				}
			}
		}
	}
	for _, f := range claims {
		if inGit && r.tracked[f.path] {
			v.ignored = append(v.ignored, f.path)
			continue
		}
		if v.project == "" && f.s.Project != "" {
			v.project = f.s.Project
		}
		if v.related == nil && f.s.Related != nil {
			v.related = f.s.Related
		}
	}
	return v
}

// gitTracked reports which of rel (./-relative to dir) git tracks: in the
// index or in HEAD, one git process for all of them (about 1.5 ms). A file
// whose removal is staged but not committed is still tracked. nil when git
// fails.
func gitTracked(dir string, rel []string) map[string]bool {
	var in strings.Builder
	for _, p := range rel { // ./, ../ and the file name only: no blank, no newline
		in.WriteString(":" + p + "\nHEAD:" + p + "\n")
	}
	cmd := exec.Command("git", "-C", dir, "cat-file", "--batch-check")
	cmd.Stdin = strings.NewReader(in.String())
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	got := map[string]bool{}
	i := 0
	for _, p := range rel {
		if i+1 >= len(lines) {
			return got // a short answer: the rest stays unknown
		}
		got[p] = present(lines[i]) || present(lines[i+1])
		i += 2
	}
	return got
}

// present reads one --batch-check answer: "<oid> <type> <size>" when the
// object exists, "<name> missing" (or ambiguous) when not.
func present(line string) bool {
	f := strings.Fields(line)
	return len(f) == 3 && f[1] == "blob"
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

// Ignored lists the .backsight.toml files above cwd whose project or
// related keys were ignored because git tracks them, nearest first.
func (r *Resolver) Ignored(cwd string) []string {
	if strings.TrimSpace(cwd) == "" {
		return nil
	}
	return r.settings(cwd).ignored
}

// Related lists the projects the nearest trusted .backsight.toml that
// sets related names, without blanks or duplicates, the first MaxRelated
// only.
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
	return NormaliseRemote(r.rawRemote(cwd))
}

// rawRemote is the URL of cwd's repository remote as its config writes it.
func (r *Resolver) rawRemote(cwd string) string {
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
		return origin
	}
	return first
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
