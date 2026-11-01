//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// heldError: Windows refused to delete a file because a program has it
// open or runs it (every Claude Code session runs current.exe): it can
// still be renamed, into the trash.
func heldError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// cleanerScript is cmd.exe's command: delete the trash, again every few
// seconds until it is gone or the tries are spent, then the data root
// when nothing else is left in it. The path is read from BACKSIGHT_TRASH
// only, never written here: a path with & or % is never parsed as
// cmd.exe syntax.
func cleanerScript(tries, pings int) string {
	return fmt.Sprintf(`(for /L %%i in (1,1,%d) do @if exist "%%BACKSIGHT_TRASH%%\" (rmdir /s /q "%%BACKSIGHT_TRASH%%" 2>nul & `+
		`if exist "%%BACKSIGHT_TRASH%%\" ping -n %d 127.0.0.1 >nul)) & rmdir "%%BACKSIGHT_TRASH%%\.." 2>nul`, tries, pings)
}

// cleanerCommand is the detached cleaner of trash: cmd.exe /d /q /c (no
// AutoRun, no echo) from System32, without a window, its environment the
// trash path and what ping needs, its directory System32 (it holds none
// of the data root's).
func cleanerCommand(trash string) *exec.Cmd {
	sys := os.Getenv("SystemRoot")
	if sys == "" {
		sys = `C:\Windows`
	}
	system32 := filepath.Join(sys, "System32")
	cmd := exec.Command(filepath.Join(system32, "cmd.exe"))
	cmd.Dir = system32
	cmd.Env = []string{"SystemRoot=" + sys, "PATH=" + system32, "BACKSIGHT_TRASH=" + trash}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       `cmd.exe /d /q /c ` + cleanerScript(cleanerTries, cleanerPings),
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB,
	}
	return cmd
}

// startTrashCleaner starts the cleaner and does not wait for it. Out of
// the caller's job when it allows it (Windows OpenSSH's, a terminal's:
// closing it would kill the cleaner), else as detached as allowed.
func startTrashCleaner(trash string) error {
	cmd := cleanerCommand(trash)
	err := cmd.Start()
	if err != nil {
		cmd = cleanerCommand(trash)
		cmd.SysProcAttr.CreationFlags &^= windows.CREATE_BREAKAWAY_FROM_JOB
		if err = cmd.Start(); err != nil {
			return err
		}
	}
	return cmd.Process.Release()
}
