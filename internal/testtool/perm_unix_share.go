//go:build !windows

package testtool

import (
	"os"
	"testing"
)

// ShareWithOthers lets every other account read path (0644 for a file,
// 0755 for a directory on Unix), to test what refuses or tightens a path
// others can read.
func ShareWithOthers(t testing.TB, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o644)
	if st.IsDir() {
		mode = 0o755
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
