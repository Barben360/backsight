package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
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
