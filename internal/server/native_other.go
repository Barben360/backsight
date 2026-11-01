//go:build !windows

package server

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// userPg0Home: pg0's directory is the user's ~/.pg0 here; the Windows
// install code runs only on Windows (tests on Linux replace it).
func userPg0Home() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pg0"), nil
}

// crossVolume: a rename failed because its two paths are on different
// file systems.
func crossVolume(err error) bool { return errors.Is(err, syscall.EXDEV) }

// straysWindows needs Windows' process snapshot.
func (Native) straysWindows() ([]Stray, error) {
	return nil, errors.New("the Windows stray scan runs on Windows only")
}

func (Native) strayRunning(int) bool { return false }
