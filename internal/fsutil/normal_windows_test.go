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

// git's forward slashes, an 8.3 name from the OS (RUNNER~1), a lower-case
// drive letter and a path typed in another case (C:\code\Shop for the
// disk's C:\code\shop: the volume is case-blind) all name one directory,
// in the disk's own case.
func TestNormalGivesOneSpellingPerPath(t *testing.T) {
	long, short := shortDir(t)
	want := strings.ToUpper(long[:1]) + long[1:]
	for _, in := range []string{long, short, filepath.ToSlash(short), strings.ToLower(short[:1]) + short[1:], short + `\`, short + `\sub\..`,
		strings.ToUpper(long), strings.ToLower(long), strings.ToLower(filepath.ToSlash(short))} {
		if got := Normal(in); got != want {
			t.Errorf("Normal(%q) = %q, want %q", in, got, want)
		}
	}
	// A path that does not exist (a removed worktree) keeps its tail, as
	// typed, after its existing part in the disk's case.
	if got, w := Normal(filepath.Join(short, "gone", "x.go")), filepath.Join(want, "gone", "x.go"); got != w {
		t.Errorf("missing tail: %q, want %q", got, w)
	}
	if got, w := Normal(filepath.Join(strings.ToUpper(long), "Gone", "X.go")), filepath.Join(want, "Gone", "X.go"); got != w {
		t.Errorf("missing tail under another case: %q, want %q", got, w)
	}
	if Normal("") != "" {
		t.Error("an empty path must stay empty")
	}
}
