package metrics

import (
	"strings"
	"testing"
)

// The Wilson bounds quoted below are worked out by hand with z = 1.96:
//
//	centre = (p + z²/2n) / (1 + z²/n)
//	half   = z·√(p(1−p)/n + z²/4n²) / (1 + z²/n)
//	[lo, hi] = [centre − half, centre + half], lo = 0 at k = 0, hi = 1 at k = n
//
// Worked example, 2/40: p = 0.05, z²/n = 0.09604, centre =
// (0.05 + 0.04802)/1.09604 = 0.08943, half = 1.96·√(0.0011875 +
// 0.00060025)/1.09604 = 0.07561, so [0.0138, 0.1650]. At k = 0 the upper
// bound is z²/(n + z²): 0/40 → 3.8416/43.8416 = 0.0876. At k = n the lower
// bound is n/(n + z²). The other rows use the same two lines; TestWilson
// pins the formula itself.

// mustDef is a catalog entry, failing the test when the id is unknown.
func mustDef(t *testing.T, id string) Def {
	t.Helper()
	d, ok := DefOf(id)
	if !ok {
		t.Fatalf("no catalog entry %q", id)
	}
	return d
}

func rate(num, den int64) Observation { return Observation{Num: num, Den: den} }

func pct(n int64, p95 float64) Observation { return Observation{Den: n, P95: f(p95)} }

type judgeCase struct {
	name       string
	id         string
	edit       func(*Def) // optional change to the catalog entry
	o          Observation
	status     Status
	borderline bool
	reason     string
}

func runJudge(t *testing.T, cases []judgeCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := mustDef(t, c.id)
			if c.edit != nil {
				c.edit(&d)
			}
			j := Judge(d, c.o)
			if j.Status != c.status || j.Borderline != c.borderline || j.Reason != c.reason {
				t.Errorf("Judge(%s, %+v) = {%s %q borderline=%v}, want {%s %q borderline=%v}",
					c.id, c.o, j.Status, j.Reason, j.Borderline, c.status, c.reason, c.borderline)
			}
		})
	}
}

func TestJudgeOrder(t *testing.T) {
	runJudge(t, []judgeCase{
		// Step 1 beats everything: the figures say watch, the source is gone.
		{name: "absent beats a past threshold", id: "recall.hit_rate",
			o: Observation{Absent: true, Reason: "server_down", Num: 0, Den: 40}, status: StatusNoData, reason: "server_down"},
		{name: "absent beats an error rule", id: "pipeline.flow",
			o: Observation{Absent: true, Reason: "no_events", Err: StatusBad}, status: StatusNoData, reason: "no_events"},
		{name: "absent stale is no_data, not info", id: "stale.hidden",
			o: Observation{Absent: true, Reason: "feedback_off"}, status: StatusNoData, reason: "feedback_off"},
		// Step 2 beats steps 3-5. 0/19: hi = 3.8416/22.8416 = 0.168, but
		// 19 < min_n 20, so the hi < 0.10 rule is never asked.
		{name: "0/19 is too_few", id: "recall.hit_rate", o: rate(0, 19), status: StatusTooFew},
		{name: "0/0 is too_few", id: "recall.hit_rate", o: rate(0, 0), status: StatusTooFew},
		// At min_n the rules apply: 0/20 hi = 3.8416/23.8416 = 0.1611 >= 0.10,
		// ok; the point 0 is under 0.10 and its bound is not: borderline.
		{name: "0/20 is judged", id: "recall.hit_rate", o: rate(0, 20), status: StatusOK, borderline: true},
		{name: "too_few beats an error rule", id: "pipeline.flow",
			edit: func(d *Def) { d.MinN = 5 }, o: Observation{Den: 4, Err: StatusBad}, status: StatusTooFew},
		{name: "too_few beats a relative watch", id: "used.turn_rate",
			o: Observation{Num: 0, Den: 19, Baseline: []BaselineValue{{0.5, 20}, {0.5, 20}}}, status: StatusTooFew},
		// Percentiles: min_n 100 for p95.
		{name: "latency n 99 is too_few", id: "recall.latency", o: pct(99, 3000), status: StatusTooFew},
		{name: "latency n 100 p95 2,000 ok", id: "recall.latency", o: pct(100, 2000), status: StatusOK},
		{name: "latency n 100 p95 2,001 watch", id: "recall.latency", o: pct(100, 2001), status: StatusWatch},
		{name: "latency p95 2,500 watch", id: "recall.latency", o: pct(100, 2500), status: StatusWatch},
		{name: "latency p95 2,501 bad", id: "recall.latency", o: pct(100, 2501), status: StatusBad},
		{name: "rubber_stamp 25/29 too_few", id: "used.rubber_stamp", o: rate(25, 29), status: StatusTooFew},
	})
}

