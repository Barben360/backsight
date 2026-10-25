package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/state"
)

// oldTemplate rewrites an installed file as an older binary rendered it:
// without the lines the current template added.
func oldTemplate(t *testing.T, path string, drop ...string) {
	t.Helper()
	raw := readFile(t, path)
	for _, d := range drop {
		if !strings.Contains(raw, d) {
			t.Fatalf("%s has no %q:\n%s", path, d, raw)
		}
		raw = strings.Replace(raw, d, "", 1)
	}
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

type refreshEnvelope struct {
	Data jobsRefreshResult `json:"data"`
}

func decodeRefresh(t *testing.T, out string) jobsRefreshResult {
	t.Helper()
	var e refreshEnvelope
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return e.Data
}

// A refresh rewrites only the units whose template changed, keeps the
// environment they were installed with, and reloads them without stopping
// a running job: daemon-reload, never a stop or restart.
func TestJobsRefreshRewritesStaleUnits(t *testing.T) {
	home := devInstall(t, true)
	sys := withSys(t, "linux")
	if code, out, _ := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	dir := filepath.Join(home, "xdg", "systemd", "user")
	svc, timer := filepath.Join(dir, "backsight-tick.service"), filepath.Join(dir, "backsight-tick.timer")
	installedPath := os.Getenv("PATH")
	oldTemplate(t, svc, "KillMode=mixed\nTimeoutStopSec=45min\n")
	timerBefore, maintainBefore := readFile(t, timer), readFile(t, filepath.Join(dir, "backsight-maintain.service"))
	t.Setenv("PATH", installedPath+":/elsewhere/bin") // the refreshing shell is not the installing one

	sys.calls = nil
	code, out, _ := run(t, "jobs", "refresh", "--plan", "--json")
	r := decodeRefresh(t, out)
	if code != output.OK || r.Status != "planned" || len(r.Files) != 1 || r.Files[0].Target != svc || len(sys.calls) != 0 {
		t.Fatalf("plan: exit %d, calls %q\n%s", code, sys.calls, out)
	}
	if strings.Contains(readFile(t, svc), "KillMode") {
		t.Fatal("the plan wrote the unit")
	}
	if _, out, _ := run(t, "jobs", "refresh", "--plan"); !strings.Contains(out, "would write") || !strings.Contains(out, "would run") {
		t.Errorf("plan text:\n%s", out)
	}

	code, out, _ = run(t, "jobs", "refresh", "--json")
	r = decodeRefresh(t, out)
	got := readFile(t, svc)
	if code != output.OK || r.Status != "done" || !strings.Contains(got, "KillMode=mixed\nTimeoutStopSec=45min\n") {
		t.Fatalf("refresh: exit %d\n%s\n%s", code, out, got)
	}
	if !strings.Contains(got, "Environment=PATH="+installedPath+"\n") || strings.Contains(got, "/elsewhere/bin") {
		t.Errorf("the installed environment was not kept:\n%s", got)
	}
	if readFile(t, timer) != timerBefore || readFile(t, filepath.Join(dir, "backsight-maintain.service")) != maintainBefore {
		t.Error("an up-to-date file was rewritten")
	}
	want := []string{"systemctl --user daemon-reload", "systemctl --user enable --now backsight-tick.timer backsight-maintain.timer"}
	if strings.Join(sys.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls %q, want %q", sys.calls, want)
	}
	for _, c := range sys.calls {
		if strings.Contains(c, "restart") || strings.Contains(c, "stop") || strings.Contains(c, ".service") {
			t.Errorf("a refresh must never stop or restart a service: %s", c)
		}
	}
	if r.Run == "" {
		t.Error("the rewrite went through the config editor without a run (no undo)")
	}

	sys.calls = nil
	code, out, _ = run(t, "jobs", "refresh", "--json")
	if r := decodeRefresh(t, out); code != output.OK || r.Status != "unchanged" || len(sys.calls) != 0 {
		t.Errorf("second refresh: exit %d, calls %q\n%s", code, sys.calls, out)
	}
}

// Without entries there is nothing to refresh, and no scheduler is asked.
func TestJobsRefreshWithoutEntries(t *testing.T) {
	devInstall(t, true)
	sys := withSys(t, "linux")
	code, out, _ := run(t, "jobs", "refresh", "--json")
	if r := decodeRefresh(t, out); code != output.OK || r.Status != "none" || len(sys.calls) != 0 {
		t.Errorf("exit %d, calls %q\n%s", code, sys.calls, out)
	}
}

// launchd only reads a plist on bootstrap, after a bootout that would stop
// a running job: that job's agent is left as it is (and reported), the
// others are refreshed.
func TestJobsRefreshLeavesARunningLaunchdJob(t *testing.T) {
	home := devInstall(t, true)
	sys := withSys(t, "darwin")
	sys.fail = "launchctl bootout"
	if code, _, errOut := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("install: exit %d %q", code, errOut)
	}
	sys.fail = ""
	agents := filepath.Join(home, "Library", "LaunchAgents")
	tick, maintain := filepath.Join(agents, "tech.backsight.tick.plist"), filepath.Join(agents, "tech.backsight.maintain.plist")
	stop := "<key>ExitTimeOut</key><integer>2700</integer>\n"
	oldTemplate(t, tick, stop)
	oldTemplate(t, maintain, stop)
	maintainBefore := readFile(t, maintain)
	uid := os.Getuid()
	sys.out = map[string]string{fmt.Sprintf("launchctl print gui/%d/tech.backsight.maintain", uid): "gui/501/tech.backsight.maintain = {\n\tstate = running\n\tpid = 4242\n}\n"}

	sys.calls = nil
	code, out, _ := run(t, "jobs", "refresh", "--json")
	r := decodeRefresh(t, out)
	if code != output.OK || r.Status != "done" || strings.Join(r.Deferred, ",") != "maintain" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(readFile(t, tick), stop) || readFile(t, maintain) != maintainBefore {
		t.Errorf("tick refreshed %v, maintain untouched %v", strings.Contains(readFile(t, tick), stop), readFile(t, maintain) == maintainBefore)
	}
	for _, c := range sys.calls {
		if strings.Contains(c, "tech.backsight.maintain") && !strings.HasPrefix(c, "launchctl print") {
			t.Errorf("the running job was touched: %s", c)
		}
	}
	if !strings.Contains(strings.Join(sys.calls, "|"), fmt.Sprintf("launchctl bootstrap gui/%d %s", uid, tick)) {
		t.Errorf("tick not reloaded: %q", sys.calls)
	}
	if _, out, _ := run(t, "jobs", "refresh"); !strings.Contains(out, "maintain") || !strings.Contains(out, "running") {
		t.Errorf("text names the job left for later:\n%s", out)
	}

	// Once it no longer runs, the next refresh takes it.
	sys.out, sys.calls = nil, nil
	code, out, _ = run(t, "jobs", "refresh", "--json")
	if r := decodeRefresh(t, out); code != output.OK || len(r.Deferred) != 0 || !strings.Contains(readFile(t, maintain), stop) {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// Tasks have no files: the install records its commands' digest, and a
// refresh reruns /Create for tasks whose digest is not this binary's.
func TestJobsRefreshScheduledTasks(t *testing.T) {
	home := devInstall(t, true)
	sys := withSys(t, "windows")
	d := pathsOf(home)
	_ = os.WriteFile(filepath.Join(d.Data, "bin", plugin.ShimName("windows")), []byte("MZ"), 0o700)
	if code, _, errOut := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("install: exit %d %q", code, errOut)
	}
	if st, _ := state.Load(d.StateFile()); st.Scheduler == nil || st.Scheduler.Digest == "" {
		t.Fatalf("no digest recorded: %+v", st.Scheduler)
	}
	sys.calls = nil
	if code, out, _ := run(t, "jobs", "refresh", "--json"); decodeRefresh(t, out).Status != "unchanged" || code != output.OK {
		t.Errorf("fresh install: exit %d\n%s", code, out)
	}
	// An install by a binary that recorded no digest (or another one).
	_ = state.Update(d, "test", func(s *state.State) error { s.Scheduler.Digest = ""; return nil })
	sys.calls = nil
	code, out, _ := run(t, "jobs", "refresh", "--json")
	creates := 0
	for _, c := range sys.calls {
		if strings.HasPrefix(c, "schtasks /Create /F") {
			creates++
		}
	}
	if r := decodeRefresh(t, out); code != output.OK || r.Status != "done" || creates != 2 {
		t.Errorf("exit %d, calls %q\n%s", code, sys.calls, out)
	}
	if st, _ := state.Load(d.StateFile()); st.Scheduler.Digest == "" {
		t.Error("the refresh did not record the digest")
	}
}

// Entries installed under other paths (another project name, a moved
// unit directory) are not this binary's to rewrite in place: refused,
// naming the reinstall.
func TestJobsRefreshRefusesEntriesInstalledElsewhere(t *testing.T) {
	home := devInstall(t, true)
	sys := withSys(t, "linux")
	run(t, "jobs", "install")
	sys.xdg = filepath.Join(home, "elsewhere")
	sys.calls = nil
	code, _, errOut := run(t, "jobs", "refresh")
	if code != output.Blocked || !strings.Contains(errOut, "jobs uninstall") || !strings.Contains(errOut, "jobs install") || len(sys.calls) != 0 {
		t.Errorf("exit %d, calls %q: %s", code, sys.calls, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "elsewhere", "systemd", "user", "backsight-tick.timer")); err == nil {
		t.Error("a second set of units was written")
	}
}

// doctor's probe sees the same stale entries a refresh would rewrite.
func TestDoctorProbeComparesTheSchedulerEntries(t *testing.T) {
	home := devInstall(t, true)
	withSys(t, "linux")
	e := doctor.Env{Dirs: pathsOf(home)}
	realProbes(&e)
	if s, err := e.Scheduler(); err != nil || s.Installed {
		t.Errorf("nothing installed: %+v %v", s, err)
	}
	run(t, "jobs", "install")
	if s, err := e.Scheduler(); err != nil || !s.Installed || len(s.Stale) != 0 || s.Elsewhere != "" {
		t.Errorf("fresh: %+v %v", s, err)
	}
	oldTemplate(t, filepath.Join(home, "xdg", "systemd", "user", "backsight-maintain.service"), "KillMode=mixed\n")
	if s, err := e.Scheduler(); err != nil || strings.Join(s.Stale, ",") != "maintain" {
		t.Errorf("stale: %+v %v", s, err)
	}
	_ = os.WriteFile(pathsOf(home).StateFile(), []byte("{not json"), 0o600)
	if _, err := e.Scheduler(); err == nil {
		t.Error("an unreadable state.json compares nothing")
	}
}
