//go:build windows

package server

import (
	"os"
	"os/exec"
	"syscall"
)

// CREATE_NEW_PROCESS_GROUP: a console Ctrl-C does not reach the child.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

var generateConsoleCtrlEvent = syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")

// terminateGroup sends CTRL_BREAK_EVENT to the process group p leads
// (OwnGroup), which a Go child receives as os.Interrupt; without a console
// it falls back to killing p.
func terminateGroup(p *os.Process) error {
	const ctrlBreakEvent = 1
	if r, _, _ := generateConsoleCtrlEvent.Call(ctrlBreakEvent, uintptr(p.Pid)); r != 0 {
		return nil
	}
	return p.Kill()
}
