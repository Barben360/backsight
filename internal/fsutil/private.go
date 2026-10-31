package fsutil

import (
	"os"
	"runtime"
)

// MkdirPrivate creates path and its parents (0700) when path is missing
// and, on Windows, gives path a private DACL (MakePrivate). An existing
// directory is left as its owner made it.
func MkdirPrivate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return MakePrivate(path)
	}
	return nil
}
