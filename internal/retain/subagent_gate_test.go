package retain

import (
	"strings"
	"testing"
)

func TestKeepsSubagentReport(t *testing.T) {
	pad := func(s string, n int) string { return s + strings.Repeat(".", n-len(s)) }
	cases := []struct {
		name string
		text string
		min  int
		want bool
	}{
		{"a status stub", "Keep going with block 2, reading pool.go now.", 400, false},
		{"120 characters with a pull request URL", pad("Opened https://github.com/o/r/pull/12 for the fix", 120), 400, true},
		{"120 characters with an issue number", pad("Filed #214 for the leak", 120), 400, true},
		{"an owner/repo reference", pad("Fixed in Barben360/backsight#223", 120), 400, true},
		{"600 characters", strings.Repeat("a", 600), 400, true},
		{"exactly the threshold", strings.Repeat("é", 400), 400, true}, // characters, not bytes
		{"one below the threshold", strings.Repeat("é", 399), 400, false},
		{"padding does not count", "  " + strings.Repeat("a", 399) + "\n\n", 400, false},
		{"a lower threshold from the config", strings.Repeat("a", 120), 100, true},
		{"a higher threshold from the config", strings.Repeat("a", 600), 1000, false},
		{"zero keeps every report", "ok", 0, true},
		{"empty never passes", "   ", 0, false},
		// Not references: an HTML entity, a word glued to the number, a colour, a bare scheme.
		{"an HTML entity", "Escaped &#123; in the template.", 400, false},
		{"glued to a word", "Ran step2#3 again.", 400, false},
		{"a colour code", "Set the border to #fff.", 400, false},
		{"a scheme alone", "Use https:// only.", 400, false},
		// Named references, scheme-less GitHub paths, commits.
		{"PR 205", "Merged PR 205.", 400, true},
		{"PR#205", "Merged PR#205.", 400, true},
		{"issue 12", "Closed issue 12 as fixed.", 400, true},
		{"GH-7", "See GH-7 for the plan.", 400, true},
		{"lower case pr", "rebased pr 9", 400, true},
		{"a scheme-less GitHub path", "Opened github.com/o/r/pull/12.", 400, true},
		{"a short commit", "Pushed 7f50807 to the branch.", 400, true},
		{"a full commit", "Tagged c66ddda3a1b2c3d4e5f60718293a4b5c6d7e8f90.", 400, true},
		{"a hex word is no commit", "The deadbeef value is a placeholder.", 400, false},
		{"a number is no commit", "Read 12345678 rows.", 400, false},
		{"a word ending in pr is no reference", "Ran the apr 3 report.", 400, false},
		{"a plain English sentence", "Reading the pool code and the decade of facades before the next step.", 400, false},
	}
	for _, c := range cases {
		if got := KeepsSubagentReport(c.text, c.min); got != c.want {
			t.Errorf("%s: kept %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSubagentGateChangesWithItsThreshold(t *testing.T) {
	if SubagentGate(400) == SubagentGate(500) || !strings.HasPrefix(SubagentGate(400), "v2:") {
		t.Error("a new threshold must read as a new gate")
	}
	if a, b := SubagentGate(400), SubagentGate(2*200); a != b {
		t.Errorf("the same threshold must read as the same gate: %q, %q", a, b)
	}
}

func TestReportLengthBucket(t *testing.T) {
	for n, want := range map[int]string{0: "<50", 47: "<50", 50: "50-149", 149: "50-149", 150: "150-399", 399: "150-399", 400: "400-999", 4000: "1000+"} {
		if got := ReportLengthBucket(strings.Repeat("a", n)); got != want {
			t.Errorf("%d chars: %q, want %q", n, got, want)
		}
	}
}
