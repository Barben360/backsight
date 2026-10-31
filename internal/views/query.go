package views

import (
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
)

// Params are the parameters of a view, as the command line and the
// dashboard give them. Ranges and enums are checked by Query.
type Params struct {
	Days             int
	DaysSet          bool // days was given (0 is then refused)
	Since, Until, TZ string
	Project, Path    string
	Dev, DevSet      bool // DevSet: dev was given (a page's query beats the preference)
	Limit, Offset    int  // Limit: metrics.DefaultLimit when not given
	Has, Sort, State string
	Text             bool // CLI only: the API never sets it
}

// Allowed values of the view parameters.
var (
	hasValues   = []string{"used", "wrong", "undeclared", "memory"}
	sortValues  = []string{"shown", "used", "rate", "last_shown"}
	stateValues = []string{"hidden", "wrong", "never_used"}
)

// HasValues, SortValues and StateValues are the allowed values of has, sort
// and state.
func HasValues() []string   { return slices.Clone(hasValues) }
func SortValues() []string  { return slices.Clone(sortValues) }
func StateValues() []string { return slices.Clone(stateValues) }

// TakesID reports whether the view can show one item, named by an id.
func TakesID(view string) bool {
	switch view {
	case "projects", "sessions", "memories", "days", "defs":
		return true
	}
	return false
}

// Query maps the parameters and the id of a view to its Query. The window
// is days (default 7) or since/until read in the zone of tz (this machine's
// by default); a date alone is midnight in that zone.
func Query(view string, p Params, id string, now time.Time) (metrics.Query, error) {
	if _, err := time.Parse("2006-01-02", id); view == "days" && id != "" && err != nil {
		return metrics.Query{}, Usage("a day is written YYYY-MM-DD")
	}
	loc, name, err := zone(p.TZ)
	if err != nil {
		return metrics.Query{}, err
	}
	var w metrics.Window
	switch {
	case (p.DaysSet || p.Days != 0) && (p.Since != "" || p.Until != ""):
		return metrics.Query{}, Usage("give either --days or --since/--until")
	case p.Until != "" && p.Since == "":
		return metrics.Query{}, Usage("--until needs --since")
	case p.Since != "":
		since, err := parseTime(p.Since, loc)
		if err != nil {
			return metrics.Query{}, Usage("--since: %v", err)
		}
		var until time.Time
		if p.Until != "" {
			if until, err = parseTime(p.Until, loc); err != nil {
				return metrics.Query{}, Usage("--until: %v", err)
			}
		}
		if w, err = metrics.NewWindow(now, 0, since, until, loc, name); err != nil {
			return metrics.Query{}, Usage("--since must be before --until (or now)")
		}
	default:
		days := 7
		if p.DaysSet || p.Days != 0 {
			days = p.Days
		}
		if days < 1 || days > 365 {
			return metrics.Query{}, Usage("--days: a whole number from 1 to 365")
		}
		if w, err = metrics.NewWindow(now, days, time.Time{}, time.Time{}, loc, name); err != nil {
			return metrics.Query{}, Usage("%v", err)
		}
	}
	if p.Limit < 1 || p.Limit > metrics.MaxLimit {
		return metrics.Query{}, Usage("--limit: a whole number from 1 to %d", metrics.MaxLimit)
	}
	if p.Offset < 0 {
		return metrics.Query{}, Usage("--offset: a whole number, 0 or more")
	}
	for _, c := range []struct {
		name, value string
		allowed     []string
	}{{"has", p.Has, hasValues}, {"sort", p.Sort, sortValues}, {"state", p.State, stateValues}} {
		if c.value != "" && !slices.Contains(c.allowed, c.value) {
			return metrics.Query{}, Usage("--%s: one of %s", c.name, strings.Join(c.allowed, ", "))
		}
	}
	if p.Text && id == "" {
		return metrics.Query{}, Usage("--text needs a memory id")
	}
	return metrics.Query{Window: w, Project: p.Project, Path: p.Path, ID: id, Limit: p.Limit, Offset: p.Offset,
		Has: p.Has, Sort: p.Sort, State: p.State, Dev: p.Dev, Text: p.Text}, nil
}

// zone is the zone of tz, this machine's when empty, and its name.
func zone(tz string) (*time.Location, string, error) {
	loc := time.Local
	if tz != "" {
		var err error
		if loc, err = time.LoadLocation(tz); err != nil {
			return nil, "", Usage("--tz: unknown zone %q (a name such as Europe/Paris or UTC)", tz)
		}
	}
	return loc, metrics.ZoneName(loc, os.Getenv, os.Readlink), nil
}
