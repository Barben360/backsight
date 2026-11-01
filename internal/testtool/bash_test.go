package testtool

import (
	"errors"
	"path/filepath"
	"testing"
)

// findBash on Windows: git's own install first (from cmd\, bin\ or
// mingw64\bin\), then Program Files, the per-user install, PortableGit;
// bin\bash.exe before usr\bin\bash.exe; never a bash from PATH.
func TestFindBashOrder(t *testing.T) {
	root := func(parts ...string) string { return filepath.Join(append([]string{"V"}, parts...)...) }
	env := map[string]string{"ProgramFiles": root("PF"), "LOCALAPPDATA": root("LA")}
	getenv := func(k string) string { return env[k] }
	for name, c := range map[string]struct {
		git   string
		files []string
		want  string
	}{
		"git's install wins": {root("Mine", "Git", "cmd", "git.exe"),
			[]string{root("PF", "Git", "bin", "bash.exe"), root("Mine", "Git", "bin", "bash.exe")}, root("Mine", "Git", "bin", "bash.exe")},
		"from mingw64": {root("Mine", "Git", "mingw64", "bin", "git.exe"),
			[]string{root("Mine", "Git", "usr", "bin", "bash.exe")}, root("Mine", "Git", "usr", "bin", "bash.exe")},
		"bin before usr": {"", []string{root("PF", "Git", "usr", "bin", "bash.exe"), root("PF", "Git", "bin", "bash.exe")}, root("PF", "Git", "bin", "bash.exe")},
		"per user":       {"", []string{root("LA", "Programs", "Git", "bin", "bash.exe"), root("LA", "PortableGit", "bin", "bash.exe")}, root("LA", "Programs", "Git", "bin", "bash.exe")},
		"portable":       {"", []string{root("LA", "PortableGit", "bin", "bash.exe")}, root("LA", "PortableGit", "bin", "bash.exe")},
		"none":           {"", []string{root("Windows", "System32", "bash.exe")}, ""},
	} {
		look := func(name string) (string, error) {
			switch {
			case name == "git" && c.git != "":
				return c.git, nil
			case name == "bash":
				return root("Windows", "System32", "bash.exe"), nil // WSL's: never taken
			}
			return "", errors.New("not found")
		}
		isFile := func(p string) bool {
			for _, f := range c.files {
				if f == p {
					return true
				}
			}
			return false
		}
		if got := findBash("windows", look, getenv, isFile); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	// Elsewhere bash is PATH's.
	if got := findBash("linux", func(string) (string, error) { return "/usr/bin/bash", nil }, getenv, func(string) bool { return false }); got != "/usr/bin/bash" {
		t.Errorf("linux: %q", got)
	}
}