func TestJudgeCI(t *testing.T) {
	d := mustDef(t, "recall.hit_rate")
	// 0/19: too_few, and the interval is still given: [0, 0.1682].
	j := Judge(d, rate(0, 19))
	if j.CI == nil {
		t.Fatal("0/19: CI nil, want [0, 0.1682]")
	}
	near(t, "0/19 lo", j.CI[0], 0, 1e-9)
	near(t, "0/19 hi", j.CI[1], 0.1682, 1e-4)
	if j := Judge(d, rate(0, 0)); j.CI != nil {
		t.Errorf("0/0: CI = %v, want nil", *j.CI)
	}
	if j := Judge(d, Observation{Absent: true, Reason: "no_events", Num: 2, Den: 40}); j.CI != nil {
		t.Errorf("absent: CI = %v, want nil", *j.CI)
	}
	// wilson_approx rows carry it too: 5/25 → [0.0886, 0.3913].
	j = Judge(mustDef(t, "verdict.wrong_rate"), rate(5, 25))
	if j.CI == nil {
		t.Fatal("wrong_rate 5/25: CI nil")
	}
	near(t, "5/25 lo", j.CI[0], 0.0886, 1e-4)
	near(t, "5/25 hi", j.CI[1], 0.3913, 1e-4)
	// ci none: never.
	if j := Judge(mustDef(t, "used.share_confident"), rate(10, 40)); j.CI != nil {
		t.Errorf("ci none: CI = %v, want nil", *j.CI)
	}
	if j := Judge(mustDef(t, "recall.latency"), pct(100, 900)); j.CI != nil {
		t.Errorf("percentile: CI = %v, want nil", *j.CI)
	}
}

// boundOf is the good-side bound of k/n, to place a threshold exactly on it.
func boundOf(side string, k, n int64) *float64 {
	lo, hi := Wilson(k, n)
	if side == "hi" {
		return f(hi)
	}
	return f(lo)
}

