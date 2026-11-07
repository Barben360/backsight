package dashboard

import (
	"cmp"
	"html/template"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/machines"
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
	Approx        bool // the figure is a range from the PCs' buckets: its CI reads "approximate" in italics
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
// loc; dev adds each tile's metric id and sample size. On all PCs, own is this
// PC's answer to the same view (the tiles' links name its counts); nil, and
// without it they say "events" (a page that is always this PC's).
func tilesOf(r *metrics.Result, p views.Params, l format.Locale, loc *time.Location, dev bool, own *metrics.Result) []TileGroup {
	var groups []TileGroup
	for _, s := range tileSpecs {
		t := tileOf(r, s, p, l, loc, dev, own)
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

func tileOf(r *metrics.Result, s tileSpec, p views.Params, l format.Locale, loc *time.Location, dev bool, own *metrics.Result) Tile {
	v, _ := metricOf(r, s.id) // the ids are the catalog's
	m := v.M
	d := v.Def
	merged := mergedTile(r, s.id)
	approx := merged && slices.Contains(m.Tags, "approximate") // speed, from the PCs' buckets
	average := merged && slices.Contains(m.Tags, "average")    // context, total over sessions
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
	if merged {
		mergedHead(&t.Head, r, m)
	}
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
	case tooFew && haveND && !average: // the average's counts are characters over sessions
		t.Big, t.Unit, t.CI = l.Int(num)+" of "+l.Int(den), "", ""
	}
	if !learning && (!tooFew || !haveND) && !average { // the average's change would be this PC's high value's
		if approx {
			t.Delta = bucketDelta(r, m)
		} else if dl, ok := l.Delta(d, m); ok && (s.id != "pipeline.flow" || !cause) {
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
		if merged && !tooFew && haveND {
			if on, od, ok := ownCounts(own, s.id); ok {
				t.Sub += " · this PC " + l.Int(on) + " of " + l.Int(od)
			}
		}
	case "used.turn_rate":
		usedTile(&t, r, l, m, learning)
	case "verdict.wrong_rate":
		t.Sub = wrongSub(l, num, den, haveND, tooFew)
	case "context.per_session":
		if m.Value == nil {
			break // nothing to qualify
		}
		if average {
			t.CI = "≈ " + l.Int(int64(math.Round(*m.Value/4/10)*10)) + " tokens"
			t.Sub = "average per session, all PCs"
			if p95, ok := extraAs[float64](m, "p95"); ok {
				t.Sub += " · this PC’s high value " + l.Size(p95, "")
			}
			break
		}
		if tokens, ok := extraAs[int64](m, "tokens_p95"); ok {
			t.CI = "≈ " + l.Int(tokens) + " tokens"
		}
		if p50, ok := extraAs[float64](m, "p50"); ok {
			t.Sub = "high · typical " + l.Size(p50, "chars")
		}
	case "recall.latency":
		if approx {
			speedRange(&t, m, l)
			break
		}
		if p50, ok := extraAs[float64](m, "p50"); ok && m.Value != nil {
			t.Sub = "slowest 1 in 20 · typical " + l.Duration(p50)
			if bad := latencyLimit(); bad != nil && (m.Status == metrics.StatusWatch || m.Status == metrics.StatusBad) {
				t.Sub += " · limit " + l.Duration(*bad)
			}
		}
	case "pipeline.flow":
		flowingTile(&t, r, l, p, m, cause)
		recallSub(&t, r, l)
	}

	t.Note = tileNote(r, s, l, loc, m, learning, tooFew, cause, merged, own)
	if merged && (s.id != "pipeline.flow" || !cause) { // the waiting items are this PC's
		t.LinkText = thisPCLink(own, s, l)
	}
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
	// The design draws the flag on all six tiles, also the two whose command
	// lists sessions (a view that adds nothing up): it is the tile's own
	// number that adds the PCs up, and the flag says so.
	if p.Machines == "all" && metrics.MergedMetric(v.ID) && !strings.Contains(h.CLI, "--machines") {
		h.CLI += " --machines all"
	}
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
		t.Sub = ofCount(l, num, den, "session", "sessions") + " sent"
	}
}

// recallSub puts the time to recall before the Flowing tile's subline, and
// the sessions it comes from after its "Based on". It states the value only:
// the metric's watch shows on its own card, never on the tile. Too few
// sessions, no value: the subline stays as it was.
func recallSub(t *Tile, r *metrics.Result, l format.Locale) {
	m, ok := r.Metrics[timeToRecallID]
	if !ok || m.Value == nil {
		return
	}
	lead := "Session to recalled memory: typical " + longDuration(l, *m.Value)
	if t.Sub != "" {
		lead += " · " + t.Sub
	}
	t.Sub = lead
	if m.Num != nil {
		from := "time to recall from " + format.Count(l, *m.Num, "session", "sessions") + " recalled"
		if b := t.Head.Info.BasedOn; b != "" {
			t.Head.Info.BasedOn = b + "; " + from
		} else {
			t.Head.Info.BasedOn = strings.ToUpper(from[:1]) + from[1:]
		}
	}
}

// timeToRecallID is the metric whose typical value leads the Flowing subline.
const timeToRecallID = "pipeline.time_to_recall"

// longDuration is ms as Duration writes it below a day, and from a day on as
// days and hours ("1 d 2 h", "9 d"): a wait of days is not told in hours.
func longDuration(l format.Locale, ms float64) string {
	const day, hour = 24 * 3600 * 1000.0, 3600 * 1000.0
	if ms < day-hour/2 {
		return l.Duration(ms)
	}
	d := math.Floor(ms / day)
	h := math.Round((ms - d*day) / hour)
	if h >= 24 {
		d, h = d+1, 0
	}
	s := l.Int(int64(d)) + " d"
	if h > 0 {
		s += " " + l.Int(int64(h)) + " h"
	}
	return s
}

// tileNote is the sentence under a tile: the first that applies.
func tileNote(r *metrics.Result, s tileSpec, l format.Locale, loc *time.Location, m metrics.Metric, learning, tooFew, cause, merged bool, own *metrics.Result) string {
	switch {
	case cause && s.id == "recall.hit_rate":
		return "Prompts skipped: server unreachable."
	case cause && s.id == "pipeline.flow":
		if since, ok := extraAs[time.Time](r.Metrics["server.reach"], "failing_since"); ok && !since.IsZero() {
			return "Memory server unreachable since " + format.Clock(r.GeneratedAt, since, loc) + ". Nothing is lost."
		}
		return "Memory server unreachable. Nothing is lost."
	}
	// The items are this PC's: a merged figure that is not judged on this PC's
	// own status leaves them to "What should I do?".
	thisPC := !merged || slices.Contains(m.Tags, "status_this_pc")
	for _, it := range r.Attention {
		if thisPC && (s.id == "pipeline.flow" && it.Axis == "flowing" || it.Metric == s.id) {
			return it.Text
		}
	}
	if merged {
		if n, ok := mergedNote(s, l, m, tooFew, own); ok {
			return n
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

// mergedTile says the tile of metric id adds up the PCs: the result is
// all PCs' and the metric one the views merge (metrics.MergedMetric).
func mergedTile(r *metrics.Result, id string) bool {
	return r.Scope.Machines == "all" && r.Scope.PCs != nil && metrics.MergedMetric(id)
}

// mergedHead gives a merged tile's frame the chip "all PCs (N)", the note
// that a project filter spares the other PCs, and, when some PCs had no
// activity in the window, how many the figure rests on.
func mergedHead(h *Head, r *metrics.Result, m metrics.Metric) {
	h.PCs = allPCsChip(r.Scope)
	h.OthersNF = othersNotFiltered(m)
	pcs := r.Scope.PCs
	if b := h.Info.BasedOn; b != "" && b != dash && pcs.Active > 0 && pcs.Active < pcs.Count {
		when := "in this window"
		if r.Window.Days == 7 {
			when = "this week"
		}
		h.Info.BasedOn = b + " on the " + plural(pcs.Active, "PC", "PCs") + " with activity " + when
	}
}

// ownCounts are this PC's own counts of metric id in own, its answer to the
// same view; ok is false without them.
func ownCounts(own *metrics.Result, id string) (num, den int64, ok bool) {
	if own == nil {
		return 0, 0, false
	}
	m, found := own.Metrics[id]
	if !found {
		return 0, 0, false
	}
	return numDen(m)
}

// thisPCLink is the link text of a merged tile: the events behind it are this
// PC's, so it names this PC's count ("This PC’s 167 prompts →").
func thisPCLink(own *metrics.Result, s tileSpec, l format.Locale) string {
	id := s.id
	if id == "used.turn_rate" {
		id = "declare.coverage" // the Used tile's link counts the turns Claude declared
	}
	if own != nil {
		if m, ok := own.Metrics[id]; ok && m.Den != nil {
			return "This PC’s " + format.Count(l, *m.Den, s.one, s.many) + " →"
		}
	}
	return "This PC’s events →"
}

// speedRange writes the merged Speed tile: the bucket that holds the slowest
// 1 in 20 as a range, labelled approximate, and the typical one.
func speedRange(t *Tile, m metrics.Metric, l format.Locale) {
	if lo, hi, ok := views.RangeMS(m, "range_ms"); ok {
		text := views.RangeText(lo, hi)
		if i := strings.LastIndex(text, " "); i > 0 {
			t.Big, t.Unit = text[:i], text[i+1:]
		}
		t.CI, t.Approx = "approximate", true
	}
	if m.Value == nil {
		return
	}
	t.Sub = "slowest 1 in 20, from the PCs’ time buckets"
	if lo, hi, ok := views.RangeMS(m, "p50_range_ms"); ok {
		t.Sub += " · typical " + views.RangeText(lo, hi)
	}
	if bad := latencyLimit(); bad != nil && (m.Status == metrics.StatusWatch || m.Status == metrics.StatusBad) {
		t.Sub += " · limit " + l.Duration(*bad)
	}
}

// bucketDelta is the change of the merged Speed against the window before,
// in buckets: the value is a bucket's upper edge, so a delta of a few ms
// would claim a precision the buckets do not hold. Nil without a change to show.
func bucketDelta(r *metrics.Result, m metrics.Metric) *format.Delta {
	if m.Delta == nil || m.Value == nil || m.Status == metrics.StatusNoData || m.Status == metrics.StatusTooFew {
		return nil
	}
	window := "window"
	if r.Window.Days == 7 {
		window = "week"
	}
	if *m.Delta == 0 {
		return &format.Delta{Arrow: "=", Text: "same bucket as the " + window + " before"}
	}
	steps := 1
	if m.Previous != nil {
		// The value is the bucket's upper edge, clamped for the open bucket:
		// the bucket itself is read from its range.
		cur := machines.Bucket(int64(*m.Value))
		if lo, _, ok := views.RangeMS(m, "range_ms"); ok {
			cur = machines.Bucket(lo + 1)
		}
		steps = max(cur-machines.Bucket(int64(*m.Previous)), machines.Bucket(int64(*m.Previous))-cur, 1)
	}
	word := map[bool]string{true: "one bucket", false: strconv.Itoa(steps) + " buckets"}[steps == 1]
	if *m.Delta > 0 {
		return &format.Delta{Arrow: "▲", Text: word + " slower, worse", Class: "worse"}
	}
	return &format.Delta{Arrow: "▼", Text: word + " faster, better", Class: "better"}
}

// mergedNote is the sentence a merged tile says about what adding up the PCs
// changed; ok is false when the tile has none.
func mergedNote(s tileSpec, l format.Locale, m metrics.Metric, tooFew bool, own *metrics.Result) (string, bool) {
	switch s.id {
	case "recall.latency":
		if !slices.Contains(m.Tags, "approximate") || m.Value == nil {
			return "", false
		}
		d, _ := metrics.DefOf("recall.latency")
		switch {
		case m.Status == metrics.StatusBad && d.Absolute.Bad != nil:
			return "Over " + l.Duration(*d.Absolute.Bad) + " for sure: the limits sit on bucket edges, so the status is exact even though the value is a range.", true
		case m.Status == metrics.StatusWatch && d.Absolute.Watch != nil:
			return "Over " + l.Duration(*d.Absolute.Watch) + " for sure: the limits sit on bucket edges, so the status is exact even though the value is a range.", true
		}
		return "A range, not a value: other PCs send their times in buckets (… 1 s, 2 s, 2.5 s, 5 s).", true
	case "context.per_session":
		if slices.Contains(m.Tags, "average") {
			return "Other PCs send totals, not sizes: the status is judged on this PC’s high value.", true
		}
	case "verdict.wrong_rate":
		if own == nil || tooFew {
			return "", false
		}
		if om, ok := own.Metrics[s.id]; ok && om.Status == metrics.StatusTooFew {
			d, _ := metrics.DefOf(s.id)
			return "Enough verdicts once the PCs are added up (" + l.Int(int64(d.MinN)) + " needed).", true
		}
	}
	return "", false
}
