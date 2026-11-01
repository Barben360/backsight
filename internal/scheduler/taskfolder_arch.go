//go:build windows && !amd64

package scheduler

import (
	"fmt"
	"runtime"
)

// DeleteTaskFolder is x64 only (taskfolder_windows.go): its COM calls
// follow the x64 calling convention. Elsewhere the folder is left, and
// the error says so.
func DeleteTaskFolder(name string) (deleted bool, err error) {
	return false, fmt.Errorf(`deleting the task folder \%s: not supported on windows/%s: delete it in Task Scheduler`, name, runtime.GOARCH)
}
