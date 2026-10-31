//go:build !windows

package hook

// childFailed is the child's exit code when its handler fails.
const childFailed = 1

// killedBy: a child ended by a signal has no exit code.
func killedBy(code int) bool { return code == -1 }
