package doctor

import (
	"fmt"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/scheduler"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/stats"
)

// checkJobs: each scheduled job ran within twice its cadence (SPEC §15),
// read from the job events of the telemetry. Without scheduler entries
// there is nothing to judge.
func checkJobs(d paths.Dirs, now time.Time) Check {
	c := Check{ID: "jobs", Status: OK}
	st, err := state.Load(d.StateFile())
	if err != nil || st.Scheduler == nil {
		c.Status, c.Summary = Info, "no scheduler entries: sessions are sent when they end"
		c.Fix = "backsight jobs install (setup does it)"
		return c
	}
	ev, err := stats.Load(d.TelemetryDir(), now.Add(-72*time.Hour), now.Add(time.Minute))
	if err != nil {
		c.Status, c.Summary = Warn, "telemetry unreadable: "+err.Error()
		return c
	}
	last := map[string]time.Time{}
	for _, e := range ev {
		if e.Event == "job" && e.TS.After(last[e.Name]) {
			last[e.Name] = e.TS
		}
	}
	var late []string
	for _, j := range scheduler.Jobs {
		limit := 2 * time.Duration(j.Minutes) * time.Minute
		if now.Sub(st.Scheduler.InstalledAt) < limit {
			continue // just installed: not due yet
		}
		if t, ok := last[j.Name]; !ok || now.Sub(t) > limit {
			when := "never in 3 days"
			if ok {
				when = now.Sub(t).Round(time.Minute).String() + " ago"
			}
			late = append(late, fmt.Sprintf("%s last ran %s (every %s)", j.Name, when, time.Duration(j.Minutes)*time.Minute))
		}
	}
	if len(late) > 0 {
		c.Status, c.Summary = Warn, fmt.Sprintf("scheduled job late: %v", late)
		c.Fix = map[string]string{"systemd": "systemctl --user list-timers 'backsight-*'", "launchd": "launchctl list | grep backsight",
			"schtasks": `schtasks /Query /TN backsight\tick`}[st.Scheduler.Kind]
		return c
	}
	c.Summary = fmt.Sprintf("%s entries installed; jobs on time", st.Scheduler.Kind)
	return c
}
