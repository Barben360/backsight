//go:build !windows

package fsutil

import "os"

// Executable: path is a regular, non-empty file with an execute bit (a
// dangling link, a directory and an empty file are not).
func Executable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0 && st.Mode().Perm()&0o111 != 0
}
