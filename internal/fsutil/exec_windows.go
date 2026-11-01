//go:build windows

package fsutil

import (
	"os"
	"path/filepath"
	"strings"
)

// Executable: path is a regular, non-empty file named *.exe (any case).
// Windows has no execute bit; the extension decides what can be run.
func Executable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0 && strings.EqualFold(filepath.Ext(path), ".exe")
}
