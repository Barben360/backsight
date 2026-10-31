package testtool

import (
	"io/fs"
	"os"
	"runtime"
	"testing"

	"github.com/Barben360/backsight/internal/fsutil"
)

// RequirePrivate fails unless path is its owner's alone: on Unix exactly
// 0600 (a file) or 0700 (a directory); on Windows no account beyond the
// user, SYSTEM and Administrators can read it (fsutil.OtherReaders).
func RequirePrivate(t testing.TB, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Errorf("%s: %v", path, err)
		return
	}
	if runtime.GOOS == "windows" {
		if who, err := fsutil.OtherReaders(path); err != nil || len(who) != 0 {
			t.Errorf("%s can be read by %v (%v)", path, who, err)
		}
		return
	}
	want := fs.FileMode(0o600)
	if st.IsDir() {
		want = 0o700
	}
	if got := st.Mode().Perm(); got != want {
		t.Errorf("%s has mode %04o, want %04o", path, got, want)
	}
}

// RequireUnixMode fails when path's permission bits are not want on Unix;
// Windows has no such bits (what it keeps private is RequirePrivate's).
func RequireUnixMode(t testing.TB, path string, want fs.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Errorf("%s: %v", path, err)
		return
	}
	if got := st.Mode().Perm(); got != want {
		t.Errorf("%s has mode %04o, want %04o", path, got, want)
	}
}