func TestAbsoluteBound(t *testing.T) {
	runJudge(t, []judgeCase{
		// recall.hit_rate: watch when hi < 0.10.
		{name: "hit 0/40 hi 0.0876 watch", id: "recall.hit_rate", o: rate(0, 40), status: StatusWatch},
		{name: "hit 3/84 hi 0.09982 just below: watch", id: "recall.hit_rate", o: rate(3, 84), status: StatusWatch},
		// 4/98: p 0.0408 is past 0.10, hi 0.10029 is not.
		{name: "hit 4/98 hi 0.10029 just above: ok, borderline", id: "recall.hit_rate", o: rate(4, 98), status: StatusOK, borderline: true},
		{name: "hit 2/40 hi 0.165 ok, borderline", id: "recall.hit_rate", o: rate(2, 40), status: StatusOK, borderline: true},
		// hi exactly on the threshold: < is strict, so ok.
		{name: "hit hi exactly at the threshold: ok", id: "recall.hit_rate",
			o: Observation{Num: 3, Den: 84, Watch: boundOf("hi", 3, 84)}, status: StatusOK, borderline: true},
		// declare.coverage: watch when hi < 0.80.
		{name: "coverage 16/25 hi 0.7975 watch", id: "declare.coverage", o: rate(16, 25), status: StatusWatch},
		{name: "coverage 60/84 hi 0.79985 just below: watch", id: "declare.coverage", o: rate(60, 84), status: StatusWatch},
		{name: "coverage 30/44 hi 0.80001 just above: ok, borderline", id: "declare.coverage", o: rate(30, 44), status: StatusOK, borderline: true},
		{name: "coverage 15/20 (0.75, hi 0.888) ok, borderline", id: "declare.coverage", o: rate(15, 20), status: StatusOK, borderline: true},
		// 20/25 = 0.80: the point is at the threshold, not past it.
		{name: "coverage 20/25 ok, not borderline", id: "declare.coverage", o: rate(20, 25), status: StatusOK},
		// verdict.wrong_rate: watch when lo > 0.05, bad when lo > 0.15.
		{name: "wrong 14/20 lo 0.481 bad", id: "verdict.wrong_rate", o: rate(14, 20), status: StatusBad},
		{name: "wrong 22/100 lo 0.15001 just above bad", id: "verdict.wrong_rate", o: rate(22, 100), status: StatusBad},
		// 15/63: p 0.2381 past 0.15, lo 0.14994 is not; it is past 0.05.
		{name: "wrong 15/63 lo 0.14994 just below bad: watch, borderline", id: "verdict.wrong_rate", o: rate(15, 63), status: StatusWatch, borderline: true},
		{name: "wrong lo exactly at bad: watch, borderline", id: "verdict.wrong_rate",
			edit: func(d *Def) { d.Absolute.Bad = boundOf("lo", 15, 20) }, o: rate(15, 20), status: StatusWatch, borderline: true},
		{name: "wrong 5/25 (0.20, lo 0.0886) watch, borderline", id: "verdict.wrong_rate", o: rate(5, 25), status: StatusWatch, borderline: true},
		// 3/20 = 0.15: at the bad threshold, not past; lo 0.0524 > 0.05.
		{name: "wrong 3/20 (0.15, lo 0.0524) watch, not borderline", id: "verdict.wrong_rate", o: rate(3, 20), status: StatusWatch},
		{name: "wrong 7/69 lo 0.05002 just above watch", id: "verdict.wrong_rate", o: rate(7, 69), status: StatusWatch},
		// 3/21: p 0.1429 past 0.05, lo 0.04981 is not.
		{name: "wrong 3/21 lo 0.04981 just below watch: ok, borderline", id: "verdict.wrong_rate", o: rate(3, 21), status: StatusOK, borderline: true},
		{name: "wrong lo exactly at watch: ok, borderline", id: "verdict.wrong_rate",
			o: Observation{Num: 3, Den: 21, Watch: boundOf("lo", 3, 21)}, status: StatusOK, borderline: true},
		{name: "wrong 2/40 (0.05) ok, not borderline", id: "verdict.wrong_rate", o: rate(2, 40), status: StatusOK},
		// used.rubber_stamp: watch when lo > 0.80; lo 29/30 = 0.8333.
		{name: "rubber 29/30 lo 0.8333 watch", id: "used.rubber_stamp", o: rate(29, 30), status: StatusWatch},
		{name: "rubber 82/93 lo 0.8005 just above: watch", id: "used.rubber_stamp", o: rate(82, 93), status: StatusWatch},
		{name: "rubber 87/99 lo 0.79999 just below: ok, borderline", id: "used.rubber_stamp", o: rate(87, 99), status: StatusOK, borderline: true},
		{name: "rubber 28/30 lo 0.7868 ok, borderline", id: "used.rubber_stamp", o: rate(28, 30), status: StatusOK, borderline: true},
		// Percentiles compare the value, and never set borderline.
		{name: "search latency 3,000 ok", id: "search.latency", o: pct(100, 3000), status: StatusOK},
		{name: "search latency 3,001 watch", id: "search.latency", o: pct(100, 3001), status: StatusWatch},
		{name: "extraction delay 30 min ok", id: "server.extraction_delay", o: pct(100, 1800000), status: StatusOK},
		{name: "extraction delay 30 min + 1 ms watch", id: "server.extraction_delay", o: pct(100, 1800001), status: StatusWatch},
		// A run-time threshold replaces the catalog's.
		{name: "run-time watch replaces the catalog's", id: "recall.latency",
			o: Observation{Den: 100, P95: f(2400), Watch: f(2500)}, status: StatusOK},
		// On "value" compares the value itself (no catalog row uses it yet).
		{name: "on value: at the threshold ok", id: "bank.size",
			edit: func(d *Def) { d.Absolute = Threshold{Watch: f(10), Bad: f(20), On: "value"} }, o: Observation{Value: f(10)}, status: StatusOK},
		{name: "on value: past watch", id: "bank.size",
			edit: func(d *Def) { d.Absolute = Threshold{Watch: f(10), Bad: f(20), On: "value"} }, o: Observation{Value: f(11)}, status: StatusWatch},
		{name: "on value: past bad", id: "bank.size",
			edit: func(d *Def) { d.Absolute = Threshold{Watch: f(10), Bad: f(20), On: "value"} }, o: Observation{Value: f(21)}, status: StatusBad},
	})
}

