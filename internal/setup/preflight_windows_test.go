//go:build windows

package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/scheduler"
)

// conhost --headless runs here, as the server's task will run it.
func TestConhostHeadlessRunsForReal(t *testing.T) {
	if err := Conhost(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ConhostPath()); err != nil {
		t.Fatal(err)
	}
}

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// The real probe registers \<project>\probe with Task Scheduler and
// deletes it: nothing of it stays, its folder included (schtasks cannot
// delete one; scheduler.DeleteTaskFolder does).
func TestProbeTaskForReal(t *testing.T) {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	project := "bs-test-probe-" + hex.EncodeToString(rnd[:])
	t.Cleanup(func() {
		_, _ = run(context.Background(), "schtasks", "/Delete", "/F", "/TN", `\`+project+`\probe`)
		_, _ = scheduler.DeleteTaskFolder(project)
	})
	sid, err := UserSID()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "Jé & Co", "current.exe")
	if err := ProbeTask(context.Background(), run, scheduler.DeleteTaskFolder, sid, project, shim, dir); err != nil {
		t.Fatalf("Task Scheduler refused the probe: %v", err)
	}
	if out, err := run(context.Background(), "schtasks", "/Query", "/TN", `\`+project+`\probe`); err == nil {
		t.Errorf("the probe task stays:\n%s", out)
	}
	// Task Scheduler itself, through PowerShell's COM binding, says whether
	// the folder is there.
	out, err := run(context.Background(), "powershell", "-NoProfile", "-Command",
		`$s=New-Object -ComObject Schedule.Service; $s.Connect(); try { $null=$s.GetFolder('\`+project+`'); 'yes' } catch { 'no' }`)
	if err != nil || strings.TrimSpace(string(out)) != "no" {
		t.Errorf("the probe's folder \\%s stays (%v): %s", project, err, out)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left beside the XML: %v", left)
	}
}
