//go:build windows

// Package detach starts a program that outlives its caller: a hook's
// background flush, a server start, the native server's supervisor.
package detach

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The flag sets tried in turn: out of the caller's job object when it
// allows it (a hook runner's, Windows OpenSSH's: closing it would kill the
// program), else as detached as allowed.
var flagSets = []uint32{
	windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS | windows.CREATE_BREAKAWAY_FROM_JOB,
	windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
}

// Start starts exe with args out of the caller's process group, its
// output going to out (nil: discarded), and does not wait for it.
func Start(out *os.File, exe string, args ...string) error {
	var err error
	for _, flags := range flagSets {
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

// StartNoStdio is Start for a program that must get no standard handle
// at all, conhost.exe --headless: it reads its standard input as its
// terminal's and exits at once at its end, killing its program, and Go's
// os/exec always passes one (NUL when none is set). It runs in exe's
// directory, as a task's program does in System32, so that it holds no
// directory of the caller's.
func StartNoStdio(exe string, args ...string) error {
	line := make([]string, 0, len(args)+1)
	for _, a := range append([]string{exe}, args...) {
		line = append(line, syscall.EscapeArg(a))
	}
	app, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	cmdLine, err := windows.UTF16PtrFromString(strings.Join(line, " "))
	if err != nil {
		return err
	}
	dir, err := windows.UTF16PtrFromString(filepath.Dir(exe))
	if err != nil {
		return err
	}
	for _, flags := range flagSets {
		si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
		var pi windows.ProcessInformation
		err = windows.CreateProcess(app, cmdLine, nil, nil, false, flags|windows.CREATE_UNICODE_ENVIRONMENT, nil, dir, &si, &pi)
		if err == nil {
			_ = windows.CloseHandle(pi.Thread)
			return windows.CloseHandle(pi.Process)
		}
	}
	return err
}
