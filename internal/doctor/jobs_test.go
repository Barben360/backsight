package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

func jobsDirs(t *testing.T, installedAgo time.Duration, runs map[string]time.Duration) paths.Dirs {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	if installedAgo >= 0 {
		_ = state.Update(d, "test", func(s *state.State) error {
			s.Scheduler = &state.Scheduler{Kind: "systemd", InstalledAt: usageNow.Add(-installedAgo)}
			return nil
		})
	}
	_ = os.MkdirAll(d.TelemetryDir(), 0o700)
	var b strings.Builder
	for name, ago := range runs {
		fmt.Fprintf(&b, `{"ts":%q,"event":"job","name":%q,"status":"done"}`+"\n", usageNow.Add(-ago).Format(time.RFC3339), name)
	}
	_ = os.WriteFile(filepath.Join(d.TelemetryDir(), "events-2026-10.jsonl"), []byte(b.String()), 0o600)
	return d
}

func TestJobsCadence(t *testing.T) {
	cases := []struct {
		name      string
		installed time.Duration // -1: no scheduler entries
		runs      map[string]time.Duration
		want      Status
	}{
		{"no scheduler entries", -1, nil, Info},
		{"just installed, nothing ran yet", 10 * time.Minute, nil, OK},
		{"both on time", 72 * time.Hour, map[string]time.Duration{"tick": 10 * time.Minute, "maintain": 20 * time.Hour}, OK},
		{"tick late (over 30 min)", 72 * time.Hour, map[string]time.Duration{"tick": 45 * time.Minute, "maintain": 20 * time.Hour}, Warn},
		{"maintain late (over 48 h)", 72 * time.Hour, map[string]time.Duration{"tick": 10 * time.Minute, "maintain": 50 * time.Hour}, Warn},
		{"never ran", 72 * time.Hour, nil, Warn},
	}
	for _, c := range cases {
		got := checkJobs(jobsDirs(t, c.installed, c.runs), usageNow, nil)
		if got.Status != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got.Status, got.Summary, c.want)
		}
	}
}

func TestJobsLingerAndUnreadableState(t *testing.T) {
	d := jobsDirs(t, 72*time.Hour, map[string]time.Duration{"tick": 10 * time.Minute, "maintain": 20 * time.Hour})
	if c := checkJobs(d, usageNow, func() (bool, error) { return false, nil }); c.Status != Info || !strings.Contains(c.Detail, "logged out") {
		t.Errorf("no linger: %+v", c)
	}
	if c := checkJobs(d, usageNow, func() (bool, error) { return true, nil }); c.Status != OK {
		t.Errorf("linger: %+v", c)
	}
	_ = os.WriteFile(d.StateFile(), []byte("{not json"), 0o600)
	if c := checkJobs(d, usageNow, nil); c.Status != Warn {
		t.Errorf("unreadable state: %+v", c)
	}
}

// Entries rendered by an older template are a warning naming the refresh;
// entries installed under other paths name the reinstall; a comparison
// that could not run is information.
func TestSchedulerEntriesCheck(t *testing.T) {
	probe := func(s SchedulerEntries, err error) func() (SchedulerEntries, error) {
		return func() (SchedulerEntries, error) { return s, err }
	}
	if _, ok := checkScheduler(probe(SchedulerEntries{}, nil), schedulerOn); ok {
		t.Error("no entries installed: no line (the jobs check says it)")
	}
	c, ok := checkScheduler(probe(SchedulerEntries{Installed: true, Kind: "systemd"}, nil), schedulerOn)
	if !ok || c.ID != "scheduler" || c.Status != OK || c.Fix != "" {
		t.Errorf("fresh: %+v", c)
	}
	c, _ = checkScheduler(probe(SchedulerEntries{Installed: true, Kind: "systemd", Stale: []string{"tick", "maintain"}}, nil), schedulerOn)
	if c.Status != Warn || !strings.Contains(c.Summary, "tick, maintain") || c.Fix != "backsight jobs refresh" {
		t.Errorf("stale: %+v", c)
	}
	c, _ = checkScheduler(probe(SchedulerEntries{Installed: true, Kind: "systemd", Elsewhere: "the entries are /a; this install's would be /b"}, nil), schedulerOn)
	if c.Status != Warn || !strings.Contains(c.Detail, "/a") || !strings.Contains(c.Fix, "jobs uninstall") || !strings.Contains(c.Fix, "jobs install") {
		t.Errorf("elsewhere: %+v", c)
	}
	c, ok = checkScheduler(probe(SchedulerEntries{}, fmt.Errorf("systemctl: no user bus")), schedulerOn)
	if !ok || c.Status != Info || !strings.Contains(c.Detail, "no user bus") {
		t.Errorf("probe failed: %+v", c)
	}
}

