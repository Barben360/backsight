package views

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
)

// CommonParams are the parameters every view takes; ViewParams adds the
// ones a view alone takes.
var CommonParams = []string{"days", "since", "until", "tz", "project", "path", "dev", "limit", "offset"}

// ViewParams are the parameters that only some views take.
var ViewParams = map[string][]string{"sessions": {"has"}, "memories": {"sort", "state"}}

// parseTime reads a window bound: RFC 3339 or YYYY-MM-DDTHH:MM with a
// zone, or a date alone, which is midnight in loc.
func parseTime(s string, loc *time.Location) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a time (RFC 3339, YYYY-MM-DDTHH:MMZ or YYYY-MM-DD)", s)
}

// FromValues reads the parameters of a view from a query string. A key the
// view does not take, or given twice, is refused; the integers and dev are
// read here, their ranges and the enums by Query.
func FromValues(view string, v url.Values) (Params, error) {
	allowed := append(slices.Clone(CommonParams), ViewParams[view]...)
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	p := Params{Limit: metrics.DefaultLimit}
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			return Params{}, Usage("unknown parameter %q for %s: one of %s", k, view, strings.Join(allowed, ", "))
		}
		if len(v[k]) != 1 {
			return Params{}, Usage("give %s once", k)
		}
		s := v[k][0]
		switch k {
		case "days":
			n, err := strconv.Atoi(s)
			if err != nil {
				return Params{}, Usage("--days: a whole number from 1 to 365")
			}
			p.Days, p.DaysSet = n, true
		case "limit":
			n, err := strconv.Atoi(s)
			if err != nil {
				return Params{}, Usage("--limit: a whole number from 1 to %d", metrics.MaxLimit)
			}
			p.Limit = n
		case "offset":
			n, err := strconv.Atoi(s)
			if err != nil {
				return Params{}, Usage("--offset: a whole number, 0 or more")
			}
			p.Offset = n
		case "dev":
			switch s {
			case "1", "true":
				p.Dev = true
			case "0", "false":
			default:
				return Params{}, Usage("--dev: 1, true, 0 or false")
			}
			p.DevSet = true
		case "since":
			p.Since = s
		case "until":
			p.Until = s
		case "tz":
			p.TZ = s
		case "project":
			p.Project = s
		case "path":
			p.Path = s
		case "has":
			p.Has = s
		case "sort":
			p.Sort = s
		case "state":
			p.State = s
		}
	}
	return p, nil
}
