//go:build windows

package scheduler

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Task Scheduler accepts both jobs' tasks as written, for a standard user:
// they are registered from their files under a bs-test project (never the
// real backsight one), then deleted with their folder.
func TestJobsTasksRegisterForReal(t *testing.T) {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	project := "bs-test-jobs-" + hex.EncodeToString(rnd[:])
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p, err := For("windows", Options{Name: project, Shim: filepath.Join(dir, "Jé & Co", "current.exe"), State: dir,
		UserSID: u.User.Sid.String(), Env: map[string]string{"BACKSIGHT_HOME": filepath.Join(dir, "bs home")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, c := range p.Remove {
			_, _ = exec.Command(c[0], c[1:]...).CombinedOutput()
		}
		_, _ = exec.Command("powershell", "-NoProfile", "-Command",
			`$s=New-Object -ComObject Schedule.Service; $s.Connect(); $s.GetFolder('\').DeleteFolder('`+project+`', 0)`).CombinedOutput()
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
	for _, j := range Jobs {
		out, err := exec.Command("schtasks", "/Query", "/TN", project+`\`+j.Name, "/XML").CombinedOutput()
		if err != nil || !strings.Contains(string(out), "jobs "+j.Name+` --backsight-home "`) {
			t.Errorf("%s as registered (%v):\n%s", j.Name, err, out)
		}
	}
	for _, c := range p.Remove {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", strings.Join(c, " "), err, out)
		}
	}
}
