//go:build !windows

package server

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// terminateGroup sends SIGTERM to the process group p leads (OwnGroup).
func terminateGroup(p *os.Process) error {
	err := syscall.Kill(-p.Pid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
