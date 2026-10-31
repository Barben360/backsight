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
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04"), true
}

// extraCounts reads a map of counts, sorted by key.
func extraCounts(m metrics.Metric, key string, words map[string]string) []string {
	var out []string
	switch v := m.Extra[key].(type) {
	case map[string]int64:
		for k, n := range v {
			if n > 0 {
				out = append(out, Word(words, k)+" "+Grouped(n))
			}
		}
	case map[string]any:
		for k, x := range v {
			if n, ok := x.(int64); ok && n > 0 {
				out = append(out, Word(words, k)+" "+Grouped(n))
			}
		}
	}
	slices.Sort(out)
	return out
}

// MetricDetails are the indented lines under a metric: what the number does
// not say by itself (a lower bound, a partial total, a split).
func MetricDetails(d metrics.Def, m metrics.Metric, loc *time.Location) []string {
	return details(d, m, loc, false)
}

// PageDetails are the same lines for a page, which names the skip reasons
// and the hook failure kinds in words.
func PageDetails(d metrics.Def, m metrics.Metric, loc *time.Location) []string {
	return details(d, m, loc, true)
}

func details(d metrics.Def, m metrics.Metric, loc *time.Location, page bool) []string {
	var skipWords, failureWords map[string]string
	if page {
		skipWords, failureWords = SkipWords, FailureKindWords
	}
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	switch d.ID {
	case "recall.empty_reasons":
		if e := extraCounts(m, "empty", nil); len(e) > 0 {
			add("why: %s", EmptyWords.Replace(strings.Join(e, ", ")))
		}
		if s := extraCounts(m, "skips", skipWords); len(s) > 0 {
			add("prompts skipped: %s", strings.Join(s, ", "))
		}
	case "used.turn_rate":
		if w, ok := extraFloat(m, "worst_case"); ok {
			add("worst case, if every turn where Claude did not say counts as unused: %s %%", Percent(w))
		}
		if p, _ := extraInt(m, "pending"); p > 0 {
			add("%s still open", CountOf(p, "turn", "turns"))
		}
	case "context.per_session", "context.per_prompt", "recall.latency", "search.latency":
		p50, ok50 := extraFloat(m, "p50")
		p95, ok95 := extraFloat(m, "p95")
		if ok50 && ok95 {
			add("typical %s, %s %s", ValueIn(d.Unit, p50, m), map[bool]string{true: "slowest 1 in 20", false: "high"}[d.Unit == "ms"], ValueIn(d.Unit, p95, m))
		}
	case "extraction.tokens", "extraction.cost":
		if d.ID == "extraction.cost" && m.Value == nil {
			add("%s", PricesHint)
		}
		if n, _ := extraInt(m, "failed_operations"); n > 0 {
			add("partial: %s could not be read, so this is a lower bound", CountOf(n, "server task", "server tasks"))
		}
	case "bank.size":
		var kinds []string
		for _, k := range []string{"world", "experience", "observation"} {
			if n, ok := extraInt(m, k); ok {
				kinds = append(kinds, fmt.Sprintf("%s %s", Grouped(n), KindWords[k]))
			}
		}
		if len(kinds) > 0 {
			add("%s", strings.Join(kinds, ", "))
		}
	case "bank.consolidation":
		if f, _ := extraInt(m, "failed"); f > 0 {
			add("%s failed", CountOf(f, "summary", "summaries"))
		}
	case "pipeline.flow":
		ok, _ := extraInt(m, "ok")
		failed, _ := extraInt(m, "failed")
		aside, _ := extraInt(m, "set_aside")
		wait, _ := extraInt(m, "waiting")
		add("sent %s, failed %s, set aside after an error %s, waiting %s", Grouped(ok), Grouped(failed), Grouped(aside), Grouped(wait))
	case "local.backlog":
		notes, _ := extraInt(m, "notes_queued")
		sess, _ := extraInt(m, "sessions_waiting")
		aside, _ := extraInt(m, "quarantined")
		if notes+sess+aside > 0 {
			ms, _ := extraInt(m, "oldest_ms")
			add("%s and %s wait on this PC, %s set aside after an error; the oldest %s", CountOf(notes, "note", "notes"), CountOf(sess, "session", "sessions"), Grouped(aside), Age(ms))
		}
	case "server.queue":
		pending, _ := extraInt(m, "pending")
		proc, _ := extraInt(m, "processing")
		failed, _ := extraInt(m, "failed")
		total, _ := extraInt(m, "failed_total")
		capped, _ := m.Extra["failed_capped"].(bool)
		f := Grouped(failed)
		if capped {
			f = Grouped(failed) + "+ (a lower bound: more are not read)"
		}
		add("waiting %s, running %s, failed in the last 7 days %s, failed in all %s", Grouped(pending), Grouped(proc), f, Grouped(total))
	case "hooks.failures":
		if l := extraCounts(m, "last_24h", failureWords); len(l) > 0 {
			add("in the last 24 h: %s", strings.Join(l, ", "))
		}
	case "server.reach":
		if t, ok := extraTime(m, "failing_since", loc); ok {
			add("not answering since %s", t)
		}
		if n, _ := extraInt(m, "skipped_prompts"); n > 0 {
			add("%s skipped while it was down", CountOf(n, "prompt", "prompts"))
		}
	case "doctor.check":
		e, _ := extraInt(m, "error")
		w, _ := extraInt(m, "warn")
		add("%s failed, %s with a warning", Grouped(e), Grouped(w))
	}
	if slices.Contains(m.Tags, "partial") && d.ID != "extraction.tokens" && d.ID != "extraction.cost" {
		add("partial: some of it could not be read")
	}
	return out
}
