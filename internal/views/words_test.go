package views

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/metrics"
)

// A language of recall.by_lang reads by its name, on a page as in stats --dev: the known codes by their English
// name, the group of undetected prompts in words, any other code as it is.
func TestLanguageName(t *testing.T) {
	for code, want := range map[string]string{"en": "English", "fr": "French", "zh": "Chinese", "unknown": "language not detected", "xx": "xx"} {
		if got := LanguageName(code); got != want {
			t.Errorf("LanguageName(%q) = %q, want %q", code, got, want)
		}
	}
	m := metrics.Metric{Extra: map[string]any{"by_lang": map[string]any{"fr": map[string]any{"num": int64(3), "den": int64(4)}}}}
	d, _ := metrics.DefOf("recall.by_lang")
	if lines := ExtraLines(d, m); len(lines) != 1 || !strings.HasPrefix(lines[0], "French: ") {
		t.Errorf("the extra lines of recall.by_lang %q, want one for French", lines)
	}
}

// used.by_source reads one line per source and path in a fixed order of
// sources, each at the top ranks with the smallest change it can show when
// it has one, then at any rank; then every show at any rank, then each
// project at the top ranks. A cell without a show at the top ranks says
// none there; the words never print a source's key.
func TestBySourceLines(t *testing.T) {
	stat := func(num, den int64, value, mdc any) map[string]any {
		return map[string]any{"num": num, "den": den, "value": value, "mdc": mdc}
	}
	m := metrics.Metric{Extra: map[string]any{
		"top_rank": int64(8),
		"by_source": map[string]any{
			"unknown":           map[string]any{"search": map[string]any{"all": stat(0, 2, 0.0, nil), "top8": stat(0, 2, 0.0, nil)}},
			"conversation":      map[string]any{"search": map[string]any{"all": stat(40, 300, 0.1333, 0.08), "top8": stat(30, 210, 0.1429, 0.0956)}, "prompt": map[string]any{"all": stat(1, 4, 0.25, nil), "top8": stat(1, 4, 0.25, nil)}},
			"conversation+note": map[string]any{"search": map[string]any{"all": stat(1, 1, 1.0, nil), "top8": stat(0, 0, nil, nil)}},
		},
		"all_ranks":  stat(41, 307, 0.1336, 0.079),
		"by_project": map[string]any{"beta": stat(1, 6, 0.1667, nil), "alpha": stat(30, 210, 0.1429, 0.0956), "unknown": stat(0, 1, 0.0, nil)},
	}}
	d, _ := metrics.DefOf("used.by_source")
	want := []string{
		"from conversations, added to the prompt: 1 of 4 (25 %) at ranks 1 to 8; 1 of 4 (25 %) at any rank",
		"from conversations, Claude's search: 30 of 210 (14 %) at ranks 1 to 8, smallest change it can show 10 pts; 40 of 300 (13 %) at any rank",
		"from conversations and notes, Claude's search: none at ranks 1 to 8; 1 of 1 (100 %) at any rank",
		"source not recorded, Claude's search: 0 of 2 (0 %) at ranks 1 to 8; 0 of 2 (0 %) at any rank",
		"every source, any rank: 41 of 307 (13 %)",
		"project alpha, ranks 1 to 8: 30 of 210 (14 %), smallest change it can show 10 pts",
		"project beta, ranks 1 to 8: 1 of 6 (17 %)",
		"no project recorded, ranks 1 to 8: 0 of 1 (0 %)",
	}
	got := ExtraLines(d, m)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, k := range []string{"conversation", "note", "conversation+note", "subagent", "other", "unknown"} {
		if w := MemorySourceWords[k]; w == "" || w == k {
			t.Errorf("source %q has no plain words (%q)", k, w)
		}
	}
}

// Every key the time-to-recall funnel carries, and every state of a
// never-extracted session, has a plain word of its own (Word falls back to
// the key itself, which a person must not read as a snake_case name).
func TestFunnelAndNeverExtractedWordsCoverEveryKey(t *testing.T) {
	for _, k := range metrics.FunnelKeys {
		if w := FunnelWords[k]; w == "" || (w == k && k != "sent") { // "sent" is its own plain word
			t.Errorf("funnel key %q has no plain word (%q)", k, w)
		}
	}
	for _, k := range metrics.NeverStates {
		if w := NeverExtractedWords[k]; w == "" || w == k {
			t.Errorf("never-extracted state %q has no plain word (%q)", k, w)
		}
	}
	if len(FunnelWords) != len(metrics.FunnelKeys) {
		t.Errorf("FunnelWords has %d words for %d funnel keys", len(FunnelWords), len(metrics.FunnelKeys))
	}
}

