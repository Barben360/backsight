// Package project names the project of a session from its working
// directory and tells whether the user opted it out of retain (SPEC §4,
// §13, decisions 0045 and 0050).
package project

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Settings is a repository's .backsight.toml.
type Settings struct {
	Project string `toml:"project"`
	Retain  *bool  `toml:"retain"`
}

// Resolver caches git answers per directory.
type Resolver struct {
	home       string
	transverse []string
	roots      map[string]gitRoots
}

type gitRoots struct{ top, main string } // empty when not in a repository

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
		if filepath.Base(common) == ".git" {
			g.main = filepath.Dir(common)
		} else {
			g.main = g.top
		}
	}
	r.roots[dir] = g
	return g
}

// settings reads the nearest .backsight.toml from cwd up to the repository
// top (or only cwd outside a repository).
func (r *Resolver) settings(cwd string) (Settings, string, error) {
	stop := r.git(cwd).top
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, ".backsight.toml")
		raw, err := os.ReadFile(p)
		if err == nil {
			var s Settings
			if err := toml.Unmarshal(raw, &s); err != nil {
				return s, p, err
			}
			return s, p, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return Settings{}, p, err
		}
		if stop == "" || dir == stop || dir == filepath.Dir(dir) {
			return Settings{}, "", nil
		}
	}
}

// Project names the session run in cwd.
func (r *Resolver) Project(cwd string) string {
	cwd = filepath.Clean(cwd)
	claude := filepath.Join(r.home, ".claude")
	if cwd == r.home || cwd == claude || strings.HasPrefix(cwd, claude+string(filepath.Separator)) {
		return r.transverse[0]
	}
	if s, _, err := r.settings(cwd); err == nil && s.Project != "" {
		return s.Project
	}
	if g := r.git(cwd); g.main != "" {
		return filepath.Base(g.main)
	}
	return filepath.Base(cwd)
}

// OptedOut reports whether sessions in cwd must not be retained, and why.
// An unreadable .backsight.toml fails closed.
func (r *Resolver) OptedOut(cwd string, exclude []string) (bool, string) {
	cwd = filepath.Clean(cwd)
	for _, e := range exclude {
		if strings.HasPrefix(e, "~/") {
			e = filepath.Join(r.home, e[2:])
		}
		e = filepath.Clean(e)
		if cwd == e || strings.HasPrefix(cwd, e+string(filepath.Separator)) {
			return true, "privacy.exclude_paths contains " + e
		}
	}
	s, path, err := r.settings(cwd)
	switch {
	case err != nil:
		return true, path + " cannot be read (" + err.Error() + "): not retained until it is fixed"
	case s.Retain != nil && !*s.Retain:
		return true, path + " sets retain = false"
	}
	return false, ""
}
