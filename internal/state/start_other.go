//go:build !linux && !windows

package state

import "time"

// Start times and boot ids cannot be told here: a live PID counts as its
// owner.
func processStart(int) (time.Time, bool) { return time.Time{}, false }

func startTicks(int) (int64, bool) { return 0, false }

func bootID() string { return "" }
