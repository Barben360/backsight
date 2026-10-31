//go:build !windows

package supervise

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// errRunning: another supervisor of the project holds the lock.
var errRunning = errors.New("another supervisor runs")

// runDir holds the tests' locks and sockets: <tmp>/backsight-<uid>, 0700.
// Linux never runs the supervisor in the product (systemd's units do).
func runDir() (string, error) {
	dir := filepath.Join(os.TempDir(), "backsight-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, os.Chmod(dir, 0o700)
}

// acquire takes the project's single-instance lock: a flock, the stand-in
// for Windows' named mutex.
func acquire(project string) (release func(), err error) {
	dir, err := runDir()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, project+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errRunning
		}
		return nil, err
	}
	// Removed while still held: a test's lock files do not pile up. (A
	// racing opener of the old file would fail its LOCK_NB, as it would
	// have anyway.)
	return func() {
		_ = os.Remove(f.Name())
		_ = f.Close()
	}, nil
}
