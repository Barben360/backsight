package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/testtool"
)

type fakeSys struct {
	calls  []string
	fail   string            // a command line prefix that fails
	output string            // what a failing command prints
	xdg    string            // the user manager's XDG_CONFIG_HOME
	out    map[string]string // what a command line prints
	// noBus: no systemd user session, every systemctl --user call fails
	noBus bool
	// hang: the user manager never answers show-environment
	hang bool
}

const busError = "Failed to connect to bus: No medium found"

func (f *fakeSys) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	if f.hang && line == "systemctl --user show-environment" {
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	}
	if f.noBus && strings.HasPrefix(line, "systemctl --user") {
		f.calls = append(f.calls, line)
		return []byte(busError + "\n"), errors.New("exit status 1")
	}
	if line == "systemctl --user show-environment" {
		return []byte("HOME=/home/u\nXDG_CONFIG_HOME=" + f.xdg + "\n"), nil
	}
	f.calls = append(f.calls, line)
	if o, ok := f.out[line]; ok {
		return []byte(o), nil
	}
	if f.fail != "" && strings.HasPrefix(line, f.fail) {
		out := f.output
		if out == "" {
			out = "unit failed"
		}
		return []byte(out), errors.New("exit status 1")
	}
	f.links(line)
	return nil, nil
}

// links does what systemd does on disk: enable links each timer into
// timers.target.wants beside the units, disable removes the links.
func (f *fakeSys) links(line string) {
	if f.xdg == "" {
		return
	}
	wants := filepath.Join(f.xdg, "systemd", "user", "timers.target.wants")
	for _, verb := range []string{"enable", "disable"} {
		rest, ok := strings.CutPrefix(line, "systemctl --user "+verb+" --now ")
		if !ok {
			continue
		}
		for _, timer := range strings.Fields(rest) {
			link := filepath.Join(wants, timer)
			if verb == "disable" {
				_ = os.Remove(link)
				continue
			}
			_ = os.MkdirAll(wants, 0o700)
			_ = os.Symlink(filepath.Join("..", timer), link)
		}
	}
}

// withSys fakes the OS scheduler for an install in home, with its shim.
func withSys(t *testing.T, goos string) *fakeSys {
	t.Helper()
	f := &fakeSys{}
	if home := os.Getenv("HOME"); home != "" {
		f.xdg = filepath.Join(home, "xdg")
		shim := filepath.Join(pathsOf(home).Data, "bin", "current")
		_ = os.MkdirAll(filepath.Dir(shim), 0o700)
		_ = os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o700)
	}
	oldR, oldG := sysRunner, schedulerGOOS
	sysRunner, schedulerGOOS = f, goos
	t.Cleanup(func() { sysRunner, schedulerGOOS = oldR, oldG })
	return f
}

