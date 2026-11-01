//go:build windows && amd64

package scheduler

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// folderExists asks Task Scheduler itself, through PowerShell's COM
// binding (not the code under test), whether \name exists.
func folderExists(t *testing.T, name string) bool {
	t.Helper()
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		`$s=New-Object -ComObject Schedule.Service; $s.Connect(); try { $null=$s.GetFolder('\`+name+`'); 'yes' } catch { 'no' }`).CombinedOutput()
	if err != nil {
		t.Fatalf("asking for the folder \\%s: %v\n%s", name, err, out)
	}
	return strings.TrimSpace(string(out)) == "yes"
}

// The ruling: schtasks /Delete leaves the task's folder; DeleteTaskFolder
// removes it once empty, refuses it while a task is in it, and takes a
// folder that is not there as done. For real, for a standard user, under
// a bs-test project.
func TestDeleteTaskFolderForReal(t *testing.T) {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	project := "bs-test-folder-" + hex.EncodeToString(rnd[:])
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p, err := For("windows", Options{Name: project, Shim: filepath.Join(dir, "current.exe"), State: dir, UserSID: u.User.Sid.String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, c := range p.Remove {
			_, _ = exec.Command(c[0], c[1:]...).CombinedOutput()
		}
		_, _ = DeleteTaskFolder(project)
	})
	for _, f := range p.Files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.Path, []byte(f.Content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range p.Activate {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(c, " "), err, out)
		}
	}

	// Its tasks are still in it: refused, and they stay.
	deleted, err := DeleteTaskFolder(project)
	if deleted || err == nil || !errors.Is(err, syscall.Errno(145)) { // ERROR_DIR_NOT_EMPTY
		t.Errorf("a folder holding tasks: %v, want ERROR_DIR_NOT_EMPTY", err)
	}
	if out, err := exec.Command("schtasks", "/Query", "/TN", `\`+project+`\tick`).CombinedOutput(); err != nil {
		t.Fatalf("the refused delete took a task with it: %v\n%s", err, out)
	}

	for _, c := range p.Remove {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(c, " "), err, out)
		}
	}
	if !folderExists(t, project) {
		t.Fatal("schtasks /Delete removed the folder itself: nothing left to test")
	}
	if deleted, err := DeleteTaskFolder(project); err != nil || !deleted {
		t.Fatalf("the empty folder: deleted %v, %v", deleted, err)
	}
	if folderExists(t, project) {
		t.Error("the empty folder is still there")
	}
	if deleted, err := DeleteTaskFolder(project); err != nil || deleted {
		t.Errorf("a folder that is not there: deleted %v, %v", deleted, err)
	}
}

// Only a folder directly under the root is ever named: never the root, a
// nested path or an empty name.
func TestDeleteTaskFolderRefusesAPath(t *testing.T) {
	for _, name := range []string{"", `\`, `a\b`, "a/b", `\bs-test`} {
		if _, err := DeleteTaskFolder(name); err == nil {
			t.Errorf("%q: accepted", name)
		}
	}
}
