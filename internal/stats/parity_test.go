package stats

import "testing"

func parityReport() Report {
	var r Report
	r.Prompts.Recalls, r.Prompts.WithHits, r.Prompts.P95MS = 20, 6, 4200 // 30 % >= 26 %; 4.2 s is the bar itself
	r.Prompts.ScopeCalls, r.Prompts.ScopeFailures = 40, 0
	r.Injections.N, r.Injections.Searched, r.Injections.Credited = 16, 7, 3 // C12 exactly: equal passes
	r.Turns.Credit = map[string]int{"exact": 1, "free": 1, "empty": 1, "silent": 7}
	return r
}

func byMetric(lines []ParityLine) map[string]ParityLine {
	m := map[string]ParityLine{}
	for _, l := range lines {
		m[l.Metric] = l
	}
	return m
}

// Equal to the baseline passes; free and empty credits count as citing.
func TestParityPassesAtTheBaseline(t *testing.T) {
	lines := Parity(parityReport())
	if len(lines) != 6 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		if l.Verdict != ParityPass {
			t.Errorf("%s: %+v", l.Metric, l)
		}
	}
	if c := byMetric(lines)["replies after a search that cite the memory"]; c.N != 3 || c.Of != 10 {
		t.Errorf("cites %+v", c)
	}
	if got := ParityOverall(lines); got != "holds" {
		t.Errorf("overall %q", got)
	}
}

// One step past the baseline on each side fails, in the rule's direction.
func TestParityFailsJustPastTheBaseline(t *testing.T) {
	r := parityReport()
	r.Prompts.WithHits = 5                                   // 25 % < 26 %
	r.Prompts.P95MS = 4201                                   // above 4.2 s
	r.Prompts.ScopeFailures = 1                              // any failure is above 0 %
	r.Injections.Searched = 6                                // 6/16 < 7/16
	r.Injections.Credited = 2                                // 2/16 < 3/16
	r.Turns.Credit["exact"], r.Turns.Credit["silent"] = 0, 8 // 2/10 = 20 % < 24 %
	for _, l := range Parity(r) {
		if l.Verdict != ParityFail {
			t.Errorf("%s: %+v", l.Metric, l)
		}
	}
	if got := ParityOverall(Parity(r)); got != "fails" {
		t.Errorf("overall %q", got)
	}
}

// Below MinParity events a metric has no verdict, and the whole is not yet
// decided; a fail still wins over too few.
func TestParityTooFew(t *testing.T) {
	r := parityReport()
	r.Injections.N, r.Injections.Searched, r.Injections.Credited = 9, 9, 9
	m := byMetric(Parity(r))
	if m["injections followed by a search"].Verdict != ParityTooFew || m["injections credited"].Verdict != ParityTooFew {
		t.Errorf("%+v", m)
	}
	if got := ParityOverall(Parity(r)); got != "not yet" {
		t.Errorf("overall %q", got)
	}
	r.Prompts.P95MS = 9000
	if got := ParityOverall(Parity(r)); got != "fails" {
		t.Errorf("a fail beside too few: %q", got)
	}
	var empty Report
	if got := ParityOverall(Parity(empty)); got != "not yet" {
		t.Errorf("no events: %q", got)
	}
}
