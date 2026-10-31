//go:build windows

package state

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// A process object outlives the process while any handle to it is open (a
// parent that has not waited, a scanner, the test that started it), so
// opening it proves nothing: the process is alive only while its object is
// not signalled. Access denied means another user's live process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// deletePending: creating path failed because its previous lock file is
// still being deleted. Windows keeps a deleted file's name until its last
// handle closes (a releasing owner's, a reader's), and refuses to create
// it meanwhile with access denied, not "exists": the lock is still held,
// for a moment. A name that is really gone was a genuine refusal.
func deletePending(path string, err error) bool {
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return false
	}
	_, serr := os.Lstat(path)
	return !errors.Is(serr, fs.ErrNotExist)
}
