package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/state"
)

// checkUpdate reports the last update check of jobs maintain (SPEC §11.7)
// as information: a pending release, or why the check failed (decision
// 0067: the repository is private until M7, so a check without a token
// fails, and that is no fault of the install).
func checkUpdate(d paths.Dirs, running string, now time.Time) Check {
	c := Check{ID: "update", Status: Info, Fix: "backsight update check"}
	pending := func() { c.Fix = "backsight update apply" }
	st, err := state.Load(d.StateFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.Summary = "state.json unreadable: " + err.Error()
		return c
	}
	uc := st.UpdateCheck
	switch {
	case uc == nil:
		c.Summary = "no update check yet (jobs maintain runs one daily)"
	case uc.Error != "":
		c.Summary = "last update check failed (" + now.Sub(uc.CheckedAt).Round(time.Minute).String() + " ago)"
		c.Detail = uc.Error
		if release.NewerThan(uc.Latest, running) {
			c.Detail += "; the previous check found " + uc.Latest
		}
	case release.NewerThan(uc.Latest, running):
		pending()
		c.Summary = "update " + uc.Latest + " available"
		if uc.Summary != "" {
			c.Summary += " (" + uc.Summary + ")"
		}
	default:
		c.Status, c.Fix = OK, ""
		c.Summary = "up to date (checked " + uc.CheckedAt.UTC().Format("2006-01-02 15:04 UTC") + ")"
	}
	return c
}

// checkUpdateIncomplete reports an update apply that neither finished nor
// rolled back (its marker): the shim may run the new binary while state,
// migrations or the plugin are the old ones. The way back is the previous
// binary's client install.
func checkUpdateIncomplete(d paths.Dirs) Check {
	c := Check{ID: "update-incomplete", Status: OK, Summary: "no update in progress"}
	m, err := state.ReadUpdateMarker(d)
	switch {
	case m == nil:
		return c
	case err != nil:
		c.Status, c.Summary, c.Detail = Error, "an update did not finish", err.Error()
		c.Fix = "backsight client install, then remove " + d.UpdateMarker()
		return c
	}
	if h := state.Inspect(d.MaintenanceLock()); h != nil && !h.Stale && h.Owner.Command == "update apply" {
		c.Status, c.Summary = Info, fmt.Sprintf("update from %s to %s in progress", m.From, m.To)
		return c
	}
	c.Status = Error
	c.Summary = fmt.Sprintf("an update from %s to %s did not finish (started %s)", m.From, m.To, m.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
	c.Detail = "the shim may run " + m.Binary + " while the state or the plugin are the previous version's"
	way := m.WayBack
	if way == "" {
		way = m.Previous + " client install"
	}
	c.Fix = way + " (back to " + m.From + "), then remove " + d.UpdateMarker() + "; or rerun backsight update apply"
	return c
}
