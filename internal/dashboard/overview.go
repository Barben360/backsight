package dashboard

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// Banner is the verdict banner of the Overview.
type Banner struct {
	Title, Class, Icon string
	Axes               []AxisRow
	Reasons            []ReasonRow
	Head               Head
}

// AxisRow is one axis of the banner: its name, state word and note.
type AxisRow struct{ Name, Word, Class, Note string }

// ReasonRow is a reason or an attention item: the pill, the sentence and
// the command that helps.
type ReasonRow struct{ Word, Class, Text, Command string }

const (
	judged28Note = "Judged on the last 28 days: too few turns in 7."
	maxReasons   = 3
)

// learningDay cuts "day 2 of about 7" from the learning sentence.
var learningDay = regexp.MustCompile(`^Learning: (day \d+ of about 7),`)

// extraAs reads the metric's extra key as a T, through JSON: in process an
// extra is the metrics package's own type, in a golden a decoded map. It
// is false when the key is absent or null, or holds another shape.
func extraAs[T any](m metrics.Metric, key string) (T, bool) {
	var out T
	v, ok := m.Extra[key]
	if !ok || v == nil {
		return out, false
	}
	raw, err := json.Marshal(v)
	if err != nil || string(raw) == "null" || json.Unmarshal(raw, &out) != nil {
		var zero T
		return zero, false
	}
	return out, true
}

// axisWord is the word and class an axis state is shown as.
func axisWord(ax metrics.Axis) (word, class string) {
	switch ax.State {
	case metrics.AxisOK:
		return "ok", "st-ok"
	case metrics.AxisAttention:
		if hasBad(ax) {
			return "attention", "st-bad"
		}
		return "attention", "st-watch"
	case metrics.AxisTooEarly:
		return "too few", "st-few"
	case metrics.AxisCantJudge:
		return "can't judge", "st-cant"
	case metrics.AxisLearning:
		return "learning", "st-learn"
	}
	return string(ax.State), "st-few"
}

func hasBad(ax metrics.Axis) bool {
	return slices.ContainsFunc(ax.Reasons, func(r metrics.Reason) bool { return r.Severity == metrics.SeverityBad })
}

// hasMetric says whether one of the axis's reasons is about metric id.
func hasMetric(ax metrics.Axis, id string) bool {
	return slices.ContainsFunc(ax.Reasons, func(r metrics.Reason) bool { return r.Metric == id })
}

// rootCause says whether the server being unreachable is why Flowing is in
// attention.
func rootCause(v *metrics.Verdict) bool {
	ax, ok := v.Axes["flowing"]
	return ok && ax.State == metrics.AxisAttention && hasMetric(ax, "server.reach")
}

// pillOf is the pill of a reason or an item of severity sev.
func pillOf(sev metrics.Severity) (word, class string) {
	if sev == metrics.SeverityBad {
		return "attention", "st-bad"
	}
	return "watch", "st-watch"
}

// bannerCLI is the command that prints the banner's numbers.
func bannerCLI(p views.Params) string {
	s := "backsight stats overview --days 7"
	if p.Project != "" {
		s += " --project " + quoteArg(p.Project)
	}
	return s
}

// bannerHead is the banner's frame, with its ⓘ.
func bannerHead(title string, p views.Params) Head {
	h := withInfo(fixedHead("verdict", title, "machine", bannerCLI(p)),
		"How the verdict is judged", "How the verdict is judged", "",
		"Always the last 7 days against the 7 before, whatever window is selected. Per axis, the first rule that applies wins: learning, not enough yet, can't judge, attention, ok.",
		"An axis turns to attention when a number stays at watch two weeks in a row, or is bad once. Flowing reacts at once.")
	h.Info.Href, h.Info.HrefText = "/defs", "All definitions"
	return h
}

