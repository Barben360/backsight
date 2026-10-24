//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

func startDetached(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // its own session: not killed with the hook
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
