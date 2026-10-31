package views

import (
	"fmt"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
)

// CountOf is n with its noun, singular for 1.
func CountOf(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return Grouped(n) + " " + many
}

// SystemRow is one line of the system block: what it is about and what
// it says.
type SystemRow struct{ Label, Text string }

// SystemRows are the lines of the system block, with every time written
// by at (a zero time reads "never").
func SystemRows(s metrics.SystemInfo, at func(time.Time) string) []SystemRow {
	when := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return at(t)
	}
	reach := "no answer recorded yet"
	if !s.Reach.LastOK.IsZero() {
		reach = "last answer " + when(s.Reach.LastOK)
	}
	if !s.Reach.FailingSince.IsZero() {
		reach = "not answering since " + when(s.Reach.FailingSince)
	}
	if !s.Reach.BreakerOpenUntil.IsZero() {
		reach += fmt.Sprintf("; paused until %s after %s", when(s.Reach.BreakerOpenUntil), CountOf(int64(s.Reach.BreakerFailures), "failure", "failures"))
	}
	backup := "none yet"
	if !s.Backup.CreatedAt.IsZero() {
		ok := "no"
		if s.Backup.OK {
			ok = "yes"
		}
		backup = "last " + when(s.Backup.CreatedAt) + ", checked " + when(s.Backup.VerifiedAt) + ", ok: " + ok
	}
	update := "none newer"
	switch {
	case s.Update.CheckFailed:
		update = "the last check failed"
	case s.Update.Newer > 0:
		update = CountOf(int64(s.Update.Newer), "newer release", "newer releases")
	}
	checked := "never checked"
	if !s.Update.CheckedAt.IsZero() {
		checked = "checked " + when(s.Update.CheckedAt)
	}
	idx := fmt.Sprintf("ingested %s, %d s behind", when(s.Index.IngestedAt), s.Index.LagSeconds)
	if s.Index.Partial {
		idx += ", still catching up"
	}
	return []SystemRow{
		{"Memory server", fmt.Sprintf("version %s, running in %s, mode %s", s.ServerVersion, s.Runtime, s.Mode)},
		{"Reach", reach},
		{"Backup", backup},
		{"Updates", fmt.Sprintf("%s (%s)", update, checked)},
		{"Activity index", idx},
	}
}

// IntervalText is the interval of a metric that has one, in its unit; ""
// otherwise.
func IntervalText(d metrics.Def, m metrics.Metric) string {
	if m.CI == nil {
		return ""
	}
	if d.Unit == "ratio" {
		return Percent(m.CI[0]) + "–" + Percent(m.CI[1]) + " %"
	}
	return ValueIn(d.Unit, m.CI[0], m) + " – " + ValueIn(d.Unit, m.CI[1], m)
}
