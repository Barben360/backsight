//go:build !linux && !darwin

package session

import "time"

// ProcessStart cannot tell a process's start time on this system.
func ProcessStart(int) (time.Time, bool) { return time.Time{}, false }
