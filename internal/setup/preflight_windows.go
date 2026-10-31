//go:build windows

package setup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

// conhostWait bounds conhost's probe run.
const conhostWait = 10 * time.Second

// ConhostPath is %SystemRoot%\System32\conhost.exe, what the server's
// task runs.
func ConhostPath() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "conhost.exe")
}

// Conhost runs conhost.exe --headless cmd.exe /c exit: it must exist and
// run a program with no window (E34 W2).
func Conhost(ctx context.Context) error {
	exe := ConhostPath()
	if _, err := os.Stat(exe); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, conhostWait)
	defer cancel()
	cmd := filepath.Join(filepath.Dir(exe), "cmd.exe")
	if out, err := exec.CommandContext(ctx, exe, "--headless", cmd, "/c", "exit").CombinedOutput(); err != nil {
		return fmt.Errorf("%s --headless: %w %s", exe, err, out)
	}
	return nil
}

// UserSID is this process's user as an SDDL string: the task's principal.
func UserSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}
