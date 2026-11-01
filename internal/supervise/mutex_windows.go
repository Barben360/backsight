//go:build windows

package supervise

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// errRunning: another supervisor of the project holds the mutex.
var errRunning = errors.New("another supervisor runs")

// acquire takes the project's single-instance mutex,
// Global\backsight-<project>-<user SID>-supervisor; release closes it (the
// process's exit does too, however it ends).
func acquire(project string) (release func(), err error) {
	name, err := objectName(project)
	if err != nil {
		return nil, err
	}
	sa, _, err := userOnly()
	if err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(`Global\` + name + "-supervisor")
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateMutex(sa, false, p)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(h)
		return nil, errRunning
	}
	if err != nil {
		// Access denied: another account made the name first.
		return nil, fmt.Errorf("the mutex Global\\%s-supervisor: %w", name, err)
	}
	return func() { _ = windows.CloseHandle(h) }, nil
}
