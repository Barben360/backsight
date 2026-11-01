//go:build windows

package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapCase is p in other letters' case: C:\Users\x is c:\uSERS\X. The
// volume is case-blind: it is the same directory.
func swapCase(p string) string {
	return strings.Map(func(r rune) rune {
		if l := strings.ToLower(string(r)); l != string(r) {
			return []rune(l)[0]
		}
		return []rune(strings.ToUpper(string(r)))[0]
	}, p)
}

// A cwd spelled in another case than the disk's (C:\code\Shop for
// C:\code\shop) is the same directory: the home directory still stops the
// .backsight.toml walk, so a file above it is never trusted, and the home
// itself is still the transverse pool.
func TestMixedCaseCwdKeepsTheHomeStop(t *testing.T) {
	parent := t.TempDir()
	write(t, filepath.Join(parent, ".backsight.toml"), `project = "outside"`+"\n")
	home := filepath.Join(parent, "Home")
	notes := filepath.Join(home, "Notes")
	for _, d := range []string{notes, filepath.Join(home, ".claude", "skills")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r := New(home, []string{".claude", "Home"})
	for _, cwd := range []string{notes, swapCase(notes), strings.ToLower(notes)} {
		if name, strong := r.Named(cwd); name != "Notes" || strong {
			t.Errorf("%s: %q strong=%v, want the weak Notes (a file above the home was read?)", cwd, name, strong)
		}
	}
	for _, cwd := range []string{swapCase(home), strings.ToUpper(filepath.Join(home, ".claude", "skills"))} {
		if name, strong := r.Named(cwd); name != ".claude" || !strong {
			t.Errorf("%s: %q strong=%v, want the transverse pool", cwd, name, strong)
		}
	}
}
