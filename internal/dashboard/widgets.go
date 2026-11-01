package dashboard

import (
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// Head is the frame every widget shares: its question, scope chip, ⓘ, CLI
// disclosure and the link to the events behind its number.
type Head struct {
	ID          string // "w-" + metric id with "." → "-" (+ "-" + slot); the popover is ID + "-info"
	Level       int    // 2 for cards, 3 for tiles
	Title       string
	Scope       string // machine | server
	NotFiltered bool
	Info        Info
	CLI         string
	Drill       string
	DrillText   string
	Aside       string // a short muted note after the spacer, before the ⓘ; "" none
	Chip        string // a chip after the scope ("developer"); "" none
	Pill        Word   // the status of the card, a pill after the spacer; zero none
}

// Info is the ⓘ popover of a widget.
type Info struct {
	Label, Name string // Label: the summary's aria-label; Name: the popover's bold title
	What        string
	Limits      string
	BasedOn     string
	Smallest    string // a metric's rows; "" omits one
	Paras       []string
	Term        string // shown in <code class="term">; "" none
	Href        string
	HrefText    string
}

// hooksFailuresWhat is what the ⓘ says for hooks.failures: the catalog's
// own text uses a term the pages do not.
const hooksFailuresWhat = "Steps that crashed, by how they failed (crashed, stopped on an error, ran too long, was stopped, exited with an error, gave an unreadable answer, input too large, could not start). In the last 24 hours, a crash, a stop on an error, an error exit or a failed start, or any kind three times, is watched; ten or more is bad."

// widgetID is the id of a widget's heading.
func widgetID(id, slot string) string {
	s := "w-" + strings.ReplaceAll(id, ".", "-")
	if slot != "" {
		s += "-" + slot
	}
	return s
}

// headOf is the frame of the widget showing metric v. cli is the page's
// command and p its parameters, which the drill link carries along.
func headOf(v MetricView, cli string, l format.Locale, slot string, p views.Params) Head {
	d := v.Def
	what := d.Definition
	if d.ID == "hooks.failures" {
		what = hooksFailuresWhat
	}
	h := Head{
		ID: widgetID(d.ID, slot), Level: 2, Title: d.Question, Scope: d.Scope, CLI: cli,
		Info: Info{
			Label: "About " + views.Label(d), Name: views.Label(d) + ": " + d.Question,
			What: what, Limits: limitsText(d, l), BasedOn: basedOn(d, v.M, l), Smallest: smallest(d, v.M, l),
			Term: d.ID, Href: "/defs#" + d.ID, HrefText: "Definition",
		},
	}
	h.NotFiltered = slices.Contains(v.M.Tags, "not_filtered")
	if h.Drill = drillHref(v.M.Drill, p); h.Drill != "" {
		h.DrillText = "See the events →"
	}
	return h
}

// fixedHead is the frame of a widget that shows no catalog metric.
func fixedHead(id, title, scope, cli string) Head {
	return Head{ID: widgetID(id, ""), Level: 2, Title: title, Scope: scope, CLI: cli}
}

// withInfo gives h an ⓘ of free paragraphs.
func withInfo(h Head, label, name, term string, paras ...string) Head {
	h.Info = Info{Label: label, Name: name, Term: term, Paras: paras}
	return h
}

// level is h at heading level n.
func level(h Head, n int) Head { h.Level = n; return h }

// limitsText says when the metric is watched.
func limitsText(d metrics.Def, l format.Locale) string {
	var parts []string
	if d.Relative {
		dir := "under"
		if d.Direction == "down" {
			dir = "over"
		}
		parts = append(parts, "Watch when it stays "+dir+" its usual level two weeks running")
	}
	if a := d.Absolute; a.On != "" {
		in := func(v float64) string {
			switch d.Unit {
			case "ratio":
				return l.Percent(v)
			case "ms":
				return l.Duration(v)
			case "chars", "tokens":
				return l.Size(v, d.Unit)
			}
			return l.Int(int64(math.Round(v)))
		}
		attention := func(prefix string) string {
			if a.Bad != nil {
				return prefix + ", attention over " + in(*a.Bad)
			}
			return prefix
		}
		switch {
		case a.On == "hi" && a.Watch != nil:
			s := "when even the upper bound is under " + in(*a.Watch)
			if len(parts) > 0 {
				parts[len(parts)-1] += ", or " + s
			} else {
				parts = append(parts, "Watch "+s)
			}
		case a.On == "lo" && a.Watch != nil:
			parts = append(parts, attention("Watch when the lower bound is over "+in(*a.Watch)))
		case (a.On == "p95" || a.On == "value") && a.Watch != nil:
			parts = append(parts, attention("Watch over "+in(*a.Watch)))
		case d.ID == "context.per_session":
			parts = append(parts, "Watch when the high value is over 1.5 × the session budget")
		}
	}
	if d.ErrorClass {
		parts = append(parts, "Judged at once, not over two weeks")
	}
	if len(parts) == 0 {
		return "No limit: shown for information."
	}
	s := strings.Join(parts, "; ") + "."
	if d.ID == "used.turn_rate" {
		if c, ok := metrics.DefOf("declare.coverage"); ok && c.Absolute.Watch != nil {
			s += " Unreliable while Claude says what it used on under " + l.Percent(*c.Absolute.Watch) + " of turns."
		}
	}
	return s
}

// nouns are what a metric counts, singular and plural; event(s) otherwise.
var nouns = map[string][2]string{
	"recall.hit_rate": {"prompt", "prompts"}, "recall.latency": {"prompt", "prompts"}, "search.rate": {"prompt", "prompts"},
	"used.turn_rate": {"declared turn", "declared turns"}, "declare.coverage": {"turn", "turns"},
	"verdict.wrong_rate":  {"verdict", "verdicts"},
	"context.per_session": {"session", "sessions"}, "pipeline.flow": {"session", "sessions"},
	"search.empty_rate": {"search", "searches"},
}

func nounOf(id string) (string, string) {
	if n, ok := nouns[id]; ok {
		return n[0], n[1]
	}
	return "event", "events"
}

// basedOn says how many events the number rests on.
func basedOn(d metrics.Def, m metrics.Metric, l format.Locale) string {
	if m.Den == nil {
		return dash
	}
	one, many := nounOf(d.ID)
	s := format.Count(l, *m.Den, one, many)
	if m.Status == metrics.StatusTooFew {
		s += " (" + l.Int(int64(d.MinN)) + " needed)"
	}
	if slices.Contains(m.Tags, "judged_28d") {
		s += ", last 28 days"
	}
	return s
}

// smallest is the smallest change the metric can see.
func smallest(d metrics.Def, m metrics.Metric, l format.Locale) string {
	if m.MDC == nil {
		return "not yet measurable"
	}
	v := *m.MDC
	switch d.Unit {
	case "ratio":
		return "about " + l.Int(int64(math.Round(v*100))) + " pts"
	case "ms":
		return "about " + l.Duration(v)
	case "chars", "tokens":
		return "about " + l.Size(v, d.Unit)
	}
	return "about " + l.Int(int64(math.Round(v)))
}

// words splits a command like a shell would, quotes removed; ok is false
// for an unclosed quote.
func shellWords(s string) ([]string, bool) {
	var out []string
	var cur strings.Builder
	in, quote := false, rune(0)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if in {
		out = append(out, cur.String())
	}
	return out, true
}

// drillHref is the page that lists the events a drill command lists, with
// the page's own window and filters carried along for every key the drill
// does not set itself. "" for a command that is not a stats view of the
// dashboard.
func drillHref(drill string, p views.Params) string {
	w, ok := shellWords(drill)
	if !ok || len(w) < 3 || w[0] != "backsight" || w[1] != "stats" || !slices.Contains(metrics.Views, w[2]) {
		return ""
	}
	view, rest := w[2], w[3:]
	q := url.Values{}
	path := "/" + view
	if view == "overview" {
		path = "/"
	}
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "--") {
		if rest[0] == "." || rest[0] == ".." {
			return ""
		}
		path += "/" + url.PathEscape(rest[0])
		rest = rest[1:]
	}
	for i := 0; i < len(rest); i++ {
		if !strings.HasPrefix(rest[i], "--") {
			return ""
		}
		k := strings.TrimPrefix(rest[i], "--")
		if !slices.Contains(views.CommonParams, k) && !slices.Contains(views.ViewParams[view], k) {
			return ""
		}
		if k == "dev" {
			q.Set(k, "1")
			continue
		}
		if i+1 >= len(rest) {
			return ""
		}
		i++
		if strings.HasPrefix(rest[i], "--") {
			return ""
		}
		q.Set(k, rest[i])
	}
	// The window is one group: a drill that sets any of days, since or
	// until keeps the page's window out (the two forms are refused together).
	ownWindow := q.Has("days") || q.Has("since") || q.Has("until")
	set := func(k, v string) {
		if v != "" && !q.Has(k) {
			q.Set(k, v)
		}
	}
	if !ownWindow {
		if p.DaysSet {
			set("days", strconv.Itoa(p.Days))
		}
		set("since", p.Since)
		set("until", p.Until)
	}
	set("tz", p.TZ)
	set("project", p.Project)
	switch {
	case p.DevSet && !p.Dev:
		set("dev", "0")
	case p.Dev:
		set("dev", "1")
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// quoteArg is s as one shell word: bare when it holds only characters that
// mean nothing to a shell, else in single quotes (a ' written '\”).
func quoteArg(s string) string {
	safe := s != ""
	for _, r := range s {
		plain := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./:=@%+,-", r)
		if !plain {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cliFor is the command that prints what the page shows.
func cliFor(view, id string, p views.Params) string {
	var b strings.Builder
	b.WriteString("backsight stats " + view)
	if id != "" {
		b.WriteString(" " + quoteArg(id))
	}
	flag := func(k, v string) {
		if v != "" {
			b.WriteString(" --" + k + " " + quoteArg(v))
		}
	}
	if p.DaysSet {
		flag("days", strconv.Itoa(p.Days))
	}
	flag("since", p.Since)
	flag("until", p.Until)
	flag("tz", p.TZ)
	flag("project", p.Project)
	flag("path", p.Path)
	if p.Dev {
		b.WriteString(" --dev")
	}
	if p.Limit != 0 && p.Limit != metrics.DefaultLimit {
		flag("limit", strconv.Itoa(p.Limit))
	}
	if p.Offset > 0 {
		flag("offset", strconv.Itoa(p.Offset))
	}
	flag("has", p.Has)
	flag("sort", p.Sort)
	flag("state", p.State)
	return b.String()
}

// Word is a status word and the class that colours it.
type Word struct{ Word, Class string }

// statusWord is the word a metric's status is shown as (axisLearning: the
// metric's axis has too little history to be judged yet).
func statusWord(_ metrics.Def, m metrics.Metric, axisLearning bool) (word, class string) {
	switch {
	case m.Borderline:
		return "borderline", "st-border"
	case slices.Contains(m.Tags, "unreliable"):
		return "unreliable", "st-unrel"
	case m.Status == metrics.StatusNoData && axisLearning:
		return "learning", "st-learn"
	}
	switch m.Status {
	case metrics.StatusOK:
		return "ok", "st-ok"
	case metrics.StatusWatch:
		return "watch", "st-watch"
	case metrics.StatusBad:
		return "attention", "st-bad"
	case metrics.StatusTooFew:
		return "too few", "st-few"
	case metrics.StatusInfo:
		return "info", "st-info"
	}
	return "no data", "st-few"
}

// Fig is a figure: a label, its value, its status and its change.
type Fig struct {
	Label, Value, Word, Class string
	Delta                     *format.Delta
	Dev                       string
	Sub                       string // a muted line under the value; "" none
	Icon                      string // the icon before the label (server, pc, doc); "" none
	Swatch                    string // the class of a swatch before the label (a kind of memory); "" none
}

// figOf is the figure of metric v; the developer's detail is its id.
func figOf(v MetricView, l format.Locale) Fig {
	f := Fig{Label: views.Label(v.Def), Value: dash}
	if v.Found {
		f.Value = l.Value(v.Def, v.M)
		if d, ok := l.Delta(v.Def, v.M); ok {
			f.Delta = &d
		}
	}
	f.Word, f.Class = statusWord(v.Def, v.M, false)
	if v.ShowID {
		f.Dev = v.ID
	}
	return f
}
