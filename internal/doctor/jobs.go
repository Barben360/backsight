package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/scheduler"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/stats"
)

// schedulerMode is whether the scheduler entries can run the jobs.
type schedulerMode int

const (
	schedulerOn   schedulerMode = iota
	schedulerOff                // none recorded, and setup was answered scheduler=off
	schedulerIdle               // systemd entries recorded, no timer of theirs enabled
)

// answeredOff reports whether the last setup run was answered
// scheduler=off.
func answeredOff(d paths.Dirs) bool {
	r, err := setup.Load(d.SetupFile())
	return err == nil && r != nil && r.Answers["scheduler"] == "off"
}

func modeOf(d paths.Dirs, sc *state.Scheduler) schedulerMode {
	switch {
	case sc == nil && answeredOff(d):
		return schedulerOff
	case sc != nil && sc.Kind == "systemd" && slices.ContainsFunc(sc.Files, func(f string) bool { return strings.HasSuffix(f, ".timer") }) &&
		!scheduler.TimersEnabled(sc.Files):
		return schedulerIdle
	}
	return schedulerOn
}

// schedulerModeOf reads the mode from state.json (unreadable: on, the
// jobs check says why).
func schedulerModeOf(d paths.Dirs) schedulerMode {
	st, err := state.Load(d.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return schedulerOn
	}
	return modeOf(d, st.Scheduler)
}

// byHand is what runs the jobs without a scheduler.
const byHand = "run backsight jobs tick every 15 min and backsight jobs maintain daily (it makes the backup), by hand or from a scheduler of your own"

// checkJobs: each scheduled job ran within twice its cadence (SPEC §15),
// and its newest job event is not a failure, read from the job events of
// the telemetry. Without scheduler entries there is nothing to judge.
// linger reports whether systemd keeps the user's manager running when
// they are logged out (nil: unknown).
func checkJobs(d paths.Dirs, now time.Time, linger func() (bool, error)) Check {
	c := Check{ID: "jobs", Status: OK}
	st, err := state.Load(d.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.Status, c.Summary = Warn, "state.json unreadable: "+err.Error()
		return c
	}
	switch modeOf(d, st.Scheduler) {
	case schedulerOff:
		c.Status, c.Summary = Info, "scheduler off (setup was answered scheduler=off): nothing runs on a timer, sessions are sent when they end"
		c.Fix = byHand
		return c
	case schedulerIdle:
		c.Status, c.Summary = Warn, "systemd entries written but not enabled: no timer runs jobs tick or jobs maintain"
		c.Detail = "systemd's user session was likely unreachable when they were installed"
		c.Fix = "backsight jobs install from a login session of this user (an ssh login, not su or sudo); on a machine without one, backsight jobs uninstall and rerun setup with --answer scheduler=off"
		if answeredOff(d) {
			c.Detail += "; setup was answered scheduler=off since"
			c.Fix = "backsight jobs uninstall"
		}
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
	newest := map[string]stats.Event{} // a "started" one: a run in progress, not judged yet
	for _, e := range ev {
		if e.Event != "job" {
			continue
		}
		if e.TS.After(last[e.Name]) {
			last[e.Name] = e.TS
		}
		if f, ok := newest[e.Name]; !ok || !e.TS.Before(f.TS) {
			newest[e.Name] = e
		}
	}
	var late, failing []string
	firstFailing := ""
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
			continue
		}
		// It runs, but its last run failed: running on time is not working.
		if f, ok := newest[j.Name]; ok && f.Status == "failed" {
			item := fmt.Sprintf("%s failed %s ago", j.Name, now.Sub(f.TS).Round(time.Minute))
			if reason, _ := f.Details["reason"].(string); reason != "" {
				item += " (" + reason + ")"
			}
			failing = append(failing, item)
			if firstFailing == "" {
				firstFailing = j.Name
			}
		}
	}
	if len(late) > 0 || len(failing) > 0 {
		var parts []string
		if len(late) > 0 {
			parts = append(parts, fmt.Sprintf("scheduled job late: %v", late))
			c.Fix = map[string]string{"systemd": "systemctl --user list-timers 'backsight-*'", "launchd": "launchctl list | grep backsight",
				"schtasks": `schtasks /Query /TN backsight\tick`}[st.Scheduler.Kind]
		}
		if len(failing) > 0 {
			parts = append(parts, fmt.Sprintf("scheduled job failing: %v", failing))
			if c.Fix == "" {
				c.Fix = "backsight jobs " + firstFailing // run by hand, it says why
			}
		}
		c.Status, c.Summary = Warn, strings.Join(parts, "; ")
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
func checkScheduler(probe func() (SchedulerEntries, error), mode schedulerMode) (Check, bool) {
	c := Check{ID: "scheduler", Status: OK}
	s, err := probe()
	switch {
	case mode == schedulerOff && err == nil && !s.Installed:
		c.Status, c.Summary = Info, "off: setup was answered scheduler=off"
	case mode == schedulerIdle:
		c.Status, c.Summary = Info, "systemd entries not enabled: see the jobs check"
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
