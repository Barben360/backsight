package views

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/Barben360/backsight/internal/metrics"
)

// The dashboard's words for what `stats` prints as it is stored: job
// names and statuses, health check statuses, server task types, the
// runtime and mode of the memory server.
var (
	JobWords = map[string]string{"tick": "regular upkeep", "maintain": "daily maintenance"}

	JobStatusWords = map[string]string{
		"done": "done", "partial": "partly done", "started": "started, not finished", "failed": "failed", "error": "failed",
	}

	DoctorStatusWords = map[string]string{"ok": "ok", "info": "for information", "warn": "warning", "error": "problem"}

	OperationWords = map[string]string{
		"retain": "saving notes", "batch_retain": "saving notes in batches", "consolidation": "summarising",
		"refresh_mental_model": "refreshing summaries", "file_convert_retain": "reading files", "other": "other",
		"recall": "recall", "reflect": "reflection",
	}

	RuntimeWords = map[string]string{"docker": "Docker", "native": "a native install"}
	ModeWords    = map[string]string{"local": "on this PC", "remote": "on another machine"}

	// LanguageWords name the language groups of recall.by_lang.
	LanguageWords = map[string]string{"unknown": "language not detected"}
)

// generic is v as plain JSON values, so a breakdown held in a type of the
// computing code reads like any other.
func generic(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func asCount(v any) (int64, bool) {
	f, ok := v.(float64)
	return int64(math.Round(f)), ok
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// share is "num of den (percent %)" of a decoded {num, den} pair.
func share(m map[string]any) (string, bool) {
	num, ok1 := asCount(m["num"])
	den, ok2 := asCount(m["den"])
	if !ok1 || !ok2 {
		return "", false
	}
	s := fmt.Sprintf("%s of %s", Grouped(num), Grouped(den))
	if v, ok := m["value"].(float64); ok {
		s += " (" + Percent(v) + " %)"
	}
	return s, true
}

// ExtraLines are the lines a metric's breakdown adds: the split by kind,
// by result, by path, by language and by rank. MetricDetails holds the
// other extras. A metric without a breakdown has none.
func ExtraLines(d metrics.Def, m metrics.Metric) []string {
	var out []string
	group := func(key string) map[string]any { g, _ := generic(m.Extra[key]).(map[string]any); return g }
	switch d.ID {
	case "bank.growth":
		var kinds []string
		for _, k := range []string{"world", "experience", "observation"} {
			if n, ok := extraInt(m, k); ok {
				kinds = append(kinds, fmt.Sprintf("%s %s", Grouped(n), KindWords[k]))
			}
		}
		if len(kinds) > 0 {
			out = append(out, "added over the period: "+strings.Join(kinds, ", "))
		}
		if n, ok := extraInt(m, "days_covered"); ok {
			out = append(out, "the server keeps only "+CountOf(n, "day", "days")+" of history")
		}
	case "retain.outcomes":
		g := group("by_status")
		var parts []string
		for _, k := range sortedKeys(g) {
			if n, ok := asCount(g[k]); ok && n > 0 {
				parts = append(parts, Word(SentWords, k)+" "+Grouped(n))
			}
		}
		if len(parts) > 0 {
			out = append(out, "by result: "+strings.Join(parts, ", "))
		}
		if v, ok := extraFloat(m, "p95_ms"); ok {
			out = append(out, "slowest 1 in 20: "+ValueIn("ms", v, m))
		}
	case "used.by_path":
		g := group("by_path")
		for _, k := range sortedKeys(g) {
			if one, ok := g[k].(map[string]any); ok {
				if s, ok := share(one); ok {
					out = append(out, Word(PathWords, k)+": "+s)
				}
			}
		}
	case "recall.by_lang":
		g := group("by_lang")
		for _, k := range sortedKeys(g) {
			if one, ok := g[k].(map[string]any); ok {
				if s, ok := share(one); ok {
					out = append(out, Word(LanguageWords, k)+": "+s)
				}
			}
		}
	case "used.rank":
		if v, ok := extraFloat(m, "hit_at_1"); ok {
			out = append(out, "the best-ranked memory was used in "+Percent(v)+" % of turns")
		}
		if v, ok := extraFloat(m, "hit_at_3"); ok {
			out = append(out, "one of the first three was used in "+Percent(v)+" % of turns")
		}
		g := group("histogram")
		var parts []string
		for _, k := range []string{"1", "2", "3", "4", "5", "6+", "unranked"} {
			if n, ok := asCount(g[k]); ok {
				name := "rank " + k
				if k == "unranked" {
					name = "not ranked"
				}
				parts = append(parts, name+" "+Grouped(n))
			}
		}
		if len(parts) > 0 {
			out = append(out, "credited memories by rank: "+strings.Join(parts, ", "))
		}
	}
	return slices.Clip(out)
}