// bannerOf is the verdict banner of the overview r, its times read in loc.
func bannerOf(r *metrics.Result, l format.Locale, loc *time.Location, p views.Params) Banner {
	v := r.Verdict
	if v == nil {
		b := Banner{Title: "Nothing to judge yet.", Class: "v-cant", Icon: "st-cant"}
		b.Head = bannerHead(b.Title, p)
		return b
	}
	var b Banner
	cause := rootCause(v)
	for _, an := range views.AxisNames {
		ax, ok := v.Axes[an.Key]
		if !ok {
			continue
		}
		w, c := axisWord(ax)
		b.Axes = append(b.Axes, AxisRow{Name: an.Name, Word: w, Class: c, Note: axisNote(r, l, an.Key, ax, cause)})
	}

	// The headline axis: the first in attention, else the first that cannot
	// be judged, is too early, or is learning.
	var name string
	var head metrics.Axis
	var found bool
	for _, states := range [][]metrics.AxisState{{metrics.AxisAttention}, {metrics.AxisCantJudge}, {metrics.AxisTooEarly}, {metrics.AxisLearning}} {
		for _, an := range views.AxisNames {
			if ax, ok := v.Axes[an.Key]; ok && ax.State == states[0] {
				name, head, found = an.Name, ax, true
				break
			}
		}
		if found {
			break
		}
	}
	switch {
	case !found:
		b.Title, b.Class, b.Icon = "Memory is working.", "v-ok", "st-ok"
	case head.State == metrics.AxisAttention:
		b.Title = attentionTitle(r, l, loc, name, head)
		if hasBad(head) {
			b.Class, b.Icon = "v-bad", "st-bad"
		} else {
			b.Class, b.Icon = "v-watch", "st-watch"
		}
	case head.State == metrics.AxisCantJudge:
		b.Title, b.Class, b.Icon = name+": can't judge yet.", "v-cant", "st-cant"
	case head.State == metrics.AxisTooEarly:
		b.Title, b.Class, b.Icon = name+": not enough yet.", "v-cant", "st-few"
	default:
		b.Title, b.Class, b.Icon = firstReason(head, name+": learning."), "v-learn", "st-learn"
	}

	for _, state := range []metrics.AxisState{metrics.AxisAttention, metrics.AxisCantJudge} {
		for _, an := range views.AxisNames {
			ax, ok := v.Axes[an.Key]
			if !ok || ax.State != state {
				continue
			}
			for _, rs := range ax.Reasons {
				if len(b.Reasons) == maxReasons {
					break
				}
				w, c := pillOf(rs.Severity)
				b.Reasons = append(b.Reasons, ReasonRow{Word: w, Class: c, Text: rs.Text, Command: rs.Command})
			}
		}
	}
	b.Head = bannerHead(b.Title, p)
	return b
}

// firstReason is the text of the axis's first reason, else none.
func firstReason(ax metrics.Axis, none string) string {
	if len(ax.Reasons) == 0 {
		return none
	}
	return ax.Reasons[0].Text
}

// attentionTitle is the title for an axis in attention.
func attentionTitle(r *metrics.Result, l format.Locale, loc *time.Location, name string, ax metrics.Axis) string {
	switch {
	case name == "Flowing" && hasMetric(ax, "server.reach"):
		if since, ok := extraAs[time.Time](r.Metrics["server.reach"], "failing_since"); ok && !since.IsZero() {
			return "Flowing: memory server unreachable since " + format.Clock(r.GeneratedAt, since, loc) + ", nothing is lost."
		}
		return "Flowing: memory server unreachable, nothing is lost."
	case len(ax.Reasons) == 1 && ax.Reasons[0].Metric == "recall.latency":
		if m := r.Metrics["recall.latency"]; m.Value != nil {
			return "Cost needs attention: slowest prompts take " + l.Duration(*m.Value) + "."
		}
	case len(ax.Reasons) > 1:
		return name + " needs attention (" + strconv.Itoa(len(ax.Reasons)) + " reasons)."
	}
	return name + " needs attention."
}

// axisNote is the line under an axis.
func axisNote(r *metrics.Result, l format.Locale, key string, ax metrics.Axis, cause bool) string {
	first := firstReason(ax, dash)
	switch ax.State {
	case metrics.AxisLearning:
		if m := learningDay.FindStringSubmatch(first); m != nil {
			return m[1]
		}
		return first
	case metrics.AxisTooEarly:
		return first
	}
	var note string
	var ok bool
	switch key {
	case "found":
		note, ok = foundNote(r, l, ax, cause)
	case "right":
		note, ok = rightNote(r, l, ax)
	case "cost":
		note, ok = costNote(r, l, ax)
	case "flowing":
		note, ok = flowingNote(r, l, ax, cause)
	}
	if !ok {
		return first
	}
	return note
}

func foundNote(r *metrics.Result, l format.Locale, ax metrics.Axis, cause bool) (string, bool) {
	m := r.Metrics["recall.hit_rate"]
	if m.Value == nil {
		return "", false
	}
	pct := l.Percent(*m.Value)
	if ax.State == metrics.AxisAttention && m.Previous != nil {
		s := pct + " vs " + l.Percent(*m.Previous) + " usual"
		if !hasBad(ax) {
			s += ", two weeks running"
		}
		return s, true
	}
	if cause {
		return pct + ": prompts skipped while the server is unreachable", true
	}
	return pct + " of prompts got memory", true
}

func rightNote(r *metrics.Result, l format.Locale, ax metrics.Axis) (string, bool) {
	if ax.State == metrics.AxisCantJudge {
		c := r.Metrics["declare.coverage"]
		if c.Num == nil || c.Den == nil {
			return "", false
		}
		return "Claude said what it used on " + l.Int(*c.Num) + " of " + l.Int(*c.Den) + " turns", true
	}
	m := r.Metrics["used.turn_rate"]
	if slices.Contains(m.Tags, "judged_28d") {
		return judged28Note, true
	}
	if m.Value == nil {
		return "", false
	}
	s := l.Percent(*m.Value) + " used"
	if m.Borderline {
		s += ", borderline"
	}
	return s, true
}