// Every status a doctor history day can read, and the tag of an old scan,
// has a plain word of its own.
func TestDoctorHistoryAndScanWords(t *testing.T) {
	for _, k := range metrics.DayStatuses {
		if w := DayStatusWords[k]; w == "" || (w == k && k != "ok") {
			t.Errorf("day status %q has no plain word (%q)", k, w)
		}
	}
	if len(DayStatusWords) != len(metrics.DayStatuses) {
		t.Errorf("DayStatusWords has %d words for %d statuses", len(DayStatusWords), len(metrics.DayStatuses))
	}
	if w := TagWords["old_scan"]; w == "" || w == "old_scan" {
		t.Errorf("old_scan has no plain word (%q)", w)
	}
}

// Every state of a memory's origin has a plain word of its own.
func TestOriginWordsCoverEveryState(t *testing.T) {
	for _, k := range metrics.OriginStates {
		if w := OriginWords[k]; w == "" || w == k {
			t.Errorf("origin state %q has no plain word (%q)", k, w)
		}
	}
	if len(OriginWords) != len(metrics.OriginStates) {
		t.Errorf("OriginWords has %d words for %d states", len(OriginWords), len(metrics.OriginStates))
	}
}

// Every reason a recall can get nothing has a plain word of its own: the
// replacer would otherwise leave the code (snake_case, or "error") raw.
func TestEmptyWordsCoverEveryReason(t *testing.T) {
	for _, k := range []string{"no_candidates", "below_floor", "all_seen", "capped", "error", "unknown"} {
		if w := EmptyWords.Replace(k); w == "" || w == k {
			t.Errorf("empty reason %q has no plain word (%q)", k, w)
		}
	}
}

// Every verdict a turn can declare has a word of its own: confirmed is
// not read as "not understood", nor as used.
func TestDeclaredWordsCoverEveryVerdict(t *testing.T) {
	seen := map[string]string{}
	for _, v := range []string{"used", "confirmed", "none", "wrong", "outdated", "invalid"} {
		w, ok := DeclaredWords[v]
		if !ok || w == "" {
			t.Errorf("verdict %q has no word", v)
		}
		if other, dup := seen[w]; dup {
			t.Errorf("verdicts %q and %q share the word %q", other, v, w)
		}
		seen[w] = v
	}
}

// A latency range reads the same whether the view computed it or JSON
// carried it, the open bucket as "over".
func TestRangeMSReadsBothForms(t *testing.T) {
	var decoded struct{ R []any }
	if err := json.Unmarshal([]byte(`{"R":[1000,2000]}`), &decoded); err != nil {
		t.Fatal(err)
	}
	var open struct{ R []any }
	if err := json.Unmarshal([]byte(`{"R":[5000,null]}`), &open); err != nil {
		t.Fatal(err)
	}
	lo, hi := int64(250), int64(500)
	for _, c := range []struct {
		name string
		v    any
		want string
	}{
		{"computed", &[2]*int64{&lo, &hi}, "250–500 ms"},
		{"decoded", decoded.R, "1.0–2.0 s"},
		{"decoded open", open.R, "over 5.0 s"},
	} {
		l, h, ok := RangeMS(metrics.Metric{Extra: map[string]any{"r": c.v}}, "r")
		if !ok || RangeText(l, h) != c.want {
			t.Errorf("%s: %d %v %v, text %q, want %q", c.name, l, h, ok, RangeText(l, h), c.want)
		}
	}
	for _, v := range []any{nil, (*[2]*int64)(nil), []any{"x", 1.0}, []any{1.0}, "1-2"} {
		if _, _, ok := RangeMS(metrics.Metric{Extra: map[string]any{"r": v}}, "r"); ok {
			t.Errorf("%v read as a range", v)
		}
	}
}

// A document set aside reads the design's two words, and the one the design
// has no word for says what to do: the pages and the CLI share this map.
func TestIgnoredWordsAreTheDesigns(t *testing.T) {
	want := map[string]string{
		"too_large":       "over the 1 MiB cap",
		"not_valid":       "not valid",
		"unknown_version": "written by a newer backsight: update this PC",
	}
	if len(IgnoredWords) != len(want) {
		t.Errorf("IgnoredWords has %d words, want %d: %v", len(IgnoredWords), len(want), IgnoredWords)
	}
	for k, w := range want {
		if got := IgnoredWords[k]; got != w {
			t.Errorf("%s reads %q, want %q", k, got, w)
		}
	}
}
