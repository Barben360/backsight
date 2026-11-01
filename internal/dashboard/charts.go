package dashboard

import (
	"html/template"
	"math"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// sparkOf is the small trend line of metric v over its daily series, in the
// metric's own unit; label names it for a reader who cannot see it.
func sparkOf(v MetricView, label string, l format.Locale) template.HTML {
	// The window's first day is cut short (Partial) and would draw as a dip:
	// it is left out, so a 7-day window draws its 7 days, today last.
	series := v.M.Series
	if len(series) > 0 && series[0].Partial {
		series = series[1:]
	}
	y := make([]*float64, len(series))
	for i, p := range series {
		y[i] = p.Value
	}
	return chart.Spark{Label: label, Y: y, Fmt: unitFormat(v.Def.Unit, l)}.HTML()
}

// unitFormat writes a value of a catalog unit the way the pages write it.
func unitFormat(unit string, l format.Locale) chart.Num {
	switch unit {
	case "ratio":
		return l.Percent
	case "ms":
		return l.Duration
	case "chars", "tokens":
		return func(x float64) string { return l.Size(x, unit) }
	}
	return func(x float64) string { return l.Int(int64(math.Round(x))) }
}

// Daily charts. The window's first day is cut short (Partial): a count
// drawn for it would read as a dip, so the count charts leave it out, as
// sparkOf does. A rate or a percentile is a value per event, not a total:
// the line charts keep that day, where the faint bars and the interval show
// how little it rests on.

const (
	hourKey = "2006-01-02T15:04-07:00"
	dayKey  = "2006-01-02"
)

// xName is the label of a bucket key under a chart: "Jan 2" for a day,
// "15:04" for an hour; the key itself when it is neither.
func xName(key string) string {
	if t, err := time.Parse(dayKey, key); err == nil {
		return t.Format("Jan 2")
	}
	if t, err := time.Parse(hourKey, key); err == nil {
		return t.Format("15:04")
	}
	return key
}

// pointKey is the bucket key of a point.
func pointKey(p metrics.Point) string {
	switch {
	case p.Day != "":
		return p.Day
	case p.Hour != "":
		return p.Hour
	}
	return p.Week
}

// withoutPartialLead drops a first point that covers only part of its day,
// unless it is the only one.
func withoutPartialLead(ps []metrics.Point) []metrics.Point {
	if len(ps) > 1 && ps[0].Partial {
		return ps[1:]
	}
	return ps
}

func countOf(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// lineStyle is the class and legend word of each charted metric.
var lineStyle = map[string][2]string{
	"used.turn_rate": {"s-used", "Used"}, "declare.coverage": {"s-coverage", "Said what it used"},
	"recall.hit_rate": {"s-hit", "Found"}, "search.rate": {"s-search", "Claude searched"},
	"search.empty_rate": {"s-empty", "Searches that found nothing"},
	"recall.latency":    {"s-latency", ""}, "search.latency": {"s-search-latency", ""},
}

// barNames is what the faint bars of a chart count, by its first metric.
var barNames = map[string]string{
	"used.turn_rate": "turns", "recall.hit_rate": "prompts", "search.rate": "prompts", "recall.latency": "prompts",
}

func barName(id string) string {
	if n, ok := barNames[id]; ok {
		return n
	}
	_, many := nounOf(id)
	return many
}

// styleOf is the class and name of a metric's line.
func styleOf(v MetricView) (class, name string) {
	s := lineStyle[v.ID]
	class, name = s[0], s[1]
	if class == "" {
		class = "s-fg"
	}
	if name == "" {
		name = views.Label(v.Def)
	}
	return class, name
}

// withSeries are the metrics that have days to draw.
func withSeries(vs []MetricView) []MetricView {
	var out []MetricView
	for _, v := range vs {
		if v.Found && len(v.M.Series) > 0 {
			out = append(out, v)
		}
	}
	return out
}

// longest is the series of the given metrics with most points.
func longest(vs []MetricView) []metrics.Point {
	var best []metrics.Point
	for _, v := range vs {
		if len(v.M.Series) > len(best) {
			best = v.M.Series
		}
	}
	return best
}

func xsOf(ps []metrics.Point) []string {
	xs := make([]string, len(ps))
	for i, p := range ps {
		xs[i] = xName(pointKey(p))
	}
	return xs
}

// linesOf is a line per metric over the days of its series. A rate is
// drawn 0 to 100 % with the 95 % interval of the first line and the faint
// bars of its denominators; anything else is drawn on a scale of its own,
// with the faint bars of the days' counts.
func linesOf(l format.Locale, label, more string, rate bool, vs ...MetricView) template.HTML {
	vs = withSeries(vs)
	if len(vs) == 0 {
		return chart.Line{Label: label, Size: chart.Half, Empty: "No days to draw in this window."}.HTML()
	}
	c := chart.Line{Label: label, Size: chart.Half, XName: "Day", X: xsOf(longest(vs)), Rate: rate, More: more,
		Empty: "No days to draw in this window.", NName: barName(vs[0].ID)}
	if rate {
		c.Fmt, c.Merge = l.Percent, chart.MergeRate
	} else {
		c.Fmt = unitFormat(vs[0].Def.Unit, l)
		c.Merge = chart.MergeMean
		if vs[0].Def.Unit == "ms" {
			c.Merge = chart.MergeMax
		}
	}
	for i, v := range vs {
		class, name := styleOf(v)
		s := chart.Series{Name: name, Class: class, Dashed: rate && i > 0, Band: rate && i == 0}
		for _, p := range v.M.Series {
			y := chart.Y{V: p.Value}
			if rate {
				y.Num, y.Den = countOf(p.Num), countOf(p.Den)
			}
			s.Y = append(s.Y, y)
		}
		c.Series = append(c.Series, s)
	}
	for _, p := range vs[0].M.Series {
		if rate {
			c.N = append(c.N, countOf(p.Den))
		} else {
			c.N = append(c.N, countOf(p.N))
		}
	}
	return c.HTML()
}

// latencyOf is the slowest 1 in 20 per day of each latency metric, with the
// limit past which recall's latency needs attention.
func latencyOf(l format.Locale, more string, vs ...MetricView) template.HTML {
	vs = withSeries(vs)
	c := chart.Line{Label: "Time added, slowest 1 in 20, per day", Size: chart.Half, XName: "Day", Fmt: l.Duration,
		Merge: chart.MergeMax, More: more, Empty: "No days to draw in this window.", NName: "prompts"}
	if len(vs) == 0 {
		return c.HTML()
	}
	c.X = xsOf(longest(vs))
	for _, v := range vs {
		class, name := styleOf(v)
		s := chart.Series{Name: name + ", slowest 1 in 20", Class: class}
		for _, p := range v.M.Series {
			y := p.P95
			if y == nil {
				y = p.Value
			}
			s.Y = append(s.Y, chart.Y{V: y})
		}
		c.Series = append(c.Series, s)
	}
	for _, p := range vs[0].M.Series {
		c.N = append(c.N, countOf(p.N))
	}
	if bad := latencyLimit(); bad != nil {
		c.Limits = []chart.Limit{{Label: l.Duration(*bad), V: *bad, Class: "thr"}}
	}
	return c.HTML()
}

// wordsOf names each key with name.
func wordsOf(keys []string, name func(string) string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = name(k)
	}
	return out
}

// partsOf stacks, per day, the parts of v's series in order (bottom first),
// each named by words and coloured by the class at its place; valued is the
// class whose count is printed above each bar.
func partsOf(l format.Locale, label, more string, v MetricView, order []string, words map[string]string, classes []string, valued string) template.HTML {
	ps := withoutPartialLead(v.M.Series)
	c := chart.Stack{Label: label, Size: chart.Half, XName: "Day", X: xsOf(ps), Valued: valued,
		Fmt: unitFormat(v.Def.Unit, l), More: more, Empty: "Nothing to draw in this window."}
	for i, k := range order {
		s := chart.Series{Name: words[k], Class: classes[i%len(classes)]}
		for _, p := range ps {
			n := float64(p.Parts[k])
			s.Y = append(s.Y, chart.Y{V: &n})
		}
		c.Parts = append(c.Parts, s)
	}
	return c.Bars()
}

// outcomeShapes are the outcome mix's parts, bottom first.
var outcomeShapes = []struct {
	shape, class string
	of           func(metrics.OutcomeDay) int64
}{
	{"used", "f-used", func(d metrics.OutcomeDay) int64 { return d.Used }},
	{"ignored", "f-ignored", func(d metrics.OutcomeDay) int64 { return d.Ignored }},
	{"undeclared", "f-undeclared", func(d metrics.OutcomeDay) int64 { return d.NotDeclared }},
	{"shown", "f-shown", func(d metrics.OutcomeDay) int64 { return d.ShownOnly }},
	{"pending", "f-pending", func(d metrics.OutcomeDay) int64 { return d.Pending }},
}

// outcomesOf stacks what became of the memories shown, per day.
func outcomesOf(l format.Locale, more string, rows []metrics.OutcomeDay) template.HTML {
	return outcomeBars(l, "What Claude did with the memories shown, per day", more, rows)
}

func outcomeBars(l format.Locale, label, more string, rows []metrics.OutcomeDay) template.HTML {
	if len(rows) > 1 && rows[0].Partial {
		rows = rows[1:]
	}
	c := chart.Stack{Label: label, Size: chart.Half, XName: "Day", Hatch: "f-pending",
		Fmt: func(x float64) string { return l.Int(int64(math.Round(x))) }, More: more, Empty: "Nothing shown in this window."}
	for _, r := range rows {
		c.X = append(c.X, xName(r.Day))
	}
	for _, sh := range outcomeShapes {
		s := chart.Series{Name: chart.ShapeWords[sh.shape], Class: sh.class}
		for _, r := range rows {
			n := float64(sh.of(r))
			s.Y = append(s.Y, chart.Y{V: &n})
		}
		c.Parts = append(c.Parts, s)
	}
	return c.Bars()
}

// PathChart is a chart named by a recall path.
type PathChart struct {
	Name string
	HTML template.HTML
}

// outcomePaths is the outcome mix of each path the rows hold, by path name.
func outcomePaths(l format.Locale, more string, rows []metrics.OutcomeDay) []PathChart {
	seen := map[string]bool{}
	for _, r := range rows {
		for p := range r.ByPath {
			seen[p] = true
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	var out []PathChart
	for _, p := range paths {
		per := make([]metrics.OutcomeDay, len(rows))
		for i, r := range rows {
			per[i] = r.ByPath[p]
			per[i].Day, per[i].Partial = r.Day, r.Partial
		}
		word := views.Word(views.PathWords, p)
		out = append(out, PathChart{Name: word, HTML: outcomeBars(l, "What Claude did with the memories shown "+word+", per day", more, per)})
	}
	return out
}

// histBin is a bar of the metrics package's context histogram; Hi is null
// for the last, open one.
type histBin struct {
	Lo int64  `json:"lo"`
	Hi *int64 `json:"hi"`
	N  int64  `json:"n"`
}

// thousands writes a number of characters in thousands: "2 k".
func thousands(l format.Locale, v int64) string { return l.Fixed(float64(v)/1000, 0) + " k" }

// contextHistOf is the sessions by the context they got, with the typical
// and the high value marked.
func contextHistOf(l format.Locale, more string, v MetricView) template.HTML {
	c := chart.Hist{Label: "Sessions by characters of context added", Size: chart.Half, XName: "Characters added in a session",
		Class: "f-seq1", Unit: "sessions", FromLabels: true, More: more, Empty: "No session got context in this window.",
		Fmt: func(x float64) string { return l.Int(int64(math.Round(x))) }}
	bins, _ := extraAs[[]histBin](v.M, "histogram")
	var width int64
	for _, b := range bins {
		label := thousands(l, b.Lo) + "+"
		if b.Hi != nil {
			label = thousands(l, b.Lo) + "–" + thousands(l, *b.Hi)
			if width == 0 {
				width = *b.Hi - b.Lo
			}
		}
		c.Bins = append(c.Bins, chart.Bin{Label: label, N: b.N})
	}
	if width > 0 {
		at := func(x float64) float64 { return x / float64(width) }
		if p50, ok := extraAs[float64](v.M, "p50"); ok {
			c.Markers = append(c.Markers, chart.Marker{Label: "typical " + l.Size(p50, "chars"), At: at(p50), Class: "mk"})
		}
		if p95, ok := extraAs[float64](v.M, "p95"); ok {
			c.Markers = append(c.Markers, chart.Marker{Label: "high " + l.Size(p95, "chars"), At: at(p95), Class: "thr-w"})
		}
	}
	return c.HTML()
}

// rankKeys are the bars of the rank histogram.
var rankKeys = []string{"1", "2", "3", "4", "5", "6+", "unranked"}

// rankHistOf is the turns by the best rank among the memories used.
func rankHistOf(l format.Locale, more string, v MetricView) template.HTML {
	counts, _ := extraAs[map[string]int64](v.M, "histogram")
	c := chart.Hist{Label: "Turns by the best rank among the memories used", Size: chart.Half, XName: "Rank",
		Class: "f-seq1", Unit: "turns", More: more, Empty: "No turn used a ranked memory in this window.",
		Fmt: func(x float64) string { return l.Int(int64(math.Round(x))) }}
	for _, k := range rankKeys {
		label := k
		if k == "unranked" {
			label = "not ranked"
		}
		c.Bins = append(c.Bins, chart.Bin{Label: label, N: counts[k]})
	}
	return c.HTML()
}

// extractionOf is the server's model tokens per day, by task: the first
// three tasks seen keep their own colour, the rest (and "other") share one.
func extractionOf(l format.Locale, more string, rows []metrics.ExtractionRow) template.HTML {
	var days, ops []string
	tokens := map[[2]string]float64{}
	for _, r := range rows {
		if !slices.Contains(days, r.Day) {
			days = append(days, r.Day)
		}
		op := r.Operation
		if op != "other" && !slices.Contains(ops, op) {
			ops = append(ops, op)
		}
		tokens[[2]string{r.Day, op}] += float64(r.Input + r.Output)
	}
	c := chart.Stack{Label: "Tokens the server's model used, per day, by task", Size: chart.Half, XName: "Day",
		Fmt: unitFormat("tokens", l), More: more, Empty: "The server's model used no tokens in this window."}
	for _, d := range days {
		c.X = append(c.X, xName(d))
	}
	classes := []string{"f-seq1", "f-seq2", "f-seq3"}
	named := ops[:min(len(ops), len(classes))]
	part := func(name, class string, of func(day string) float64) {
		s := chart.Series{Name: name, Class: class}
		for _, d := range days {
			v := of(d)
			s.Y = append(s.Y, chart.Y{V: &v})
		}
		c.Parts = append(c.Parts, s)
	}
	for i, op := range named {
		part(views.Word(views.OperationWords, op), classes[i], func(d string) float64 { return tokens[[2]string{d, op}] })
	}
	if len(ops) > len(named) || hasOther(rows) {
		rest := ops[len(named):]
		part(views.Word(views.OperationWords, "other"), "f-neutral", func(d string) float64 {
			v := tokens[[2]string{d, "other"}]
			for _, op := range rest {
				v += tokens[[2]string{d, op}]
			}
			return v
		})
	}
	return c.Bars()
}

func hasOther(rows []metrics.ExtractionRow) bool {
	return slices.ContainsFunc(rows, func(r metrics.ExtractionRow) bool { return r.Operation == "other" })
}
