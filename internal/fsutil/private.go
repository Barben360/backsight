package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
)

// MkdirPrivate creates path and its parents (0700) when path is missing
// and, on Windows, gives the topmost directory it created a private DACL
// (MakePrivate), which what is created below inherits. An existing
// directory is left as its owner made it.
func MkdirPrivate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	top := path
	for {
		parent := filepath.Dir(top)
		if parent == top {
			break
		}
		if _, err := os.Stat(parent); err == nil {
			break
		}
		top = parent
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return MakePrivate(top)
	}
	return nil
}
