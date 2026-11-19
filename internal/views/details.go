package views

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
)

// PricesHint says how to get a cost: it needs the model's prices.
const PricesHint = "costs need prices, set price_input, price_cached and price_output under [llm] in the config file"

// Reading the extras, whose types are those of the computing code.

// FlowKinds names what pipeline.flow's items are ("35 from sessions, 160
// notes, 80 subagent reports"), the kinds with none left out; "" when the
// metric does not say or holds none.
func FlowKinds(m metrics.Metric) string {
	var parts []string
	for _, k := range []struct{ key, one, many string }{
		{"sessions", "from a session", "from sessions"}, {"notes", "note", "notes"}, {"subagent_reports", "subagent report", "subagent reports"},
	} {
		if n, _ := extraInt(m, k.key); n > 0 {
			if n == 1 {
				parts = append(parts, "1 "+k.one)
			} else {
				parts = append(parts, Grouped(n)+" "+k.many)
			}
		}
	}
	return strings.Join(parts, ", ")
}

func extraInt(m metrics.Metric, key string) (int64, bool) {
	switch v := m.Extra[key].(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	}
	return 0, false
}

func extraFloat(m metrics.Metric, key string) (float64, bool) {
	switch v := m.Extra[key].(type) {
	case float64:
		return v, true
	case *float64:
		if v != nil {
			return *v, true
		}
	case int64:
		return float64(v), true
	}
	return 0, false
}

func extraTime(m metrics.Metric, key string, loc *time.Location) (string, bool) {
	var t time.Time
	switch v := m.Extra[key].(type) {
	case time.Time:
		t = v
	case *time.Time:
		if v == nil {
			return "", false
		}
		t = *v
	default:
		return "", false
	}
	if t.IsZero() {
		return "", false
	}
	return When(t, loc), true
}

// extraCounts reads a map of counts, sorted by key.
func extraCounts(m metrics.Metric, key string, words map[string]string, count func(int64) string) []string {
	var out []string
	switch v := m.Extra[key].(type) {
	case map[string]int64:
		for k, n := range v {
			if n > 0 {
				out = append(out, Word(words, k)+" "+count(n))
			}
		}
	case map[string]any:
		for k, x := range v {
			if n, ok := x.(int64); ok && n > 0 {
				out = append(out, Word(words, k)+" "+count(n))
			}
		}
	}
	slices.Sort(out)
	return out
}

// Numbers writes the figures of the detail lines: bucket ranges, values in a
// unit, counts and percents. A field left nil writes the command line's
// (RangeText, ValueIn, Grouped, Percent), so a page can follow its locale.
type Numbers struct {
	Range   func(lo int64, hi *int64) string
	Value   func(unit string, v float64, m metrics.Metric) string
	Count   func(n int64) string
	Percent func(r float64) string // the number, without the %
}

func (n Numbers) orDefaults() Numbers {
	if n.Range == nil {
		n.Range = RangeText
	}
	if n.Value == nil {
		n.Value = ValueIn
	}
	if n.Count == nil {
		n.Count = Grouped
	}
	if n.Percent == nil {
		n.Percent = Percent
	}
	return n
}

// countOf is CountOf with n written by n's Count.
func (n Numbers) countOf(c int64, one, many string) string {
	if c == 1 {
		return "1 " + one
	}
	return n.Count(c) + " " + many
}

// MetricDetails are the indented lines under a metric: what the number does
// not say by itself (a lower bound, a partial total, a split).
func MetricDetails(d metrics.Def, m metrics.Metric, loc *time.Location) []string {
	return details(d, m, loc, false, Numbers{})
}

// PageDetails are the same lines for a page, which names the skip reasons
// and the hook failure kinds in words.
func PageDetails(d metrics.Def, m metrics.Metric, loc *time.Location) []string {
	return details(d, m, loc, true, Numbers{})
}

// PageDetailsIn is PageDetails with the figures written by nf, so a page can
// follow its locale; the command line keeps its own.
func PageDetailsIn(d metrics.Def, m metrics.Metric, loc *time.Location, nf Numbers) []string {
	return details(d, m, loc, true, nf)
}

