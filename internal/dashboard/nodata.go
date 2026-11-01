package dashboard

import (
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
)

// NoData is the sentence that stands where a number would be. Code, when
// set, is shown after Text, then Close; Tail is a muted second sentence;
// Pick, when set, introduces Links.
type NoData struct {
	Text, Code, Close, Tail, Pick string
	Links                         []Choice
	Class                         string
}

// windowDaysOf is the days a window phrase spans; 0 for a custom window.
func windowDaysOf(phrase string) int {
	if phrase == "last 24 h" {
		return 1
	}
	n, ok := strings.CutPrefix(phrase, "last ")
	if !ok {
		return 0
	}
	n, ok = strings.CutSuffix(n, " days")
	d, err := strconv.Atoi(n)
	if !ok || err != nil {
		return 0
	}
	return d
}

// noDataOf is the sentence for a metric without a number; false when the
// metric has one.
func noDataOf(v MetricView, f Frame, l format.Locale, loc *time.Location) (NoData, bool) {
	m := v.M
	switch {
	case v.Found && m.Status == metrics.StatusTooFew:
		_, many := nounOf(v.Def.ID)
		one, _ := nounOf(v.Def.ID)
		s := "Too few to judge."
		if m.Num != nil && m.Den != nil {
			s = l.Int(*m.Num) + " of " + l.Int(*m.Den) + ": too few to judge."
		}
		if m.Den != nil {
			s += " " + format.Count(l, *m.Den, one, many) + "; " + l.Int(int64(v.Def.MinN)) + " are needed."
		}
		return NoData{Text: s, Class: "st-few"}, true
	case v.Found && m.Status != metrics.StatusNoData:
		return NoData{}, false
	}
	switch m.Reason {
	case metrics.ReasonNoEvents:
		if snapshotMetric[v.Def.ID] {
			return NoData{Text: "No reading yet."}, true
		}
		n := NoData{Text: "Nothing recorded in the " + f.Window + "."}
		if cur := windowDaysOf(f.Window); cur > 0 && cur < 90 {
			for _, w := range f.Windows {
				if d := daysOfLabel(w.Label); d > cur {
					n.Links = append(n.Links, w)
				}
			}
			if len(n.Links) > 0 {
				n.Pick = "Pick a longer window:"
			}
		}
		return n, true
	case metrics.ReasonFeedbackOff:
		return NoData{Text: "Memory feedback is off (", Code: "feedback.enabled",
			Close: ").", Tail: "Quality numbers need it. Found, Cost and Flowing still work."}, true
	case metrics.ReasonServerOnlyHost:
		return NoData{Text: "This PC runs the server only: activity is recorded on the PCs that use it."}, true
	case metrics.ReasonServerDown:
		var at *time.Time
		switch {
		case f.ServerRead != nil:
			at = f.ServerRead.LastOK
		case f.Scope != nil:
			at = f.Scope.ServerOKAt
		}
		s := "Memory server not answering."
		if at != nil && !at.IsZero() {
			s = "Memory server not answering since " + format.Clock(f.Generated, *at, loc) + "."
		}
		return NoData{Text: s, Class: "st-watch"}, true
	case metrics.ReasonNotConfigured:
		return NoData{Text: "No memory server is set up on this PC."}, true
	case metrics.ReasonServerUnsupported:
		return NoData{Text: "This memory server does not provide it."}, true
	}
	return NoData{Text: "No data."}, true
}

// snapshotMetric holds the metrics that say a state now (what is waiting,
// whether the server answers, how many memories there are), not events over
// a window: their no-data sentence does not name a window.
var snapshotMetric = map[string]bool{
	"local.backlog": true, "server.reach": true, "server.queue": true, "server.extraction_delay": true,
	"bank.size": true, "bank.growth": true, "bank.by_project": true, "bank.consolidation": true,
	"doctor.check": true, "jobs.last_run": true,
}

// daysOfLabel is the days of a window link's label ("7 d"); 0 when unknown.
func daysOfLabel(label string) int {
	for _, w := range windowDays {
		if w.label == label {
			return w.days
		}
	}
	return 0
}
