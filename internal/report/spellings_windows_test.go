//go:build windows

package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/fsutil"
)

// The home is scrubbed however Windows or Git Bash spells it: a TEMP or
// USERPROFILE in 8.3 form (C:\Users\RUNNER~1) for a home known by its
// long name and the reverse, a lower-case drive, /c/Users/....
func TestScrubEverySpellingOfAWindowsHome(t *testing.T) {
	long := filepath.Join(t.TempDir(), "A Long Home Directory Name")
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	long, _ = fsutil.LongPathName(long)
	short, err := fsutil.ShortPathName(long)
	if err != nil {
		t.Fatal(err)
	}
	if short == long {
		t.Skip("this volume keeps no 8.3 names")
	}
	lower := strings.ToLower(long[:1]) + long[1:]
	// An 8.3 ancestor with the rest long, as a TEMP of C:\Users\RUNNER~1
	// gives: the parent short, the home's own name long.
	parent, err := fsutil.ShortPathName(filepath.Dir(long))
	if err != nil {
		t.Fatal(err)
	}
	mixed := filepath.Join(parent, filepath.Base(long))
	bash := "/" + strings.ToLower(long[:1]) + filepath.ToSlash(long[2:])
	for _, home := range []string{long, short} {
		s := NewScrubber(home)
		for _, p := range []string{long, short, mixed, lower, filepath.ToSlash(short), bash} {
			if got := s.String("open " + p + `\x.json: denied`); got != `open ~\x.json: denied` && got != `open ~/x.json: denied` {
				t.Errorf("home %s: %q → %q", home, p, got)
			}
		}
	}
}