// turnBase are used.turn_rate's baselines 0.40, 0.45, 0.50, 0.35, each over
// 20 turns: sorted 0.35 0.40 0.45 0.50, median (0.40 + 0.45)/2 = 0.425,
// watch limit 0.7 × 0.425 = 0.2975 on the upper bound.
var turnBase = []BaselineValue{{0.40, 20}, {0.45, 20}, {0.50, 20}, {0.35, 20}}

// emptyBase are search.empty_rate's (lower is better) baselines: median
// (0.10 + 0.20)/2 = 0.15, watch limit 0.15 / 0.7 = 0.2143 on the lower bound.
var emptyBase = []BaselineValue{{0.10, 20}, {0.10, 20}, {0.20, 20}, {0.20, 20}}

func withBase(o Observation, b []BaselineValue) Observation { o.Baseline = b; return o }

func TestRelativeThresholds(t *testing.T) {
	runJudge(t, []judgeCase{
		{name: "turn 2/40 hi 0.165 < 0.2975: watch", id: "used.turn_rate", o: withBase(rate(2, 40), turnBase), status: StatusWatch},
		{name: "turn 9/52 hi 0.29731 just below: watch", id: "used.turn_rate", o: withBase(rate(9, 52), turnBase), status: StatusWatch},
		{name: "turn 10/56 hi 0.29841 just above: ok", id: "used.turn_rate", o: withBase(rate(10, 56), turnBase), status: StatusOK},
		// The point 0.20 is below 0.2975 and hi 0.416 is not: relative
		// comparisons never set borderline.
		{name: "turn 4/20 (0.20, hi 0.416) ok, not borderline", id: "used.turn_rate", o: withBase(rate(4, 20), turnBase), status: StatusOK},
		{name: "turn 30/100 ok", id: "used.turn_rate", o: withBase(rate(30, 100), turnBase), status: StatusOK},
		// 0/100: hi 0.037, far under the limit; still watch, never bad.
		{name: "turn 0/100 watch, never bad", id: "used.turn_rate", o: withBase(rate(0, 100), turnBase), status: StatusWatch},
		// hi exactly at the limit: 20/20 has hi exactly 1; two baselines of
		// 1/0.7 give a median of 1/0.7 and a limit 0.7 × (1/0.7) = 1 exactly
		// in float64. < is strict: ok.
		{name: "turn hi exactly at the limit: ok", id: "used.turn_rate",
			o: withBase(rate(20, 20), []BaselineValue{{1 / 0.7, 20}, {1 / 0.7, 20}}), status: StatusOK},
		// Fewer than two qualifying baselines: no relative status at all.
		{name: "one baseline with den >= 20: no relative status", id: "used.turn_rate",
			o: withBase(rate(0, 100), []BaselineValue{{0.40, 20}, {0.45, 5}, {0.50, 5}, {0.35, 5}}), status: StatusOK},
		{name: "baselines at den 19 do not count", id: "used.turn_rate",
			o: withBase(rate(0, 100), []BaselineValue{{0.40, 20}, {0.45, 19}, {0.50, 19}, {0.35, 19}}), status: StatusOK},
		{name: "no baselines: no relative status", id: "used.turn_rate", o: rate(0, 100), status: StatusOK},
		// Exactly two qualifying (den 20 is min_n): median of 0.40 and 0.50
		// is 0.45, limit 0.315; 0/100 hi 0.037 is under it.
		{name: "two baselines with den >= 20 are enough", id: "used.turn_rate",
			o: withBase(rate(0, 100), []BaselineValue{{0.40, 20}, {0.45, 5}, {0.50, 20}, {0.35, 5}}), status: StatusWatch},
		// Only the qualifying windows enter the median: 0.40 and 0.50 (den
		// 20) give 0.45, limit 0.315. Counting the den-5 windows of 0.05 too
		// would give (0.05 + 0.40)/2 = 0.225, limit 0.1575. 5/40 has hi
		// 0.2611: under 0.315 (watch), over 0.1575 (it would read ok).
		{name: "unqualified windows stay out of the median", id: "used.turn_rate",
			o: withBase(rate(5, 40), []BaselineValue{{0.40, 20}, {0.05, 5}, {0.50, 20}, {0.05, 5}}), status: StatusWatch},
		// Lower is better: watch when lo > 0.2143.
		{name: "empty 10/20 lo 0.2993 > 0.2143: watch", id: "search.empty_rate", o: withBase(rate(10, 20), emptyBase), status: StatusWatch},
		{name: "empty 22/71 lo 0.2144 just above: watch", id: "search.empty_rate", o: withBase(rate(22, 71), emptyBase), status: StatusWatch},
		{name: "empty 18/56 lo 0.21401 just below: ok", id: "search.empty_rate", o: withBase(rate(18, 56), emptyBase), status: StatusOK},
		{name: "empty 6/20 (0.30, lo 0.1455) ok, not borderline", id: "search.empty_rate", o: withBase(rate(6, 20), emptyBase), status: StatusOK},
		// No empty search at all: lo 0.
		{name: "empty 0/20 ok", id: "search.empty_rate", o: withBase(rate(0, 20), emptyBase), status: StatusOK},
		// lo exactly at the limit: baselines of 0 give a limit of 0 / 0.7 =
		// 0; 0/20 has lo exactly 0; > is strict: ok. 1/20 has lo 0.0089: watch.
		{name: "empty lo exactly at the limit: ok", id: "search.empty_rate",
			o: withBase(rate(0, 20), []BaselineValue{{0, 20}, {0, 20}}), status: StatusOK},
		{name: "empty lo just above a zero limit: watch", id: "search.empty_rate",
			o: withBase(rate(1, 20), []BaselineValue{{0, 20}, {0, 20}}), status: StatusWatch},
		// A metric that is not relative ignores baselines: used.share_confident
		// (up, min_n 20, no rule) at 0/40, hi 0.0876, would be under the
		// 0.7 × 0.50 = 0.35 limit if it were relative.
		{name: "baselines on a metric that is not relative are ignored", id: "used.share_confident",
			o: withBase(rate(0, 40), []BaselineValue{{0.50, 40}, {0.50, 40}, {0.50, 40}, {0.50, 40}}), status: StatusOK},
	})
}

