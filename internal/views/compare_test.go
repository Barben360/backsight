package views

import (
	"net/url"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
)

// TestCompareFlags: every refusal of --compare and --measure is a usage
// error (exit 2) with one sentence, and the dashboard reads the same
// parameters through FromValues and Query, so its 400 carries the same text.
func TestCompareFlags(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, query, err string
	}{
		{"one version", "compare=v1", "--compare: two versions, as A,B"},
		{"three versions", "compare=v1,v2,v3", "--compare: two versions, as A,B"},
		{"an empty side", "compare=v1,", "--compare: two versions, as A,B"},
		{"empty", "compare=", ""},
		{"the same version", "compare=v1,v1", "--compare: A and B are the same version"},
		{"the same version with a space", "compare=v1,%20v1", "--compare: A and B are the same version"},
		{"an unknown measure", "compare=v1,v2&measure=declare.coverage", "--measure: one of used.turn_rate, recall.hit_rate, verdict.wrong_rate"},
		{"a measure alone", "measure=recall.hit_rate", "--measure needs --compare"},
		{"an unknown measure alone", "measure=x", "--measure: one of used.turn_rate, recall.hit_rate, verdict.wrong_rate"},
		{"ok", "compare=v1,v2", ""},
		{"ok with a measure", "compare=v1,v2&measure=verdict.wrong_rate", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, _ := url.ParseQuery(c.query)
			p, err := FromValues("releases", v)
			if err == nil {
				_, err = Query("releases", p, "", now)
			}
			if c.err == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || err.Error() != c.err || codeOf(t, err) != output.Usage {
				t.Fatalf("error %v, want usage %q", err, c.err)
			}
		})
	}
	// the two versions reach the query, trimmed, with the measure
	v, _ := url.ParseQuery("compare=v0.1.16-m7,%20v0.1.17-m7&measure=recall.hit_rate")
	p, _ := FromValues("releases", v)
	q, err := Query("releases", p, "", now)
	if err != nil || q.Compare != [2]string{"v0.1.16-m7", "v0.1.17-m7"} || q.Measure != "recall.hit_rate" {
		t.Errorf("query %+v, %v", q, err)
	}
	// other views take neither parameter
	v, _ = url.ParseQuery("compare=v1,v2")
	if _, err := FromValues("overview", v); err == nil {
		t.Error("overview took compare")
	}
}
