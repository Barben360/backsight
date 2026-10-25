package stats

import "fmt"

// Parity compares a report with the reference stack's usage on the
// author's machine (SPEC §1.2, decision 0065): C12's numbers for that
// machine, 2026-10-07 → 2026-10-23 (research/C12-usage-data.md).

// MinParity is the smallest denominator a verdict is given on.
const MinParity = 10

// ParityLine is one metric of the comparison. Rates are N of Of; the
// latency line carries milliseconds in N and the recall count in Of.
type ParityLine struct {
	Metric string `json:"metric"`
	Rule   string `json:"rule"` // ">=" or "<="
	BaseN  int    `json:"baseline_n"`
	BaseOf int    `json:"baseline_of,omitempty"`
	// BarN/BarOf: the bar a rate is judged against, the baseline itself
	// unless 0065 sets a tolerance.
	BarN     int    `json:"bar_n"`
	BarOf    int    `json:"bar_of,omitempty"`
	N        int    `json:"n"`
	Of       int    `json:"of"`
	Latency  bool   `json:"latency,omitempty"`
	Verdict  string `json:"verdict"` // pass, fail, too few
	Baseline string `json:"baseline"`
}

// Verdicts.
const (
	ParityPass   = "pass"
	ParityFail   = "fail"
	ParityTooFew = "too few"
)

// Parity is the comparison of 0065, in its table's order.
func Parity(r Report) []ParityLine {
	cites := r.Turns.Credit["exact"] + r.Turns.Credit["free"] + r.Turns.Credit["empty"]
	replies := 0
	for _, n := range r.Turns.Credit {
		replies += n
	}
	lines := []ParityLine{
		rate("recalls that injected something", ">=", 16, 61, r.Prompts.WithHits, r.Prompts.Recalls),
		{Metric: "prompt hook p95", Rule: "<=", BaseN: 4200, N: r.Prompts.P95MS, Of: r.Prompts.Recalls, Latency: true, Baseline: "4200 ms"},
		// 0/124 in C12: one transient failure must not fail parity, so the
		// bar is 1 % (doctor warns from 5 %).
		withBar(rate("failed recall scope calls", "<=", 0, 124, r.Prompts.ScopeFailures, r.Prompts.ScopeCalls), 1, 100, "0% (0/124), up to 1%"),
		rate("injections followed by a memory_search", ">=", 7, 16, r.Injections.SearchedBySearch, r.Injections.N),
		rate("injections credited (exact)", ">=", 3, 16, r.Injections.Exact, r.Injections.N),
		rate("replies after a search that cite the memory", ">=", 4, 17, cites, replies),
	}
	for i := range lines {
		lines[i].Verdict = verdict(lines[i])
	}
	return lines
}

func rate(metric, rule string, baseN, baseOf, n, of int) ParityLine {
	return ParityLine{Metric: metric, Rule: rule, BaseN: baseN, BaseOf: baseOf, BarN: baseN, BarOf: baseOf, N: n, Of: of, Baseline: pctOf(baseN, baseOf)}
}

func withBar(l ParityLine, n, of int, shown string) ParityLine {
	l.BarN, l.BarOf, l.Baseline = n, of, shown
	return l
}

func verdict(l ParityLine) string {
	if l.Of < MinParity {
		return ParityTooFew
	}
	// Rates compare n/of with baseN/baseOf by cross-multiplication.
	got, want := l.N, l.BaseN
	if !l.Latency {
		got, want = l.N*l.BarOf, l.BarN*l.Of
	}
	if (l.Rule == ">=" && got >= want) || (l.Rule == "<=" && got <= want) {
		return ParityPass
	}
	return ParityFail
}

// ParityOverall: "holds" when every metric passes, "fails" when one fails,
// otherwise "not yet" (some have too few events).
func ParityOverall(lines []ParityLine) string {
	out := "holds"
	for _, l := range lines {
		switch l.Verdict {
		case ParityFail:
			return "fails"
		case ParityTooFew:
			out = "not yet"
		}
	}
	return out
}

func pctOf(n, of int) string {
	if of == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%% (%d/%d)", (100*n+of/2)/of, n, of)
}