func TestWorstOfSteps(t *testing.T) {
	// recall.hit_rate baselines of 0.50 give a limit of 0.35.
	hitBase := []BaselineValue{{0.50, 40}, {0.50, 40}, {0.50, 40}, {0.50, 40}}
	runJudge(t, []judgeCase{
		{name: "error rule bad, no absolute: bad", id: "pipeline.flow", o: Observation{Num: 3, Den: 4, Err: StatusBad}, status: StatusBad},
		{name: "error rule watch: watch", id: "pipeline.flow", o: Observation{Num: 3, Den: 4, Err: StatusWatch}, status: StatusWatch},
		{name: "error rule ok: ok", id: "pipeline.flow", o: Observation{Num: 3, Den: 4, Err: StatusOK}, status: StatusOK},
		{name: "per_session p95 9,001 over run-time 9,000: watch", id: "context.per_session",
			o: Observation{Den: 100, P95: f(9001), Watch: f(9000)}, status: StatusWatch},
		{name: "per_session p95 9,000 at run-time 9,000: ok", id: "context.per_session",
			o: Observation{Den: 100, P95: f(9000), Watch: f(9000)}, status: StatusOK},
		{name: "per_session without a run-time watch has no rule", id: "context.per_session",
			o: Observation{Den: 100, P95: f(1e9)}, status: StatusOK},
		// 4/40: hi 0.2305 >= 0.10 (absolute ok) and < 0.35 (relative watch).
		{name: "relative watch over absolute ok: watch", id: "recall.hit_rate", o: withBase(rate(4, 40), hitBase), status: StatusWatch},
		// 2/40: absolute ok and borderline (p 0.05, hi 0.165); relative watch
		// (0.165 < 0.35). The status is the worst, the flag is step 3's.
		{name: "relative watch keeps step 3's borderline", id: "recall.hit_rate", o: withBase(rate(2, 40), hitBase), status: StatusWatch, borderline: true},
		// 0/40: absolute watch (hi 0.0876), relative watch: watch.
		{name: "absolute watch and relative watch: watch", id: "recall.hit_rate", o: withBase(rate(0, 40), hitBase), status: StatusWatch},
		// Absolute bad beats an error-rule watch, and an error-rule bad beats
		// an absolute watch (synthetic rows: no catalog metric has both).
		{name: "absolute bad over error watch: bad", id: "recall.latency", o: Observation{Den: 100, P95: f(2501), Err: StatusWatch}, status: StatusBad},
		{name: "error bad over absolute watch: bad", id: "recall.latency", o: Observation{Den: 100, P95: f(2001), Err: StatusBad}, status: StatusBad},
		{name: "error watch over absolute ok: watch", id: "recall.latency", o: Observation{Den: 100, P95: f(1000), Err: StatusWatch}, status: StatusWatch},
	})
}

