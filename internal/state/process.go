package state

import "time"

// ProcessAlive reports whether process pid runs and, where the system
// tells a start time, did not start after since (give or take
// startSlack): a PID reused by a later process counts as gone. PID 0 and
// negative PIDs name process groups, never a process: gone.
func ProcessAlive(pid int, since time.Time) bool {
	if pid <= 0 {
		return false
	}
	return ownerAlive(Owner{PID: pid, Since: since})
}
