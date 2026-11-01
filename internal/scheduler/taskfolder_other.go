//go:build !windows

package scheduler

// DeleteTaskFolder deletes Task Scheduler's folder \<name>: Windows only;
// elsewhere there is none.
func DeleteTaskFolder(name string) (deleted bool, err error) { return false, nil }
