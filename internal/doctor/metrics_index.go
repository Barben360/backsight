package doctor

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/stats"
)

// IndexFacts is what the cli read of the metrics index (read-only); doctor
// does not import the index package.
type IndexFacts struct {
	Built, Newer    bool
	File            string
	Size            int64
	Schema, Head    int
	StoppedAt       time.Time
	MigrateFailedAt time.Time
	Errors24h       int
	ErrorsByReason  map[string]int // the last 24 h
	LagSeconds      int64
	Folder          string // "" local, else nfs, cifs, smb, smb2, 9p, sshfs, unc, onedrive, icloud, dropbox
}

const (
	indexRebuildFix = "backsight stats index rebuild"
	indexLagLimit   = 3600 // seconds behind that matter while jobs tick runs
)

// tickRecent reports whether a jobs tick ran within the last hour of the
// telemetry (as checkJobs reads it).
func tickRecent(d paths.Dirs, now time.Time) bool {
	ev, err := stats.Load(d.TelemetryDir(), now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		return false
	}
	for _, e := range ev {
		if e.Event == "job" && e.Name == "tick" && !e.TS.Before(now.Add(-time.Hour)) {
			return true
		}
	}
	return false
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// checkMetricsIndex judges the local metrics index. err is the cli's
// failure to read it, already reduced to its class (never an error's
// text, which can hold paths and ends up in report bundles). The first
// problem is the summary and fix; the others go in the detail.
func checkMetricsIndex(f IndexFacts, err error, tickRecent bool) Check {
	c := Check{ID: "metrics-index", Status: OK}
	if err != nil {
		c.Status, c.Summary, c.Fix = Warn, "cannot read the metrics index", indexRebuildFix
		if err.Error() != "" {
			c.Summary += " (" + err.Error() + ")"
		}
		return c
	}
	if !f.Built {
		c.Status, c.Summary = Info, "not built yet: the next jobs tick builds it"
		return c
	}
	var warns []string
	if !f.StoppedAt.IsZero() {
		warns = append(warns, "unreadable, and the automatic rebuild failed ("+f.StoppedAt.UTC().Format(time.RFC3339)+")")
	}
	if !f.MigrateFailedAt.IsZero() && f.Schema < f.Head {
		warns = append(warns, fmt.Sprintf("not migrated to this backsight's schema (%d of %d): the last attempt failed at %s", f.Schema, f.Head, f.MigrateFailedAt.UTC().Format(time.RFC3339)))
	}
	var folderWarn string
	if f.Folder != "" {
		folderWarn = "the state directory is on " + f.Folder + " (a network or synced folder): the index runs without its write-ahead log"
		warns = append(warns, folderWarn)
	}
	if f.Errors24h > 0 {
		reasons := make([]string, 0, len(f.ErrorsByReason))
		for r := range f.ErrorsByReason {
			reasons = append(reasons, r)
		}
		sort.Slice(reasons, func(i, j int) bool {
			a, b := f.ErrorsByReason[reasons[i]], f.ErrorsByReason[reasons[j]]
			return a > b || a == b && reasons[i] < reasons[j]
		})
		parts := make([]string, len(reasons))
		for i, r := range reasons {
			parts[i] = fmt.Sprintf("%s %d", r, f.ErrorsByReason[r])
		}
		warns = append(warns, fmt.Sprintf("%d line(s) of telemetry or journal skipped in the last 24 h (%s)", f.Errors24h, strings.Join(parts, ", ")))
	}
	if f.LagSeconds > indexLagLimit && tickRecent {
		warns = append(warns, fmt.Sprintf("%d h behind while jobs tick runs", f.LagSeconds/3600))
	}
	if len(warns) > 0 {
		c.Status, c.Summary, c.Detail = Warn, warns[0], strings.Join(warns[1:], "; ")
		c.Fix = indexRebuildFix
		if warns[0] == folderWarn {
			c.Fix = "keep backsight's state directory on a local disk"
		}
		return c
	}
	if f.Newer {
		c.Status, c.Summary = Info, "a newer backsight's index: this one uses its own file beside it"
		return c
	}
	c.Summary = fmt.Sprintf("schema %d, %s, %d s behind", f.Schema, humanSize(f.Size), f.LagSeconds)
	return c
}
