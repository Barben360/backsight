//go:build !windows

package testtool

import (
	"os"
	"testing"
)

// DenyWrite makes writing into path (a directory: creating files in it; a
// file: changing it) fail for this user, until the returned function or the
// end of the test restores it. A test that runs as root must skip itself:
// root ignores the permission.
func DenyWrite(t testing.TB, path string) (restore func()) {
	t.Helper()
	return chmodTemporarily(t, path, func(m os.FileMode) os.FileMode {
		if m.IsDir() {
			return 0o500
		}
		return 0o400
	})
}

// DenyRead makes reading path fail for this user, as DenyWrite does for
// writing.
func DenyRead(t testing.TB, path string) (restore func()) {
	t.Helper()
	return chmodTemporarily(t, path, func(os.FileMode) os.FileMode { return 0 })
}

func chmodTemporarily(t testing.TB, path string, to func(os.FileMode) os.FileMode) func() {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, to(st.Mode())); err != nil {
		t.Fatal(err)
	}
	restore := func() { _ = os.Chmod(path, st.Mode().Perm()) }
	t.Cleanup(restore)
	return restore
}