func TestRunReportsStaleSchedulerEntries(t *testing.T) {
	f := newFixture(t)
	f.env.Scheduler = func() (SchedulerEntries, error) {
		return SchedulerEntries{Installed: true, Kind: "launchd", Stale: []string{"maintain"}}, nil
	}
	r := f.run()
	if c := check(t, r, "scheduler"); c.Status != Warn || !strings.Contains(c.Summary, "maintain") {
		t.Errorf("%+v", c)
	}
	f.env.Scheduler = nil
	for _, c := range f.run().Checks {
		if c.ID == "scheduler" {
			t.Errorf("no probe, no line: %+v", c)
		}
	}
}

// saveAnsweredOff saves a setup run answered scheduler=off.
func saveAnsweredOff(t *testing.T, d paths.Dirs) {
	t.Helper()
	r := setup.Record{SchemaVersion: setup.RecordVersion, Run: "r1", Answers: map[string]string{"scheduler": "off"}}
	if err := r.Save(d.SetupFile()); err != nil {
		t.Fatal(err)
	}
}

// leftoverEntries records systemd entries whose files are on disk, the
// timers linked (enabled) or not.
func leftoverEntries(t *testing.T, d paths.Dirs, enabled bool) {
	t.Helper()
	dir := filepath.Join(d.Config, "systemd", "user")
	var files []string
	for _, n := range []string{"bs-tick.service", "bs-tick.timer", "bs-maintain.service", "bs-maintain.timer"} {
		files = append(files, filepath.Join(dir, n))
		put(t, filepath.Join(dir, n), "x\n", 0o644)
		if enabled && strings.HasSuffix(n, ".timer") {
			_ = os.MkdirAll(filepath.Join(dir, "timers.target.wants"), 0o700)
			_ = os.Symlink("../"+n, filepath.Join(dir, "timers.target.wants", n))
		}
	}
	_ = state.Update(d, "test", func(s *state.State) error {
		s.Scheduler = &state.Scheduler{Kind: "systemd", Files: files, InstalledAt: usageNow.Add(-10 * time.Minute)}
		return nil
	})
}

// scheduler=off is reported as off, with the commands to run by hand;
// entries on disk whose timers were never enabled run nothing and warn,
// with the fix that applies (removal when the answer was off).
func TestJobsWhenTheSchedulerIsOff(t *testing.T) {
	d := jobsDirs(t, -1, nil)
	saveAnsweredOff(t, d)
	c := checkJobs(d, usageNow, nil)
	if c.Status != Info || !strings.Contains(c.Summary, "scheduler=off") || !strings.Contains(c.Fix, "jobs tick") || !strings.Contains(c.Fix, "jobs maintain") {
		t.Errorf("off: %+v", c)
	}

	leftoverEntries(t, d, false)
	c = checkJobs(d, usageNow, nil)
	if c.Status != Warn || !strings.Contains(c.Summary, "not enabled") || c.Fix != "backsight jobs uninstall" {
		t.Errorf("off with leftover entries: %+v", c)
	}

	d = jobsDirs(t, -1, nil)
	leftoverEntries(t, d, false)
	if c := checkJobs(d, usageNow, nil); c.Status != Warn || !strings.Contains(c.Fix, "login session") || !strings.Contains(c.Fix, "scheduler=off") {
		t.Errorf("never enabled: %+v", c)
	}
	d = jobsDirs(t, -1, nil)
	leftoverEntries(t, d, true)
	if c := checkJobs(d, usageNow, nil); c.Status != OK {
		t.Errorf("enabled: %+v", c)
	}
}

// The scheduler line says off (information) instead of nothing, and
// never calls entries that run nothing a match.
func TestSchedulerCheckWhenOff(t *testing.T) {
	none := func() (SchedulerEntries, error) { return SchedulerEntries{}, nil }
	c, ok := checkScheduler(none, schedulerOff)
	if !ok || c.Status != Info || !strings.Contains(c.Summary, "off") {
		t.Errorf("off: %v %+v", ok, c)
	}
	installed := func() (SchedulerEntries, error) { return SchedulerEntries{Installed: true, Kind: "systemd"}, nil }
	c, ok = checkScheduler(installed, schedulerIdle)
	if !ok || c.Status != Info || strings.Contains(c.Summary, "match") {
		t.Errorf("not enabled: %+v", c)
	}
}

// The backup fix names the maintain job's own log, and never a journal
// when the scheduler is off.
func TestBackupFixFollowsTheScheduler(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \"http://127.0.0.1:1\"\n[backup]\ndir = \""+filepath.Join(f.home, "bk")+"\"\n", 0o600)
	saveAnsweredOff(t, f.env.Dirs)
	if c := checkBackup(f.env, now); c.Status != Warn || strings.Contains(c.Fix, "journalctl") || !strings.Contains(c.Fix, "jobs maintain") {
		t.Errorf("off: %+v", c)
	}
	leftoverEntries(t, f.env.Dirs, true)
	if c := checkBackup(f.env, now); !strings.Contains(c.Fix, "journalctl --user -u bs-maintain") {
		t.Errorf("systemd: %+v", c)
	}
	r := f.run()
	if c := check(t, r, "jobs"); c.Status == Info && strings.Contains(c.Summary, "scheduler=off") {
		t.Errorf("entries installed since are not off: %+v", c)
	}
}
