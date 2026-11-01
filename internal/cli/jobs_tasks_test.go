package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/testtool"
)

// The jobs' tasks on Windows (Task 12): faked schtasks, so that no test
// ever reaches a real task of project backsight.

// taskText decodes a task file written by jobs install.
func taskText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return utf16Text(t, raw)
}

// utf16Text decodes a task's XML: UTF-16LE after its BOM.
func utf16Text(t *testing.T, raw []byte) string {
	t.Helper()
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xFE || len(raw)%2 != 0 {
		t.Fatalf("not UTF-16LE with a BOM: % x", raw[:min(4, len(raw))])
	}
	u := make([]uint16, 0, len(raw)/2)
	for i := 2; i < len(raw); i += 2 {
		u = append(u, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return string(utf16.Decode(u))
}

// windowsInstall is a dev install whose scheduler is Task Scheduler
// (faked), with its shim.
func windowsInstall(t *testing.T) (string, *fakeSys) {
	t.Helper()
	home := devInstall(t, true)
	sys := withSys(t, "windows")
	_ = os.WriteFile(filepath.Join(pathsOf(home).Data, "bin", plugin.ShimName("windows")), []byte("MZ"), 0o700)
	return home, sys
}

// jobs install writes each task's XML under <state>\tasks, registers it
// from that file, and records the files; the task runs the shim with
// the install's BACKSIGHT_HOME as --backsight-home. jobs uninstall
// deletes both tasks by name and removes the files.
func TestJobsInstallRegistersTasksFromXML(t *testing.T) {
	home, sys := windowsInstall(t)
	d := pathsOf(home)
	if code, out, errOut := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("install: exit %d\n%s%s", code, out, errOut)
	}
	files := []string{filepath.Join(d.State, "tasks", "backsight", "tick.xml"), filepath.Join(d.State, "tasks", "backsight", "maintain.xml")}
	want := []string{
		"schtasks /Create /F /XML " + files[0] + ` /TN backsight\tick`,
		"schtasks /Create /F /XML " + files[1] + ` /TN backsight\maintain`,
	}
	if !slices.Equal(sys.calls, want) {
		t.Errorf("calls %q, want %q", sys.calls, want)
	}
	for i, j := range []string{"tick", "maintain"} {
		doc := taskText(t, files[i])
		if !strings.Contains(doc, " jobs "+j+" --backsight-home ") || !strings.Contains(doc, filepath.Join(home, "backsight")) {
			t.Errorf("%s task does not carry BACKSIGHT_HOME:\n%s", j, doc)
		}
		if !strings.Contains(doc, "<UserId>"+fakeUserSID+"</UserId>") {
			t.Errorf("%s task's principal is not the user's SID:\n%s", j, doc)
		}
	}
	st, _ := state.Load(d.StateFile())
	if st.Scheduler == nil || st.Scheduler.Kind != "schtasks" || !slices.Equal(st.Scheduler.Files, files) {
		t.Fatalf("state %+v", st.Scheduler)
	}

	sys.calls = nil
	if code, out, errOut := run(t, "jobs", "uninstall"); code != output.OK {
		t.Fatalf("uninstall: exit %d\n%s%s", code, out, errOut)
	}
	if want := []string{`schtasks /Delete /F /TN backsight\tick`, `schtasks /Delete /F /TN backsight\maintain`}; !slices.Equal(sys.calls, want) {
		t.Errorf("uninstall calls %q, want %q", sys.calls, want)
	}
	for _, f := range files {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s left behind (%v)", f, err)
		}
	}
	if st, _ := state.Load(d.StateFile()); st.Scheduler != nil {
		t.Errorf("record left: %+v", st.Scheduler)
	}
}

// A task whose deletion fails for another reason than its absence keeps
// its file and the record, so that a rerun finishes.
func TestJobsUninstallKeepsATaskItCouldNotDelete(t *testing.T) {
	home, sys := windowsInstall(t)
	d := pathsOf(home)
	run(t, "jobs", "install")
	sys.fail, sys.output = `schtasks /Delete /F /TN backsight\maintain`, "ERROR: Access is denied."
	if code, _, errOut := run(t, "jobs", "uninstall"); code != output.Blocked || !strings.Contains(errOut, "Access is denied") {
		t.Errorf("exit %d %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(d.State, "tasks", "backsight", "maintain.xml")); err != nil {
		t.Error("the file of a task still registered was removed")
	}
	if st, _ := state.Load(d.StateFile()); st.Scheduler == nil {
		t.Error("the record was dropped while a task may remain")
	}
	// Already gone is what uninstall wants.
	sys.fail, sys.output = `schtasks /Delete`, "ERROR: The system cannot find the file specified."
	if code, _, errOut := run(t, "jobs", "uninstall"); code != output.OK {
		t.Errorf("tasks already gone: exit %d %q", code, errOut)
	}
}

// A refresh rewrites a task whose file is not this binary's and
// registers it again from the file; an up-to-date one is left alone.
func TestJobsRefreshScheduledTasks(t *testing.T) {
	home, sys := windowsInstall(t)
	d := pathsOf(home)
	if code, _, errOut := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("install: exit %d %q", code, errOut)
	}
	sys.calls = nil
	if code, out, _ := run(t, "jobs", "refresh", "--json"); decodeRefresh(t, out).Status != "unchanged" || code != output.OK || len(sys.calls) != 0 {
		t.Errorf("fresh install: exit %d, calls %q\n%s", code, sys.calls, out)
	}
	tick := filepath.Join(d.State, "tasks", "backsight", "tick.xml")
	_ = os.WriteFile(tick, []byte("an older template"), 0o600)
	sys.calls = nil
	code, out, _ := run(t, "jobs", "refresh", "--json")
	want := []string{"schtasks /Create /F /XML " + tick + ` /TN backsight\tick`}
	if r := decodeRefresh(t, out); code != output.OK || r.Status != "done" || !slices.Equal(sys.calls, want) {
		t.Errorf("exit %d, calls %q, want %q\n%s", code, sys.calls, want, out)
	}
	if !strings.Contains(taskText(t, tick), " jobs tick") {
		t.Error("the task file was not rewritten")
	}
}

