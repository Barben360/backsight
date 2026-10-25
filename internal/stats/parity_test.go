package stats

import "testing"

func parityReport() Report {
	var r Report
	r.Prompts.Recalls, r.Prompts.WithHits, r.Prompts.P95MS = 20, 6, 4200 // 30 % >= 26 %; 4.2 s is the bar itself
	r.Prompts.ScopeCalls, r.Prompts.ScopeFailures = 100, 1               // 1 %: the tolerance itself
	// C12 exactly: equal passes. Searched and Credited (any memory tool,
	// exact or free) are looser than C12 and must not be what is judged.
	r.Injections.N, r.Injections.SearchedBySearch, r.Injections.Exact = 16, 7, 3
	r.Injections.Searched, r.Injections.Credited = 16, 16
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

// Equal to the bar passes; free and empty credits count as citing.
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

// One step past the bar on each side fails, in the rule's direction; the
// looser counts (any memory tool, free credits) do not rescue it.
func TestParityFailsJustPastTheBaseline(t *testing.T) {
	r := parityReport()
	r.Prompts.WithHits = 5                                   // 25 % < 26 %
	r.Prompts.P95MS = 4201                                   // above 4.2 s
	r.Prompts.ScopeFailures = 2                              // 2 % > 1 %
	r.Injections.SearchedBySearch = 6                        // 6/16 < 7/16, though 16/16 searched with any tool
	r.Injections.Exact = 2                                   // 2/16 < 3/16, though 16/16 credited exact or free
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
	r.Injections.N, r.Injections.SearchedBySearch, r.Injections.Exact = 9, 9, 9
	m := byMetric(Parity(r))
	if m["injections followed by a memory_search"].Verdict != ParityTooFew || m["injections credited (exact)"].Verdict != ParityTooFew {
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

// The latency line has its own gate: a fast p95 over 9 recalls is no pass.
func TestParityLatencyNeedsEnoughRecalls(t *testing.T) {
	r := parityReport()
	r.Prompts.Recalls, r.Prompts.WithHits, r.Prompts.P95MS = 9, 9, 100
	m := byMetric(Parity(r))
	if m["prompt hook p95"].Verdict != ParityTooFew {
		t.Errorf("%+v", m["prompt hook p95"])
	}
	if got := ParityOverall(Parity(r)); got != "not yet" {
		t.Errorf("overall %q", got)
	}
}
