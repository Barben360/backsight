package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/scheduler"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/stats"
)

// checkJobs: each scheduled job ran within twice its cadence (SPEC §15),
// read from the job events of the telemetry. Without scheduler entries
// there is nothing to judge. linger reports whether systemd keeps the
// user's manager running when they are logged out (nil: unknown).
func checkJobs(d paths.Dirs, now time.Time, linger func() (bool, error)) Check {
	c := Check{ID: "jobs", Status: OK}
	st, err := state.Load(d.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.Status, c.Summary = Warn, "state.json unreadable: "+err.Error()
		return c
	}
	if st.Scheduler == nil {
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
	if st.Scheduler.Kind == "systemd" && linger != nil {
		if on, err := linger(); err == nil && !on {
			c.Status = Info
			c.Detail = "user timers stop while you are logged out (no linger): sessions are sent at the next login"
			c.Fix = "loginctl enable-linger (optional)"
		}
	}
	return c
}

// SchedulerEntries is how the installed scheduler entries compare with
// this binary's templates.
type SchedulerEntries struct {
	Installed bool
	Kind      string
	Stale     []string // jobs whose entry differs
	Elsewhere string   // why they cannot be refreshed in place
}

// checkScheduler: entries rendered by an older template lack what this
// binary's carry (the stop timeout an update's rollback needs, issue 222).
// ok is false when no entries are installed (checkJobs says so).
func checkScheduler(probe func() (SchedulerEntries, error)) (Check, bool) {
	c := Check{ID: "scheduler", Status: OK}
	s, err := probe()
	switch {
	case err != nil:
		c.Status, c.Summary, c.Detail = Info, "could not compare the scheduler entries with this binary's", err.Error()
	case !s.Installed:
		return c, false
	case s.Elsewhere != "":
		c.Status, c.Summary, c.Detail = Warn, s.Kind+" entries installed under other names or paths", s.Elsewhere
		c.Fix = "backsight jobs uninstall, then backsight jobs install"
	case len(s.Stale) > 0:
		c.Status = Warn
		c.Summary = s.Kind + " entries older than this binary's: " + strings.Join(s.Stale, ", ")
		c.Detail = "an older template may lack the stop timeout an update's rollback needs"
		c.Fix = "backsight jobs refresh"
	default:
		c.Summary = s.Kind + " entries match this binary's"
	}
	return c, true
}