// A running job is its own lock held by a live process: its task is left
// for the next refresh (registering it again could end the run), without
// asking schtasks, whose text is localized. A lock whose owner is gone
// does not hold the refresh back.
func TestJobsRefreshDefersARunningTask(t *testing.T) {
	home, sys := windowsInstall(t)
	d := pathsOf(home)
	run(t, "jobs", "install")
	for _, j := range []string{"tick", "maintain"} {
		_ = os.WriteFile(filepath.Join(d.State, "tasks", "backsight", j+".xml"), []byte("old"), 0o600)
	}
	lock := filepath.Join(d.LocksDir(), "job-maintain.lock")
	holdLock(t, lock, testtool.OtherPID())
	creates := func() []string {
		var out []string
		for _, c := range sys.calls {
			if strings.HasPrefix(c, "schtasks /Create") {
				out = append(out, c)
			}
		}
		return out
	}
	sys.calls = nil
	code, out, _ := run(t, "jobs", "refresh", "--json")
	r := decodeRefresh(t, out)
	if c := creates(); code != output.OK || r.Status != "done" || strings.Join(r.Deferred, ",") != "maintain" || len(c) != 1 || !strings.HasSuffix(c[0], `/TN backsight\tick`) {
		t.Fatalf("exit %d, creates %q\n%s", code, c, out)
	}
	for _, c := range sys.calls {
		if strings.HasPrefix(c, "schtasks /Query") {
			t.Errorf("schtasks was asked whether a job runs: %s", c)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(d.State, "tasks", "backsight", "maintain.xml")); string(raw) != "old" {
		t.Error("the running job's task file was rewritten")
	}
	// The job ended without removing its lock (killed): not running.
	holdLock(t, lock, deadPID(t))
	sys.calls = nil
	code, out, _ = run(t, "jobs", "refresh", "--json")
	if c := creates(); code != output.OK || len(decodeRefresh(t, out).Deferred) != 0 || len(c) != 1 || !strings.HasSuffix(c[0], `/TN backsight\maintain`) {
		t.Errorf("next refresh: exit %d, creates %q\n%s", code, c, out)
	}
}

// holdLock writes a lock file as state.TryAcquire does, owned by pid.
func holdLock(t *testing.T, path string, pid int) {
	t.Helper()
	raw, _ := json.Marshal(state.Owner{PID: pid, Command: "jobs maintain"})
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// deadPID is the PID of a process that ran and exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// The task files are under the project's name, so a project renamed
// after jobs install behaves as on Linux: a refresh refuses (the entries
// are elsewhere), and uninstall deletes the tasks installed, never the new
// name's.
func TestJobsTasksOfARenamedProject(t *testing.T) {
	home, sys := windowsInstall(t)
	if code, _, errOut := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("install: exit %d %s", code, errOut)
	}
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, []byte(strings.Replace(string(raw), "[server]\n", "[server]\nproject = \"renamed\"\n", 1)), 0o600)
	sys.calls = nil
	if code, _, errOut := run(t, "jobs", "refresh"); code != output.Blocked || !strings.Contains(errOut, "jobs uninstall") || len(sys.calls) != 0 {
		t.Errorf("refresh: exit %d, calls %q: %s", code, sys.calls, errOut)
	}
	if code, _, errOut := run(t, "jobs", "uninstall"); code != output.OK {
		t.Fatalf("uninstall: exit %d %s", code, errOut)
	}
	if want := []string{`schtasks /Delete /F /TN backsight\tick`, `schtasks /Delete /F /TN backsight\maintain`}; !slices.Equal(sys.calls, want) {
		t.Errorf("uninstall calls %q, want %q", sys.calls, want)
	}
}

// A BACKSIGHT_STATE_DIR set at install reaches each task as
// --backsight-state-dir, and the lock a refresh probes is the one the job
// then takes, under that state directory.
func TestJobsTasksCarryTheStateDir(t *testing.T) {
	home, sys := windowsInstall(t)
	st := filepath.Join(home, "other state")
	t.Setenv("BACKSIGHT_STATE_DIR", st)
	if code, _, errOut := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("install: exit %d %s", code, errOut)
	}
	tick := filepath.Join(st, "tasks", "backsight", "tick.xml")
	if doc := taskText(t, tick); !strings.Contains(doc, " --backsight-state-dir ") || !strings.Contains(doc, st) || !strings.Contains(doc, " --backsight-home ") {
		t.Errorf("the task does not carry both directories:\n%s", doc)
	}
	_ = os.WriteFile(tick, []byte("old"), 0o600)
	holdLock(t, filepath.Join(st, "locks", "job-tick.lock"), testtool.OtherPID())
	sys.calls = nil
	code, out, _ := run(t, "jobs", "refresh", "--json")
	if r := decodeRefresh(t, out); code != output.OK || strings.Join(r.Deferred, ",") != "tick" {
		t.Errorf("the job's lock under the state dir was not seen: exit %d\n%s", code, out)
	}
}
