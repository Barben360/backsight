//go:build windows

package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortDir makes a directory with a long name and returns its long and
// 8.3 spellings; it skips where the volume keeps no 8.3 names.
func shortDir(t *testing.T) (long, short string) {
	t.Helper()
	long = filepath.Join(t.TempDir(), "A Long Directory Name For 8dot3")
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	long, err := LongPathName(long)
	if err != nil {
		t.Fatal(err)
	}
	short, err = ShortPathName(long)
	if err != nil {
		t.Fatal(err)
	}
	if short == long {
		t.Skip("this volume keeps no 8.3 names")
	}
	return long, short
}

// git's forward slashes, an 8.3 name from the OS (RUNNER~1) and a
// lower-case drive letter all name one directory.
func TestNormalGivesOneSpellingPerPath(t *testing.T) {
	long, short := shortDir(t)
	want := strings.ToUpper(long[:1]) + long[1:]
	for _, in := range []string{long, short, filepath.ToSlash(short), strings.ToLower(short[:1]) + short[1:], short + `\`, short + `\sub\..`} {
		if got := Normal(in); got != want {
			t.Errorf("Normal(%q) = %q, want %q", in, got, want)
		}
	}
	// A path that does not exist (a removed worktree) keeps its tail.
	if got, w := Normal(filepath.Join(short, "gone", "x.go")), filepath.Join(want, "gone", "x.go"); got != w {
		t.Errorf("missing tail: %q, want %q", got, w)
	}
	if Normal("") != "" {
		t.Error("an empty path must stay empty")
	}
}
