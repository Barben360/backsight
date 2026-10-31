//go:build !windows

package supervise

import (
	"errors"
	"os"
	"syscall"
)

// joinJob: no Job Object here; the tests' children are in process groups
// of their own, which stop and kill reach.
func joinJob(string) error { return nil }

// interrupt sends SIGTERM to the group p leads.
func interrupt(p *os.Process) error { return signalGroup(p, syscall.SIGTERM) }

// kill sends SIGKILL to the group p leads.
func kill(p *os.Process) error { return signalGroup(p, syscall.SIGKILL) }

func signalGroup(p *os.Process, sig syscall.Signal) error {
	err := syscall.Kill(-p.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