func details(d metrics.Def, m metrics.Metric, loc *time.Location, page bool, nf Numbers) []string {
	nf = nf.orDefaults()
	var skipWords, failureWords map[string]string
	if page {
		skipWords, failureWords = SkipWords, FailureKindWords
	}
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	switch d.ID {
	case "recall.empty_reasons":
		if e := extraCounts(m, "empty", nil, nf.Count); len(e) > 0 {
			add("why: %s", EmptyWords.Replace(strings.Join(e, ", ")))
		}
		if s := extraCounts(m, "skips", skipWords, nf.Count); len(s) > 0 {
			add("prompts skipped: %s", strings.Join(s, ", "))
		}
	case "used.turn_rate":
		if w, ok := extraFloat(m, "worst_case"); ok {
			add("worst case, if every turn where Claude did not say counts as unused: %s %%", nf.Percent(w))
		}
		if p, _ := extraInt(m, "pending"); p > 0 {
			add("%s still open", nf.countOf(p, "turn", "turns"))
		}
	case "context.per_session", "context.per_prompt", "recall.latency", "search.latency":
		p50, ok50 := extraFloat(m, "p50")
		p95, ok95 := extraFloat(m, "p95")
		merged := slices.Contains(m.Tags, "approximate") || slices.Contains(m.Tags, "average")
		if lo, hi, ok := RangeMS(m, "p50_range_ms"); ok && slices.Contains(m.Tags, "approximate") {
			add("all PCs: typical %s", nf.Range(lo, hi))
		}
		if ok50 && ok95 {
			// merged, these two stay this PC's exact figures: say so
			add("%stypical %s, %s %s", map[bool]string{true: "this PC alone: ", false: ""}[merged], nf.Value(d.Unit, p50, m),
				map[bool]string{true: "slowest 1 in 20", false: "high"}[d.Unit == "ms"], nf.Value(d.Unit, p95, m))
		}
	case "extraction.tokens", "extraction.cost":
		if d.ID == "extraction.cost" && m.Value == nil {
			add("%s", PricesHint)
		}
		if n, _ := extraInt(m, "failed_operations"); n > 0 {
			add("partial: %s could not be read, so this is a lower bound", nf.countOf(n, "server task", "server tasks"))
		}
	case "bank.size":
		var kinds []string
		for _, k := range []string{"world", "experience", "observation"} {
			if n, ok := extraInt(m, k); ok {
				kinds = append(kinds, fmt.Sprintf("%s %s", nf.Count(n), KindWords[k]))
			}
		}
		if len(kinds) > 0 {
			add("%s", strings.Join(kinds, ", "))
		}
	case "bank.consolidation":
		if f, _ := extraInt(m, "failed"); f > 0 {
			add("%s failed", nf.countOf(f, "summary", "summaries"))
		}
	case "pipeline.flow":
		ok, _ := extraInt(m, "ok")
		failed, _ := extraInt(m, "failed")
		aside, _ := extraInt(m, "set_aside")
		wait, _ := extraInt(m, "waiting")
		add("sent %s, failed %s, set aside after an error %s, waiting %s", nf.Count(ok), nf.Count(failed), nf.Count(aside), nf.Count(wait))
		if k := FlowKinds(m); k != "" {
			if slices.Contains(m.Tags, "status_this_pc") { // merged: the kinds, as the counts above, are this PC's
				add("items on this PC: %s", k)
			} else {
				add("items: %s", k)
			}
		}
	case "local.backlog":
		notes, _ := extraInt(m, "notes_queued")
		sess, _ := extraInt(m, "sessions_waiting")
		aside, _ := extraInt(m, "quarantined")
		if notes+sess+aside > 0 {
			ms, _ := extraInt(m, "oldest_ms")
			add("%s and %s wait on this PC, %s set aside after an error; the oldest %s", nf.countOf(notes, "note", "notes"), nf.countOf(sess, "session", "sessions"), nf.Count(aside), Age(ms))
		}
	case "server.queue":
		pending, _ := extraInt(m, "pending")
		proc, _ := extraInt(m, "processing")
		failed, _ := extraInt(m, "failed")
		total, _ := extraInt(m, "failed_total")
		capped, _ := m.Extra["failed_capped"].(bool)
		f := nf.Count(failed)
		if capped {
			f = nf.Count(failed) + "+ (a lower bound: more are not read)"
		}
		add("waiting %s, running %s, failed in the last 7 days %s, failed in all %s", nf.Count(pending), nf.Count(proc), f, nf.Count(total))
	case "hooks.failures":
		if l := extraCounts(m, "last_24h", failureWords, nf.Count); len(l) > 0 {
			add("in the last 24 h: %s", strings.Join(l, ", "))
		}
	case "server.reach":
		if t, ok := extraTime(m, "failing_since", loc); ok {
			add("not answering since %s", t)
		}
		if n, _ := extraInt(m, "skipped_prompts"); n > 0 {
			add("%s skipped while it was down", nf.countOf(n, "prompt", "prompts"))
		}
	case "doctor.check":
		e, _ := extraInt(m, "error")
		w, _ := extraInt(m, "warn")
		add("%s failed, %s with a warning", nf.Count(e), nf.Count(w))
	}
	if slices.Contains(m.Tags, "partial") && d.ID != "extraction.tokens" && d.ID != "extraction.cost" {
		add("partial: some of it could not be read")
	}
	return out
}