func TestNoRuleIsOkStaleIsInfo(t *testing.T) {
	runJudge(t, []judgeCase{
		{name: "empty_reasons den 0 ok (min_n 0)", id: "recall.empty_reasons", o: Observation{}, status: StatusOK},
		{name: "a ratio with no rule ok", id: "used.share_confident", o: rate(0, 40), status: StatusOK},
		{name: "stale.hidden info", id: "stale.hidden", o: Observation{Value: f(3)}, status: StatusInfo},
		{name: "stale.never_used info", id: "stale.never_used", o: Observation{Value: f(0)}, status: StatusInfo},
		{name: "stale.reported info", id: "stale.reported", o: Observation{Value: f(12)}, status: StatusInfo},
	})
}

// TestJudgeEveryCatalogRow runs steps 1 and 2 over the whole catalog: every
// row reads no_data when absent, and with no data at all too_few above a
// minimum n, info for the stale lists and ok otherwise.
func TestJudgeEveryCatalogRow(t *testing.T) {
	for _, d := range Catalog {
		if j := Judge(d, Observation{Absent: true, Reason: "not_built"}); j.Status != StatusNoData || j.Reason != "not_built" {
			t.Errorf("%s absent: %s %q, want no_data not_built", d.ID, j.Status, j.Reason)
		}
		want := StatusOK
		switch {
		case d.MinN > 0:
			want = StatusTooFew
		case strings.HasPrefix(d.ID, "stale."):
			want = StatusInfo
		}
		if j := Judge(d, Observation{}); j.Status != want || j.Borderline || j.CI != nil {
			t.Errorf("%s with nothing: %+v, want %s, no borderline, no CI", d.ID, j, want)
		}
	}
}

func TestWorst(t *testing.T) {
	for _, c := range []struct{ a, b, want Status }{
		{StatusOK, StatusOK, StatusOK},
		{StatusOK, StatusWatch, StatusWatch},
		{StatusWatch, StatusOK, StatusWatch},
		{StatusWatch, StatusBad, StatusBad},
		{StatusBad, StatusWatch, StatusBad},
		{StatusBad, StatusOK, StatusBad},
		{"", StatusOK, StatusOK},
		{StatusWatch, "", StatusWatch},
	} {
		if got := worst(c.a, c.b); got != c.want {
			t.Errorf("worst(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}
