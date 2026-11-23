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

	// SkipWords name why a prompt got no recall; FailureKindWords how a hook failed.
	SkipWords = map[string]string{
		"machine": "a background session", "too_short": "too short", "same_topic": "same topic as before", "gap": "too soon after the last recall",
		"budget": "over the size limit", "topic_shift": "topic changed", "breaker": "memory server paused", "maintenance": "while backsight was being serviced",
		"first": "start of a session", "pointer": "only a link, read before searching", "other": "other", "unknown": "reason not recorded",
	}
	FailureKindWords = map[string]string{
		"panic": "crashed", "fatal": "stopped on an error", "timeout": "ran too long", "killed": "was stopped", "exit": "exited with an error",
		"invalid_output": "gave an unreadable answer", "input_too_large": "input too large", "spawn": "could not start", "unknown": "reason not recorded",
	}

	// DecisionWords say what the recall did for a prompt; DeclaredWords what
	// Claude said of the memories it was shown.
	DecisionWords = map[string]string{"recall": "looked for memory", "skip": "no memory looked up"}
	DeclaredWords = map[string]string{"used": "used", "confirmed": "confirmed", "none": "none used", "wrong": "reported wrong", "outdated": "out of date", "invalid": "not understood"}

	// RetireWords name what was done to a memory; ServerStateWords the
	// state the server keeps it in.
	RetireWords      = map[string]string{"retire": "retired", "hide": "hidden", "unhide": "restored", "forget": "forgotten"}
	ServerStateWords = map[string]string{"active": "available", "valid": "available", "invalidated": "forgotten"}

	// HasWords and SortWords label the filters and orders of the lists.
	HasWords  = map[string]string{"used": "Used", "wrong": "Reported wrong", "undeclared": "Not declared", "memory": "Got memory", "never_extracted": "Never turned into memories", "contradicted": "Corrected after memory"}
	SortWords = map[string]string{"shown": "Most shown", "used": "Most used", "rate": "Used rate", "last_shown": "Last shown"}

	// LanguageWords name the language groups of recall.by_lang: the languages prompts are read as, and the
	// prompts whose language was not detected. LanguageName reads them.
	LanguageWords = map[string]string{
		"en": "English", "fr": "French", "de": "German", "es": "Spanish", "it": "Italian", "pt": "Portuguese",
		"nl": "Dutch", "sv": "Swedish", "da": "Danish", "no": "Norwegian", "fi": "Finnish", "pl": "Polish",
		"cs": "Czech", "ru": "Russian", "uk": "Ukrainian", "tr": "Turkish", "ar": "Arabic", "he": "Hebrew",
		"hi": "Hindi", "ja": "Japanese", "ko": "Korean", "zh": "Chinese", "vi": "Vietnamese", "id": "Indonesian",
		"unknown": "language not detected",
	}
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

// memorySources is the order used.by_source lists its sources in.
var memorySources = []string{"conversation", "note", "conversation+note", "subagent", "other", "unknown"}

// MemorySourceWords say where a memory of used.by_source came from.
var MemorySourceWords = map[string]string{
	"conversation": "from conversations", "note": "from notes", "conversation+note": "from conversations and notes",
	"subagent": "with a subagent's report", "other": "from other sources", "unknown": "source not recorded",
}

// smallestChange is ", smallest change it can show N pts" of a decoded
// cell that has one, else "".
func smallestChange(m map[string]any) string {
	if v, ok := m["mdc"].(float64); ok {
		return ", smallest change it can show " + Percent(v) + " pts"
	}
	return ""
}

// topShare is a cell at the top ranks (named by at): its share and the
// smallest change it can show, or none when it holds no show.
func topShare(m map[string]any, at string) string {
	if n, ok := asCount(m["den"]); !ok || n == 0 {
		return "none at " + at
	}
	s, _ := share(m)
	return s + " at " + at + smallestChange(m)
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
	case "used.by_source":
		top := "ranks 1 to 8"
		if n, ok := extraInt(m, "top_rank"); ok && n > 0 {
			top = "ranks 1 to " + Grouped(n)
		}
		g := group("by_source")
		for _, src := range memorySources {
			paths, _ := g[src].(map[string]any)
			for _, p := range sortedKeys(paths) {
				one, _ := paths[p].(map[string]any)
				all, okAll := one["all"].(map[string]any)
				topCell, _ := one["top8"].(map[string]any)
				a, ok := share(all)
				if !okAll || !ok {
					continue
				}
				out = append(out, Word(MemorySourceWords, src)+", "+Word(PathWords, p)+": "+topShare(topCell, top)+"; "+a+" at any rank")
			}
		}
		if all, ok := m.Extra["all_ranks"]; ok {
			if one, ok := generic(all).(map[string]any); ok {
				if s, ok := share(one); ok {
					out = append(out, "every source, any rank: "+s)
				}
			}
		}
		p := group("by_project")
		for _, k := range sortedKeys(p) {
			if one, ok := p[k].(map[string]any); ok {
				if s, ok := share(one); ok {
					name := "project " + k
					if k == "unknown" { // the session's turn named none
						name = "no project recorded"
					}
					out = append(out, name+", "+top+": "+s+smallestChange(one))
				}
			}
		}
	case "recall.by_lang":
		g := group("by_lang")
		for _, k := range sortedKeys(g) {
			if one, ok := g[k].(map[string]any); ok {
				if s, ok := share(one); ok {
					out = append(out, LanguageName(k)+": "+s)
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
