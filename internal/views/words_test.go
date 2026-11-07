package views

import (
	"encoding/json"
	"testing"

	"github.com/Barben360/backsight/internal/metrics"
)

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
