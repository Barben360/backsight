//go:build !windows

package dashboard

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive flock on path, created 0600 if missing,
// without waiting: busy when another open file holds it. The kernel drops
// it when the holder exits, crash included.
func tryLock(path string) (unlock func(), busy bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return func() { _ = f.Close() }, false, nil
}
