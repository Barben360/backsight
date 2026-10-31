//go:build windows

package dashboard

import (
	"errors"
	"syscall"
)

// errSharingViolation is ERROR_SHARING_VIOLATION, which the standard
// library does not name.
const errSharingViolation syscall.Errno = 32

// tryLock opens path, created if missing, with no sharing: busy while
// another handle has it open. Windows closes the handle when the holder
// exits, crash included.
func tryLock(path string) (unlock func(), busy bool, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, errSharingViolation) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return func() { _ = syscall.CloseHandle(h) }, false, nil
}
