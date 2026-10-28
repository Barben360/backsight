//go:build windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup  = 0x00000200
	detachedProcess        = 0x00000008
	createBreakawayFromJob = 0x01000000
)

// startDetachedTo starts `backsight <args>` out of the caller's process
// group, its output going to out (nil: discarded), and does not wait for it.
func startDetachedTo(out *os.File, args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Out of the hook runner's job object when it allows it, so closing
	// the job does not kill the flush; else as detached as allowed.
	for _, flags := range []uint32{createNewProcessGroup | detachedProcess | createBreakawayFromJob, createNewProcessGroup | detachedProcess} {
		cmd := exec.Command(exe, args...)
		if out != nil {
			cmd.Stdout, cmd.Stderr = out, out
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		if err = cmd.Start(); err == nil {
			return cmd.Process.Release()
		}
	}
	return err
}
