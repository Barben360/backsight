package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

type fakeSys struct {
	calls []string
	fail  string // a command line prefix that fails
}

func (f *fakeSys) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if f.fail != "" && strings.HasPrefix(line, f.fail) {
		return []byte("unit failed"), errors.New("exit status 1")
	}
	return nil, nil
}

func withSys(t *testing.T, goos string) *fakeSys {
	t.Helper()
	f := &fakeSys{}
	oldR, oldG := sysRunner, schedulerGOOS
	sysRunner, schedulerGOOS = f, goos
	t.Cleanup(func() { sysRunner, schedulerGOOS = oldR, oldG })
	return f
}

func TestJobsInstallPlanThenInstallThenUninstall(t *testing.T) {
	home := devInstall(t, true)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	sys := withSys(t, "linux")
	timer := filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")
	code, out, _ := run(t, "jobs", "install", "--plan")
	if _, err := os.Stat(timer); code != output.OK || err == nil || len(sys.calls) != 0 || !strings.Contains(out, "would write") || !strings.Contains(out, "would run") {
		t.Fatalf("plan: exit %d, calls %q\n%s", code, sys.calls, out)
	}
	if code, out, _ := run(t, "jobs", "install"); code != output.OK || !strings.Contains(readFile(t, timer), "OnCalendar=*:0/15") {
		t.Fatalf("install: exit %d\n%s", code, out)
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
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	sys := withSys(t, "linux")
	sys.fail = "systemctl --user enable"
	code, _, errOut := run(t, "jobs", "install")
	if st, _ := state.Load(pathsOf(home).StateFile()); code != output.Blocked || !strings.Contains(errOut, "unit failed") || st.Scheduler != nil {
		t.Errorf("exit %d %q, state %+v", code, errOut, st.Scheduler)
	}
}

// jobs maintain rotates telemetry past 13 months, writes a doctor
// snapshot and logs its run; it skips during maintenance.
func TestJobsMaintain(t *testing.T) {
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
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":1}`), 0o600)
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
