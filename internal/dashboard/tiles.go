package dashboard

import (
	"cmp"
	"html/template"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// TileGroup is a titled group of tiles.
type TileGroup struct {
	Name  string
	Tiles []Tile
}

// Tile is one key number of the Overview.
type Tile struct {
	Name, Class   string // Class: is-few | is-watch | is-bad | is-unrel | is-learn | ""
	Question      string // the catalog's
	Head          Head   // level 3
	Big, Unit, CI string
	Sub, Note     string
	Word          string
	WordClass     string
	Delta         *format.Delta
	Spark         template.HTML
	Link          string
	LinkText      string
	Dev           string // "<id> · n <Den>" with developer mode
}

// tileSpec is what differs between the six tiles.
type tileSpec struct {
	group, name, id string
	axis            string // the verdict axis whose learning turns the tile to learning; "" none
	one, many       string // what the metric counts
	view, has       string // the command the tile shows
}

var tileSpecs = []tileSpec{
	{"Found", "Found", "recall.hit_rate", "found", "prompt", "prompts", "quality", ""},
	{"Right", "Used", "used.turn_rate", "right", "turn", "turns", "sessions", "used"},
	{"Right", "Reported wrong", "verdict.wrong_rate", "", "verdict", "verdicts", "sessions", "wrong"},
	{"Cost", "Context", "context.per_session", "", "session", "sessions", "cost", ""},
	{"Cost", "Speed", "recall.latency", "", "prompt", "prompts", "cost", ""},
	{"Flowing", "Flowing", "pipeline.flow", "", "session", "sessions", "pipeline", ""},
}

// tilesOf are the Overview's six tiles in their four groups, times read in
// loc; dev adds each tile's metric id and sample size.
func tilesOf(r *metrics.Result, p views.Params, l format.Locale, loc *time.Location, dev bool) []TileGroup {
	var groups []TileGroup
	for _, s := range tileSpecs {
		t := tileOf(r, s, p, l, loc, dev)
		if n := len(groups); n > 0 && groups[n-1].Name == s.group {
			groups[n-1].Tiles = append(groups[n-1].Tiles, t)
		} else {
			groups = append(groups, TileGroup{Name: s.group, Tiles: []Tile{t}})
		}
	}
	return groups
}

// tileClass is the border class a status word's class asks for.
var tileClass = map[string]string{
	"st-few": "is-few", "st-watch": "is-watch", "st-bad": "is-bad", "st-unrel": "is-unrel", "st-learn": "is-learn",
}

func tileOf(r *metrics.Result, s tileSpec, p views.Params, l format.Locale, loc *time.Location, dev bool) Tile {
	v, _ := metricOf(r, s.id) // the ids are the catalog's
	m := v.M
	d := v.Def
	axisLearning := false
	if r.Verdict != nil && s.axis != "" {
		axisLearning = r.Verdict.Axes[s.axis].State == metrics.AxisLearning
	}
	t := Tile{Name: s.name, Question: d.Question}
	t.Word, t.WordClass = statusWord(d, m, axisLearning)
	t.Class = tileClass[t.WordClass]
	learning := t.Word == "learning"
	tooFew := m.Status == metrics.StatusTooFew && !learning
	cause := r.Verdict != nil && rootCause(r.Verdict)

	t.Head = tileHead(v, s, p, l)
	if dev {
		t.Dev = s.id
		if m.Den != nil {
			t.Dev += " · n " + l.Int(*m.Den)
		}
	}

	parts := l.ValueParts(d, m)
	t.Big, t.Unit, t.CI = parts.Big, parts.Unit, parts.CI
	num, den, haveND := numDen(m)
	switch {
	case learning:
		t.Big, t.Unit, t.CI = dash, "", ""
	case tooFew && haveND:
		t.Big, t.Unit, t.CI = l.Int(num)+" of "+l.Int(den), "", ""
	}
	if !learning && (!tooFew || !haveND) {
		if dl, ok := l.Delta(d, m); ok && (s.id != "pipeline.flow" || !cause) {
			t.Delta = &dl
		}
	}

	t.Link, t.LinkText = drillHref(m.Drill, p), "Events →"
	if m.Den != nil {
		t.LinkText = format.Count(l, *m.Den, s.one, s.many) + " →"
	}

	switch s.id {
	case "recall.hit_rate":
		t.Sub = foundSub(m, l, num, den, haveND, tooFew)
	case "used.turn_rate":
		usedTile(&t, r, l, m, learning)
	case "verdict.wrong_rate":
		t.Sub = wrongSub(l, num, den, haveND, tooFew)
	case "context.per_session":
		if m.Value == nil {
			break // nothing to qualify
		}
		if tokens, ok := extraAs[int64](m, "tokens_p95"); ok {
			t.CI = "≈ " + l.Int(tokens) + " tokens"
		}
		if p50, ok := extraAs[float64](m, "p50"); ok {
			t.Sub = "high · typical " + l.Size(p50, "chars")
		}
	case "recall.latency":
		if p50, ok := extraAs[float64](m, "p50"); ok && m.Value != nil {
			t.Sub = "slowest 1 in 20 · typical " + l.Duration(p50)
			if bad := latencyLimit(); bad != nil && (m.Status == metrics.StatusWatch || m.Status == metrics.StatusBad) {
				t.Sub += " · limit " + l.Duration(*bad)
			}
		}
	case "pipeline.flow":
		flowingTile(&t, r, l, p, m, cause)
	}

	t.Note = tileNote(r, s, l, loc, m, learning, tooFew, cause)
	t.Spark = sparkOf(v, s.name+" "+sparkSpan(r, v), l)
	return t
}

// numDen are the metric's counts; ok is false when it holds none.
func numDen(m metrics.Metric) (num, den int64, ok bool) {
	if m.Num == nil || m.Den == nil {
		return 0, 0, false
	}
	return *m.Num, *m.Den, true
}

func latencyLimit() *float64 {
	d, _ := metrics.DefOf("recall.latency")
	return d.Absolute.Bad
}

// tileHead is the frame of a tile: the metric's own ⓘ under the tile's name.
func tileHead(v MetricView, s tileSpec, p views.Params, l format.Locale) Head {
	q := p
	q.Has, q.Sort, q.State, q.Path, q.Limit, q.Offset = "", "", "", "", 0, 0
	// The tile's own filter comes first; the page's flags follow as cliFor writes them.
	cmd := "backsight stats " + s.view
	if s.has != "" {
		cmd += " --has " + s.has
	}
	cmd += strings.TrimPrefix(cliFor(s.view, "", q), "backsight stats "+s.view)
	h := headOf(v, cmd, l, "", p)
	h.Level = 3
	h.Drill, h.DrillText = "", ""
	h.Info.Label, h.Info.Name = "About "+s.name, s.name+": "+v.Def.Question
	return h
}

func foundSub(m metrics.Metric, l format.Locale, num, den int64, haveND, tooFew bool) string {
	switch {
	case tooFew:
		return "prompts got memory"
	case !haveND:
		return ""
	}
	s := l.Int(num) + " of " + format.Count(l, den, "prompt", "prompts")
	if (m.Status == metrics.StatusWatch || m.Status == metrics.StatusBad) && m.Previous != nil {
		s = "usual " + l.Percent(*m.Previous) + " · " + s
	}
	return s
}

func wrongSub(l format.Locale, num, den int64, haveND, tooFew bool) string {
	switch {
	case !haveND:
		return ""
	case den == 0:
		return "no memory judged yet"
	case tooFew:
		return l.Int(num) + " reported wrong of " + format.Count(l, den, "verdict", "verdicts")
	}
	return l.Int(num) + " of " + format.Count(l, den, "verdict", "verdicts")
}

// usedTile fills the parts of the Used tile that read declare.coverage.
func usedTile(t *Tile, r *metrics.Result, l format.Locale, m metrics.Metric, learning bool) {
	cov, haveCov := r.Metrics["declare.coverage"]
	if haveCov && cov.Den != nil {
		t.LinkText = format.Count(l, *cov.Den, "turn", "turns") + " →"
	}
	switch {
	case learning:
		if r.Verdict != nil {
			t.Sub = firstReason(r.Verdict.Axes["right"], "")
		}
	case haveCov && cov.Value != nil:
		t.Sub = "Said what it used on " + l.Percent(*cov.Value) + " of turns"
		if slices.Contains(m.Tags, "unreliable") && cov.Num != nil && cov.Den != nil {
			t.Sub += " (" + l.Int(*cov.Num) + " of " + l.Int(*cov.Den) + ")"
		}
		t.Sub += "."
	}
}

// flowingTile fills the Flowing tile: the backlog when the server is
// unreachable, the failed sessions when some failed, else the share sent.
func flowingTile(t *Tile, r *metrics.Result, l format.Locale, p views.Params, m metrics.Metric, cause bool) {
	if cause {
		bl := r.Metrics["local.backlog"]
		notes, ok1 := extraAs[int64](bl, "notes_queued")
		sessions, ok2 := extraAs[int64](bl, "sessions_waiting")
		if ok1 && ok2 {
			t.Big, t.Unit, t.CI = l.Int(notes+sessions), "waiting", ""
			t.Sub = format.Count(l, notes, "note", "notes") + " and " + format.Count(l, sessions, "session", "sessions") + " on this PC"
			// local.backlog's drill is the command that clears it, not a page
			// of events: the pipeline page lists what waits.
			t.Link, t.LinkText = cmp.Or(drillHref(bl.Drill, p), drillHref("backsight stats pipeline", p)), "Waiting items →"
			return
		}
	}
	num, den, haveND := numDen(m)
	if failed, ok := extraAs[int64](m, "failed"); ok && failed > 0 {
		t.Big, t.Unit, t.CI = l.Int(failed), "failed", ""
		var parts []string
		if aside, ok := extraAs[int64](m, "set_aside"); ok && aside > 0 {
			parts = append(parts, l.Int(aside)+" set aside")
		}
		if oldest, ok := extraAs[int64](m, "backlog_oldest_ms"); ok && oldest > 0 {
			parts = append(parts, "oldest waiting "+views.Age(oldest))
		}
		if haveND {
			parts = append(parts, l.Int(num)+" of "+l.Int(den)+" sent")
		}
		t.Sub = strings.Join(parts, " · ")
		return
	}
	if haveND {
		t.Sub = l.Int(num) + " of " + l.Int(den) + " sessions sent"
	}
}

// tileNote is the sentence under a tile: the first that applies.
func tileNote(r *metrics.Result, s tileSpec, l format.Locale, loc *time.Location, m metrics.Metric, learning, tooFew, cause bool) string {
	switch {
	case cause && s.id == "recall.hit_rate":
		return "Prompts skipped: server unreachable."
	case cause && s.id == "pipeline.flow":
		if since, ok := extraAs[time.Time](r.Metrics["server.reach"], "failing_since"); ok && !since.IsZero() {
			return "Memory server unreachable since " + format.Clock(r.GeneratedAt, since, loc) + ". Nothing is lost."
		}
		return "Memory server unreachable. Nothing is lost."
	}
	for _, it := range r.Attention {
		if s.id == "pipeline.flow" && it.Axis == "flowing" || it.Metric == s.id {
			return it.Text
		}
	}
	switch {
	case learning:
		return ""
	case m.Borderline:
		return "Borderline: the interval still reaches the usual level."
	case slices.Contains(m.Tags, "unreliable"):
		return "Unreliable: Claude often didn't say what it used."
	case tooFew && m.Den != nil && *m.Den == 0:
		return "Nothing judged yet."
	case tooFew && m.Num != nil && m.Den != nil:
		return l.Int(*m.Num) + " of " + l.Int(*m.Den) + ": too few to judge."
	case tooFew && m.Den != nil:
		return format.Count(l, *m.Den, s.one, s.many) + ": too few to judge."
	case slices.Contains(m.Tags, "judged_28d"):
		return judged28Note
	}
	return ""
}

// sparkSpan says what a tile's spark spans: the hours of a day, or the
// days of the window (7 when the result does not say).
func sparkSpan(r *metrics.Result, v MetricView) string {
	if r.Tables.Day != nil && len(v.M.Series) > 0 && v.M.Series[0].Hour != "" {
		return "per hour, that day"
	}
	days := r.Window.Days
	if days <= 0 {
		days = 7
	}
	return "per day, last " + strconv.Itoa(days) + " days"
}