func costNote(r *metrics.Result, l format.Locale, ax metrics.Axis) (string, bool) {
	m := r.Metrics["recall.latency"]
	if m.Value == nil {
		return "", false
	}
	if d, _ := metrics.DefOf("recall.latency"); ax.State == metrics.AxisAttention && hasMetric(ax, "recall.latency") && d.Absolute.Bad != nil {
		return "slowest 1 in 20: " + l.Duration(*m.Value) + ", limit " + l.Duration(*d.Absolute.Bad), true
	}
	return "slowest prompts " + l.Duration(*m.Value), true
}

func flowingNote(r *metrics.Result, l format.Locale, ax metrics.Axis, cause bool) (string, bool) {
	flow := r.Metrics["pipeline.flow"]
	if cause {
		notes, ok1 := extraAs[int64](r.Metrics["local.backlog"], "notes_queued")
		sessions, ok2 := extraAs[int64](r.Metrics["local.backlog"], "sessions_waiting")
		if !ok1 || !ok2 {
			return "", false
		}
		return format.Count(l, notes, "note", "notes") + " and " + format.Count(l, sessions, "session", "sessions") + " waiting", true
	}
	if failed, ok := extraAs[int64](flow, "failed"); ok && failed > 0 && ax.State == metrics.AxisAttention {
		s := l.Int(failed) + " failed"
		if oldest, ok := extraAs[int64](flow, "backlog_oldest_ms"); ok && oldest > 0 {
			s += ", oldest waiting " + views.Age(oldest)
		}
		return s, true
	}
	if flow.Num == nil || flow.Den == nil {
		return "", false
	}
	return l.Int(*flow.Num) + " of " + l.Int(*flow.Den) + " sessions sent", true
}

// SummaryLine is the figures of the line under the banner.
type SummaryLine struct{ Prompts, Turns, Tokens, Cost, Scope string }

// summaryOf is the summary line of r, which holds a Summary.
func summaryOf(r *metrics.Result, l format.Locale) SummaryLine {
	var s metrics.Summary
	if r.Summary != nil {
		s = *r.Summary
	}
	down := r.Server != nil && !r.Server.OK
	out := SummaryLine{
		Prompts: l.Int(s.PromptsWithMemory), Turns: l.Int(s.TurnsWithMemoryUsed),
		Tokens: l.Size(float64(s.TokensApprox), ""), Scope: "(server, all PCs).",
	}
	switch {
	case !s.PriceSet:
		out.Cost = "price not set"
	case s.ExtractionCost == nil:
		out.Cost = "unknown"
	default:
		out.Cost = l.Money(*s.ExtractionCost, s.Currency)
	}
	if down {
		out.Scope = "(the memory server is not answering)."
	}
	return out
}

// AttentionCard is the "What should I do?" card: the items with their
// reasons, or the calm sentence.
type AttentionCard struct {
	Head           Head
	Count          string
	Items          []ReasonRow
	Whys           []string // Whys[i] is why Items[i] matters
	Calm           string
	More, MoreHref string
}

// attentionOf is the attention card of r; the page sets the command.
func attentionOf(r *metrics.Result, l format.Locale) AttentionCard {
	c := AttentionCard{Head: fixedHead("attention", "What should I do?", "machine", "")}
	for _, it := range r.Attention {
		w, cl := pillOf(it.Severity)
		c.Items = append(c.Items, ReasonRow{Word: w, Class: cl, Text: it.Text, Command: it.Command})
		c.Whys = append(c.Whys, it.Why)
	}
	switch {
	case len(c.Items) > 0:
		c.Count = format.Count(l, int64(len(c.Items)), "item", "items")
		c.Head.Aside = c.Count
	case r.Verdict != nil && r.Verdict.Learning:
		c.Calm = "Memory is still learning: the verdict firms up after about a week of prompts."
	default:
		c.Calm = "Everything is within its usual range. Nothing to do."
	}
	if r.AttentionMore > 0 {
		c.More, c.MoreHref = l.Int(int64(r.AttentionMore))+" more on the System page →", "/system"
	}
	return c
}

// attentionPage is attentionOf with the page's command on its CLI.
func attentionPage(r *metrics.Result, l format.Locale, cli string) AttentionCard {
	c := attentionOf(r, l)
	c.Head.CLI = strings.TrimSpace(cli)
	return c
}

// summaryPage is the summary line of r, nil when r holds no summary.
func summaryPage(r *metrics.Result, l format.Locale) *SummaryLine {
	if r.Summary == nil {
		return nil
	}
	s := summaryOf(r, l)
	return &s
}