func TestJobsInstallPlanThenInstallThenUninstall(t *testing.T) {
	testtool.SkipWindows(t, "Task 12: scheduler entries are systemd units")
	home := devInstall(t, true)
	t.Setenv("BACKSIGHT_HOME", filepath.Join(home, "backsight"))
	sys := withSys(t, "linux")
	timer := filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")
	code, out, _ := run(t, "jobs", "install", "--plan")
	if _, err := os.Stat(timer); code != output.OK || err == nil || len(sys.calls) != 0 || !strings.Contains(out, "would write") || !strings.Contains(out, "would run") {
		t.Fatalf("plan: exit %d, calls %q\n%s", code, sys.calls, out)
	}
	if code, out, _ := run(t, "jobs", "install"); code != output.OK || !strings.Contains(readFile(t, timer), "OnCalendar=*:0/15") {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	// the job runs with the install's directories, not the manager's
	if svc := readFile(t, strings.TrimSuffix(timer, ".timer")+".service"); !strings.Contains(svc, "Environment=BACKSIGHT_HOME="+filepath.Join(home, "backsight")+"\n") {
		t.Errorf("service:\n%s", svc)
	}
	if len(sys.calls) != 2 || sys.calls[1] != "systemctl --user enable --now backsight-tick.timer backsight-maintain.timer" {
		t.Errorf("calls %q", sys.calls)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if st.Scheduler == nil || st.Scheduler.Kind != "systemd" || len(st.Scheduler.Files) != 4 {
		t.Errorf("state %+v", st.Scheduler)
	}
	if _, out, _ := run(t, "jobs", "install", "--plan"); strings.Contains(out, "would write") {
		t.Errorf("a second install changes nothing:\n%s", out)
	}
	sys.calls = nil
	if code, _, _ := run(t, "jobs", "uninstall"); code != output.OK {
		t.Fatalf("uninstall: exit %d", code)
	}
	if _, err := os.Stat(timer); err == nil || sys.calls[0] != "systemctl --user disable --now backsight-tick.timer backsight-maintain.timer" {
		t.Errorf("file left or calls %q", sys.calls)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.Scheduler != nil {
		t.Errorf("state kept: %+v", st.Scheduler)
	}
}

func TestJobsInstallReportsAFailingActivation(t *testing.T) {
	home := devInstall(t, true)
	sys := withSys(t, "linux")
	sys.fail = "systemctl --user enable"
	code, _, errOut := run(t, "jobs", "install")
	// the files are recorded, so that uninstall finds them
	if st, _ := state.Load(pathsOf(home).StateFile()); code != output.Blocked || !strings.Contains(errOut, "unit failed") || !strings.Contains(errOut, "jobs uninstall") ||
		st.Scheduler == nil || len(st.Scheduler.Files) != 4 {
		t.Errorf("exit %d %q, state %+v", code, errOut, st.Scheduler)
	}
}

// jobs maintain rotates telemetry past 13 months, writes a doctor
// snapshot and logs its run; it skips during maintenance.
func TestJobsMaintain(t *testing.T) {
	testtool.SkipWindows(t, "Task 12: scheduler entries are systemd units")
	home := devInstall(t, true)
	d := pathsOf(home)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[backup]\nenabled = false\n")...), 0o600)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	_ = os.MkdirAll(d.TelemetryDir(), 0o700)
	for _, m := range []string{"2025-09", "2025-10", "2026-10"} {
		_ = os.WriteFile(filepath.Join(d.TelemetryDir(), "events-"+m+".jsonl"), []byte("{}\n"), 0o600)
	}
	code, out, _ := run(t, "jobs", "maintain")
	if code != output.OK || !strings.Contains(out, "1 telemetry file(s) rotated") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(d.TelemetryDir(), "events-2025-09.jsonl")); err == nil {
		t.Error("a 14-month-old file kept")
	}
	if _, err := os.Stat(filepath.Join(d.TelemetryDir(), "events-2025-10.jsonl")); err != nil {
		t.Error("the 13th month removed")
	}
	if raw, err := os.ReadFile(filepath.Join(d.State, "doctor-snapshot.json")); err != nil || !strings.Contains(string(raw), `"checks"`) {
		t.Errorf("snapshot: %v", err)
	}
	if !strings.Contains(telemetryText(t, home), `"name":"maintain"`) {
		t.Error("no job event")
	}
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":`+strconv.Itoa(testtool.OtherPID())+`}`), 0o600)
	if _, out, _ := run(t, "jobs", "maintain"); !strings.Contains(out, "skipped: maintenance") {
		t.Errorf("%s", out)
	}
}

// Curation waits for the maintenance lock; SessionStart makes no server
// call while the breaker is open.
func TestCurationHonoursTheMaintenanceLock(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	_ = os.MkdirAll(filepath.Dir(d.MaintenanceLock()), 0o700)
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"command":"backup create"}`), 0o600)
	for _, args := range [][]string{{"forget", "f1"}, {"memory", "scan"}, {"ask", "why?"}, {"projects"}} {
		if code, _, errOut := run(t, args...); code != output.Blocked || !strings.Contains(errOut, "maintenance in progress") {
			t.Errorf("%v: exit %d %q", args, code, errOut)
		}
	}
	if len(curation) != 0 {
		t.Errorf("calls %q", curation)
	}
}

func TestSessionStartSkipsTheServerWhileTheBreakerIsOpen(t *testing.T) {
	home := hookHome(t)
	_ = os.MkdirAll(pathsOf(home).State, 0o700)
	_ = os.WriteFile(filepath.Join(pathsOf(home).State, "breaker.json"), []byte(`{"open_until":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`), 0o600)
	recentAnswer["observation"] = "[" + obs("o1", "Billing keeps PostgreSQL 16.", "2026-10-24T09:00:00Z") + "]"
	h := fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if strings.Contains(h.Specific.Context, "PostgreSQL") || !strings.Contains(h.SystemMessage, "server failing") || len(mentalCalls) != 0 {
		t.Errorf("%q %q", h.Specific.Context, h.SystemMessage)
	}
}

func TestJobsInstallNeedsTheShim(t *testing.T) {
	home := devInstall(t, true)
	withSys(t, "linux")
	_ = os.Remove(filepath.Join(pathsOf(home).Data, "bin", "current"))
	if code, _, errOut := run(t, "jobs", "install"); code != output.NeedsUser || !strings.Contains(errOut, "client install") {
		t.Errorf("exit %d %q", code, errOut)
	}
}

// Uninstall removes the files where they were written, keeps them when the
// entries could not be deactivated (no user session), and accepts entries
// already gone.
func TestJobsUninstallFailurePaths(t *testing.T) {
	testtool.SkipWindows(t, "Task 12: scheduler entries are systemd units")
	home := devInstall(t, true)
	sys := withSys(t, "linux")
	run(t, "jobs", "install")
	timer := filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")
	sys.xdg = filepath.Join(home, "elsewhere") // the manager's directory changed since
	sys.fail, sys.output = "systemctl --user disable", "Failed to connect to bus: No medium found"
	if code, _, errOut := run(t, "jobs", "uninstall"); code != output.Blocked || !strings.Contains(errOut, "bus") {
		t.Errorf("no user session: exit %d %q", code, errOut)
	}
	if _, err := os.Stat(timer); err != nil {
		t.Error("files removed while the entries may still be loaded")
	}
	sys.output = "Unit backsight-tick.timer does not exist."
	if code, _, _ := run(t, "jobs", "uninstall"); code != output.OK {
		t.Errorf("entries already gone: exit %d", code)
	}
	if _, err := os.Stat(timer); err == nil {
		t.Error("the recorded file was not removed")
	}
}

// A project renamed since the install keeps its entries' names: uninstall
// deactivates the units recorded in state.json, not the new name's.
func TestJobsUninstallOfARenamedProject(t *testing.T) {
	home := devInstall(t, true)
	sys := withSys(t, "linux")
	run(t, "jobs", "install")
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, []byte(strings.Replace(string(raw), "[server]\n", "[server]\nproject = \"renamed\"\n", 1)), 0o600)
	sys.calls = nil
	code, out, _ := run(t, "jobs", "uninstall", "--plan")
	if code != output.OK || !strings.Contains(out, "systemctl --user disable --now backsight-tick.timer backsight-maintain.timer") || strings.Contains(out, "renamed-") {
		t.Errorf("plan: exit %d\n%s", code, out)
	}
	if code, _, _ := run(t, "jobs", "uninstall"); code != output.OK {
		t.Fatalf("uninstall: exit %d", code)
	}
	if len(sys.calls) == 0 || sys.calls[0] != "systemctl --user disable --now backsight-tick.timer backsight-maintain.timer" {
		t.Errorf("calls %q", sys.calls)
	}
	for _, c := range sys.calls {
		if strings.Contains(c, "renamed-") {
			t.Errorf("deactivated the new name's units: %s", c)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")); err == nil {
		t.Error("the recorded files were not removed")
	}
}

// A reinstall keeps the first install time (doctor's grace period).
func TestJobsReinstallKeepsTheInstallTime(t *testing.T) {
	home := devInstall(t, true)
	withSys(t, "linux")
	withClock(t, time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	run(t, "jobs", "install")
	withClock(t, time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC))
	run(t, "jobs", "install")
	if st, _ := state.Load(pathsOf(home).StateFile()); st.Scheduler == nil || st.Scheduler.InstalledAt.Day() != 20 {
		t.Errorf("%+v", st.Scheduler)
	}
}

// The launchd bootout of an agent not loaded yet fails, and is expected.
func TestJobsInstallOnMacOS(t *testing.T) {
	home := devInstall(t, true)
	sys := withSys(t, "darwin")
	sys.fail = "launchctl bootout"
	if code, _, errOut := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("exit %d %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "tech.backsight.tick.plist")); err != nil {
		t.Error(err)
	}
}

// A second install's entries are its own: install and uninstall touch only
// the project's units, never the first install's backsight-*.
func TestJobsOfASecondInstallKeepToTheirProject(t *testing.T) {
	home := devInstall(t, true)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, []byte(strings.Replace(string(raw), "[server]\n", "[server]\nproject = \"bs-fresh\"\n", 1)), 0o600)
	sys := withSys(t, "linux")
	first := filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")
	_ = os.MkdirAll(filepath.Dir(first), 0o700)
	_ = os.WriteFile(first, []byte("the first install's\n"), 0o644)
	if code, out, _ := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, "xdg", "systemd", "user", "bs-fresh-tick.timer")); err != nil {
		t.Error("the project's timer was not written")
	}
	sys.calls = nil
	if code, _, _ := run(t, "jobs", "uninstall"); code != output.OK {
		t.Fatal("uninstall failed")
	}
	for _, c := range sys.calls {
		if strings.Contains(c, "backsight-") {
			t.Errorf("touched the first install's units: %s", c)
		}
	}
	if readFile(t, first) != "the first install's\n" {
		t.Error("the first install's timer was changed")
	}
}

// Without a systemd user session the activation fails: install says
// how to go on (exit 10: a login session, or setup's scheduler=off), and
// uninstall then removes the entries it left, since none was enabled.
func TestJobsWithoutAUserSession(t *testing.T) {
	home := devInstall(t, true)
	sys := withSys(t, "linux")
	sys.fail, sys.output = "systemctl --user daemon-reload", busError
	code, errOut, _ := run(t, "jobs", "install", "--json")
	timer := filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")
	if code != output.NeedsUser || !strings.Contains(errOut, "scheduler=off") || !strings.Contains(errOut, "login session") {
		t.Fatalf("install: exit %d %s", code, errOut)
	}
	if _, err := os.Stat(timer); err != nil {
		t.Fatal("the failed install left no files: nothing to test")
	}
	sys.fail, sys.noBus = "", true
	if code, _, errOut := run(t, "jobs", "uninstall"); code != output.OK {
		t.Fatalf("uninstall: exit %d %s", code, errOut)
	}
	if _, err := os.Stat(timer); err == nil {
		t.Error("the entries the failed install wrote stay on disk")
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.Scheduler != nil {
		t.Errorf("the record stays: %+v", st.Scheduler)
	}

	// The reload passes and the enable finds no session: the same.
	sys.noBus, sys.fail, sys.output = false, "systemctl --user enable", busError
	if code, out, _ := run(t, "jobs", "install", "--json"); code != output.NeedsUser || !strings.Contains(out, "scheduler=off") {
		t.Fatalf("enable without a session: exit %d %s", code, out)
	}
	sys.fail, sys.noBus = "", true
	if code, _, errOut := run(t, "jobs", "uninstall"); code != output.OK {
		t.Fatalf("uninstall after a failed enable: exit %d %s", code, errOut)
	}
	if _, err := os.Stat(timer); err == nil {
		t.Error("the entries a failed enable wrote stay on disk")
	}
}

// Without a session, a standalone install says to run it again from a
// login session or remove what it wrote; after a setup run, the resume
// without the scheduler.
func TestJobsInstallWithoutASessionNamesTheWayOn(t *testing.T) {
	home := devInstall(t, true)
	sys := withSys(t, "linux")
	sys.fail, sys.output = "systemctl --user daemon-reload", busError
	cmds := func() []string {
		t.Helper()
		_, out, _ := run(t, "jobs", "install", "--json")
		var env struct{ Error *output.Error }
		if err := json.Unmarshal([]byte(out), &env); err != nil || env.Error == nil {
			t.Fatalf("%v\n%s", err, out)
		}
		var c []string
		for _, a := range env.Error.Actions {
			c = append(c, a.Cmd)
		}
		return c
	}
	if got := strings.Join(cmds(), ","); got != "backsight jobs install,backsight jobs uninstall" {
		t.Errorf("standalone: %q", got)
	}
	r := setup.Record{SchemaVersion: setup.RecordVersion, Run: "r1", Answers: map[string]string{"scheduler": "on"}}
	if err := r.Save(pathsOf(home).SetupFile()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmds(), ","); got != "backsight setup --resume --answer scheduler=off" {
		t.Errorf("after setup: %q", got)
	}
}

// A failure other than the missing session keeps the entries, though no
// timer was enabled; the missing session alone does not.
func TestStillLoadedKeepsOtherFailures(t *testing.T) {
	testtool.SkipWindows(t, "Task 12: scheduler entries are systemd units")
	files := []string{filepath.Join(t.TempDir(), "bs-tick.timer")}
	bus := deactivation{cmd: []string{"systemctl", "--user", "disable", "--now", "bs-tick.timer"}, out: busError, err: errors.New("exit status 1")}
	other := deactivation{cmd: []string{"systemctl", "--user", "stop", "bs-tick.service"}, out: "Access denied", err: errors.New("exit status 4")}
	gone := deactivation{cmd: []string{"systemctl", "--user", "stop", "x"}, out: "Unit x does not exist.", err: errors.New("exit status 5")}
	if failed, noBus := stillLoaded([]deactivation{bus, gone}, "systemd", files); len(failed) != 0 || noBus {
		t.Errorf("never enabled, no session: %q %v", failed, noBus)
	}
	failed, _ := stillLoaded([]deactivation{bus, other}, "systemd", files)
	if len(failed) != 1 || !strings.Contains(failed[0], "Access denied") {
		t.Errorf("mixed: %q", failed)
	}
	wants := filepath.Join(filepath.Dir(files[0]), "timers.target.wants")
	_ = os.MkdirAll(wants, 0o700)
	_ = os.Symlink("../bs-tick.timer", filepath.Join(wants, "bs-tick.timer"))
	if failed, noBus := stillLoaded([]deactivation{bus, other}, "systemd", files); len(failed) != 2 || !noBus {
		t.Errorf("enabled, mixed: %q %v", failed, noBus)
	}
}

// An enabled timer may run in a manager this shell cannot reach (su):
// uninstall keeps its files and says where to run it.
func TestJobsUninstallKeepsEnabledEntriesWithoutAUserSession(t *testing.T) {
	testtool.SkipWindows(t, "Task 12: scheduler entries are systemd units")
	home := devInstall(t, true)
	sys := withSys(t, "linux")
	if code, _, _ := run(t, "jobs", "install"); code != output.OK {
		t.Fatal("install failed")
	}
	sys.noBus = true
	code, _, errOut := run(t, "jobs", "uninstall")
	if code != output.Blocked || !strings.Contains(errOut, "login session") {
		t.Errorf("exit %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")); err != nil {
		t.Error("removed the files of enabled timers")
	}
}

func TestJobsStartRunsTheJobThroughTheScheduler(t *testing.T) {
	devInstall(t, true)
	sys := withSys(t, "linux")
	if code, out, errOut := run(t, "jobs", "start", "tick"); code != output.OK || !strings.Contains(out, "jobs tick started") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if last := sys.calls[len(sys.calls)-1]; last != "systemctl --user start --no-block backsight-tick.service" {
		t.Errorf("calls %q", sys.calls)
	}
	if code, _, _ := run(t, "jobs", "start", "nightly"); code != output.Usage {
		t.Errorf("an unknown job: exit %d", code)
	}
	sys.fail = "systemctl --user start"
	if code, _, errOut := run(t, "jobs", "start", "tick"); code != output.Blocked || !strings.Contains(errOut, "start --no-block") {
		t.Errorf("a failing start: exit %d %q", code, errOut)
	}
}
