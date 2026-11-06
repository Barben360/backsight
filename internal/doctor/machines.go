package doctor

import (
	"fmt"
	"time"

	"github.com/Barben360/backsight/internal/machines"
	"github.com/Barben360/backsight/internal/paths"
)

// machinesShareWarn: a publish of this PC's daily figures that has been
// failing for longer than this is no longer a passing trouble.
const machinesShareWarn = 24 * time.Hour

// checkMachinesShare: whether this PC's daily figures reach the server (the
// jobs tick publishes them once a UTC day; published.json says when and
// since when a publish fails). Ok when sharing is off, nothing was
// published yet, or the last failures are less than a day old.
func checkMachinesShare(d paths.Dirs, enabled, share bool, now time.Time) Check {
	c := Check{ID: "machines-share", Status: OK}
	if !enabled || !share {
		c.Summary = "not shared (feedback or [feedback] share_ledger is off): this PC's daily figures are neither sent nor read"
		return c
	}
	rec, ok := machines.LoadPublished(d.MetricsDir())
	if ok && !rec.FailedSince.IsZero() && now.Sub(rec.FailedSince) > machinesShareWarn {
		c.Status, c.Fix = Warn, "backsight jobs tick"
		c.Summary = fmt.Sprintf("this PC's daily figures have not reached the server for %d h (%s)", int(now.Sub(rec.FailedSince)/time.Hour), failureText(rec.Failure))
		if rec.Failure == "budget" {
			c.Fix = indexRebuildFix
			c.Summary = fmt.Sprintf("this PC's daily figures have not been sent for %d h: the activity index never finishes catching up", int(now.Sub(rec.FailedSince)/time.Hour))
		}
		return c
	}
	if !ok || rec.At.IsZero() {
		c.Summary = "not published yet"
		return c
	}
	c.Summary = fmt.Sprintf("published %s ago", now.Sub(rec.At).Round(time.Hour))
	return c
}

// failureText is the plain words of a publish failure class.
func failureText(class string) string {
	switch class {
	case "server":
		return "the server refused or did not answer"
	case "too_large":
		return "the document is too large"
	case "compute":
		return "the figures could not be computed"
	}
	return "unknown cause"
}
