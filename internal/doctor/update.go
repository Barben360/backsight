package doctor

import (
	"errors"
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
