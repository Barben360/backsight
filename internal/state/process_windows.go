//go:build windows

package state

import (
	"errors"

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

// createRefused: an exclusive create failed with access denied, which
// Windows answers for a name whose file is being deleted (it keeps the
// name until the last handle closes) as for a real refusal.
func createRefused(err error) bool { return errors.Is(err, windows.ERROR_ACCESS_DENIED) }
