//go:build !linux

package state

import "time"

// processStart cannot be told here: a live PID counts as its owner.
func processStart(int) (time.Time, bool) { return time.Time{}, false }
