package doctor

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/machines"
	"github.com/Barben360/backsight/internal/paths"
)

func TestMachinesShareCheck(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	record := func(t *testing.T, p machines.Published) paths.Dirs {
		t.Helper()
		d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
		if err := machines.SavePublished(d.MetricsDir(), p); err != nil {
			t.Fatal(err)
		}
		return d
	}
	cases := []struct {
		name     string
		enabled  bool
		share    bool
		rec      *machines.Published
		status   Status
		summary  string
		fix      string
		mentions string
	}{
		{name: "feedback off", enabled: false, share: true, status: OK, summary: "not shared"},
		{name: "sharing off", enabled: true, share: false, status: OK, summary: "not shared"},
		{name: "published a day ago", enabled: true, share: true, rec: &machines.Published{At: now.Add(-24 * time.Hour)}, status: OK, summary: "published"},
		{name: "failing 25 h", enabled: true, share: true,
			rec:    &machines.Published{At: now.Add(-49 * time.Hour), FailedAt: now.Add(-time.Hour), FailedSince: now.Add(-25 * time.Hour), Failure: "server"},
			status: Warn, summary: "25 h", fix: "backsight jobs tick"},
		{name: "index never caught up, 25 h", enabled: true, share: true,
			rec:    &machines.Published{FailedSince: now.Add(-25 * time.Hour), Failure: "budget"},
			status: Warn, summary: "activity index", fix: "backsight stats index rebuild"},
		{name: "failing 23 h", enabled: true, share: true,
			rec:    &machines.Published{At: now.Add(-47 * time.Hour), FailedAt: now.Add(-time.Hour), FailedSince: now.Add(-23 * time.Hour), Failure: "server"},
			status: OK, summary: "published"},
		{name: "never published, no failure", enabled: true, share: true, status: OK, summary: "not published yet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
			if c.rec != nil {
				d = record(t, *c.rec)
			}
			got := checkMachinesShare(d, c.enabled, c.share, now)
			if got.ID != "machines-share" || got.Status != c.status || !strings.Contains(got.Summary, c.summary) || got.Fix != c.fix {
				t.Errorf("%+v", got)
			}
		})
	}
}

// Run reports the check; it is a client check, so a server-only host reads
// it as not a client (TestServerOnlyHostSkipsTheClientChecks).
func TestRunIncludesMachinesShare(t *testing.T) {
	f := newFixture(t)
	expect(t, check(t, f.run(), "machines-share"), OK, "not published yet")
	found := false
	for _, id := range ClientChecks {
		found = found || id == "machines-share"
	}
	if !found {
		t.Error("machines-share is not a client check")
	}
}
