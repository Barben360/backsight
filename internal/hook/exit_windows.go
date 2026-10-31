//go:build windows

package hook

// childFailed is the child's exit code when its handler fails. Windows has
// no signals: TerminateProcess and a Job kill end a process with the code
// the killer picks, 1 by convention (Go's Process.Kill, taskkill /F, Node's
// kill), so the child's own failure must not be 1. A Go panic exits 2.
const childFailed = 3

// killedBy: exit code 1 is a kill, since the child never exits 1 itself.
func killedBy(code int) bool { return code == -1 || code == 1 }
