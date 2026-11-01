//go:build windows

package attribution

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Claude Code on Windows names paths as C:\x, C:/x (git, Git Bash's
// tools), \\?\C:\x (long paths) and /c/x (Git Bash): each is evidence for
// the same directory, as /home/… is on Linux. System directories never
// are, and a redirect to a drive path writes.
func TestAttributionSeesWindowsPaths(t *testing.T) {
	home := `C:\Users\u`
	x := `C:\Users\u\src\a\x.go`
	a := `C:\Users\u\src\a`
	t.Setenv("SystemRoot", `C:\Windows`)
	cases := []struct {
		tool  string
		in    []string
		cwd   string
		paths []string
		write bool
	}{
		{"Read", []string{"file_path", x}, "", []string{x}, false},
		{"Read", []string{"file_path", `C:/Users/u/src/a/x.go`}, "", []string{x}, false},
		{"Read", []string{"file_path", `\\?\C:\Users\u\src\a\x.go`}, "", []string{x}, false},
		{"Read", []string{"file_path", `x.go`}, a, []string{x}, false},
		{"Edit", []string{"file_path", `c:\Users\u\src\a\x.go`, "old_string", "a", "new_string", "b"}, "", []string{`c:\Users\u\src\a\x.go`}, true},
		{"Bash", []string{"command", `cd C:/Users/u/src/a && go test ./...`}, home, []string{a, a}, false},
		{"Bash", []string{"command", `git -C 'C:\Users\u\src\b' status`}, home, []string{`C:\Users\u\src\b`, `C:\Users\u\src\b`}, false},
		{"Bash", []string{"command", `cat /c/Users/u/src/a/x.go`}, home, []string{x}, false},
		{"Bash", []string{"command", `echo hi > C:/Users/u/src/a/out.txt`}, "", []string{`C:\Users\u\src\a\out.txt`}, true},
		{"Bash", []string{"command", `type C:\Windows\System32\drivers\etc\hosts c:/windows/win.ini`}, home, nil, false},
	}
	for _, c := range cases {
		got := CallOf(c.tool, input(c.in...), c.cwd, home)
		slices.Sort(got.Paths)
		if !slices.Equal(got.Paths, c.paths) || got.Write != c.write {
			t.Errorf("%s %q: paths %q write %v, want %q %v", c.tool, c.in, got.Paths, got.Write, c.paths, c.write)
		}
	}
}

// Every spelling of a path into an own repository counts for it, a
// mixed-case one included: the volume is case-blind, git echoes the case
// it was given, and the repository is under the home whatever the case.
func TestAttributionCountsAnyWindowsSpellingOfARepository(t *testing.T) {
	w := newWorld(t)
	r := w.repos(w.plain, "github.com/me")
	file := filepath.Join(w.alpha, "src", "main.go")
	for _, p := range []string{file, filepath.ToSlash(file), `\\?\` + file, strings.ToUpper(file), strings.ToLower(file)} {
		if got := r.Project(p, nil); got != "alpha" {
			t.Errorf("%s: %q, want alpha", p, got)
		}
	}
	// A Bash call naming the repository in another case votes for it.
	v := Tally([]Call{CallOf("Bash", input("command", "cd "+strings.ToUpper(w.beta)+" && git status"), w.home, w.home)}, r)
	if p, n := top(v.Explicit); p != "beta" || n == 0 {
		t.Errorf("votes %+v, want beta", v)
	}
}
