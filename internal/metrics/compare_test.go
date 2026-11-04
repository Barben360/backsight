package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/telemetry"
)

// cmpDay is 10:00 Paris on October 1+d, 2026.
func cmpDay(d int) time.Time { return time.Date(2026, 10, 1+d, 10, 0, 0, 0, fxParis) }

// cmpGen builds the units of a comparison fixture.
type cmpGen struct{ units []cmpUnit }

// add appends n units of version in (project, path), the first succ of
// them successes, spread over days consecutive active days starting at day
// start, each in a session of its own (repeated rep times in that
// session: rep 2 doubles the units and leaves the sessions as they are).
func (g *cmpGen) add(version, project, path string, n, succ, days, start, rep int) {
	for i := range n {
		for range rep {
			g.units = append(g.units, cmpUnit{version: version, project: project, path: path,
				session: fmt.Sprintf("%s/%s/%s/%d", version, project, path, i),
				at:      cmpDay(start + i%days), hit: i < succ})
		}
	}
}

// recordFixture is the design's fixture of record (turn rate, v16 to v17,
// one unit per session), with every unit repeated rep times.
func recordFixture(rep int) []cmpUnit {
	var g cmpGen
	const a, b = "v0.1.16-m7", "v0.1.17-m7"
	g.add(a, "backsight", "prompt", 200, 140, 21, 0, rep)
	g.add(b, "backsight", "prompt", 60, 46, 9, 0, rep)
	g.add(a, "project-a", "prompt", 70, 21, 18, 0, rep)
	g.add(b, "project-a", "prompt", 190, 87, 9, 0, rep)
	g.add(a, "backsight", "search", 36, 17, 20, 0, rep)
	g.add(b, "backsight", "search", 12, 6, 7, 0, rep) // too few turns
	g.add(a, "project-b", "prompt", 20, 9, 4, 0, rep) // too few days
	g.add(b, "project-b", "prompt", 33, 15, 8, 0, rep)
	return g.units
}

func nearP(t *testing.T, name string, got *float64, want, tol float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = null, want %v", name, want)
		return
	}
	if math.Abs(*got-want) > tol {
		t.Errorf("%s = %.9f, want %.6f (+-%g)", name, *got, want, tol)
	}
}

func ci(t *testing.T, name string, got *[2]float64, lo, hi, tol float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = null, want [%v, %v]", name, lo, hi)
		return
	}
	nearP(t, name+"[0]", &got[0], lo, tol)
	nearP(t, name+"[1]", &got[1], hi, tol)
}

// noNaN fails for a NaN or an infinity anywhere in the comparison's JSON
// numbers (encoding/json refuses them, so a clean Marshal is the proof).
func noNaN(t *testing.T, c Comparison) {
	t.Helper()
	if _, err := json.Marshal(c); err != nil {
		t.Fatalf("comparison does not marshal (NaN or Inf?): %v", err)
	}
	for _, f := range []*float64{c.Diff, c.PooledA, c.PooledB} {
		if f != nil && (math.IsNaN(*f) || math.IsInf(*f, 0)) {
			t.Fatalf("non-finite number in %+v", c)
		}
	}
}

// The design's fixture of record, hand-computed. Eligible strata: w1 =
// 200*60/260 = 46.1538 (backsight, prompt), w2 = 70*190/260 = 51.1538
// (project-a, prompt). d1 = 46/60 - 140/200 = +0.066667, d2 = 87/190 -
// 21/70 = +0.157895; D = (w1 d1 + w2 d2) / (w1 + w2) = 11.15385 / 97.30769 =
// 0.114625. P_A = (w1 .7 + w2 .3) / W = 0.489723, P_B = 0.604348. One unit
// per session: G = 520, SE(D) = 0.045838, so 0.114625 +- 1.96 SE =
// 0.024783 to 0.204466. Raw: 187/326 = 0.573620 -> 154/295 = 0.522034.
func TestCompareSimpson(t *testing.T) {
	c := compareOf(recordFixture(1), "v0.1.16-m7", "v0.1.17-m7", "used.turn_rate", fxParis)
	noNaN(t, c)
	if c.Reading != "differs" || c.TooFew != nil {
		t.Fatalf("reading %q, too few %+v", c.Reading, c.TooFew)
	}
	nearP(t, "diff", c.Diff, 0.114625, 1e-6)
	ci(t, "ci", c.CI, 0.024783, 0.204466, 1e-6)
	nearP(t, "pooled a", c.PooledA, 0.489723, 1e-6)
	nearP(t, "pooled b", c.PooledB, 0.604348, 1e-6)
	if c.RawA != (Rate{187, 326}) || c.RawB != (Rate{154, 295}) || !c.RawDisagrees {
		t.Errorf("raw %+v -> %+v disagrees=%v, want 187/326 -> 154/295, true", c.RawA, c.RawB, c.RawDisagrees)
	}
	if c.SessionsA != 270 || c.SessionsB != 250 {
		t.Errorf("sessions %d/%d, want 270/250", c.SessionsA, c.SessionsB)
	}
	type want struct {
		project, path, reading, why string
		eligible                    bool
		weight, diff, lo, hi        float64
		a, b                        StratumSide
	}
	for i, w := range []want{
		{"backsight", "prompt", "no_clear_difference", "", true, 46.1538, 0.066667, -0.058021, 0.191355, StratumSide{140, 200, 21}, StratumSide{46, 60, 9}},
		{"backsight", "search", "too_few", "units", false, 0, 0, 0, 0, StratumSide{17, 36, 20}, StratumSide{6, 12, 7}},
		{"project-a", "prompt", "differs", "", true, 51.1538, 0.157895, 0.029024, 0.286765, StratumSide{21, 70, 18}, StratumSide{87, 190, 9}},
		{"project-b", "prompt", "too_few", "days", false, 0, 0, 0, 0, StratumSide{9, 20, 4}, StratumSide{15, 33, 8}},
	} {
		if i >= len(c.Strata) {
			t.Fatalf("%d strata, want 4", len(c.Strata))
		}
		s := c.Strata[i]
		if s.Project != w.project || s.Path != w.path || s.Reading != w.reading || s.Why != w.why || s.Eligible != w.eligible || s.A != w.a || s.B != w.b {
			t.Errorf("stratum %d = %+v, want %+v", i, s, w)
		}
		if !w.eligible {
			if s.Weight != nil || s.Diff != nil || s.CI != nil {
				t.Errorf("stratum %d is not eligible but has figures: %+v", i, s)
			}
			continue
		}
		nearP(t, "weight", s.Weight, w.weight, 1e-4)
		nearP(t, "stratum diff", s.Diff, w.diff, 1e-6)
		ci(t, "stratum ci", s.CI, w.lo, w.hi, 1e-6)
	}
	if len(c.Strata) != 4 {
		t.Errorf("%d strata, want 4", len(c.Strata))
	}
}

// Every session holds two identical units: n doubles, the sessions stay.
// A variance that counted units as independent would shrink by sqrt(2);
// the clustered interval and the difference do not move.
func TestCompareClustersBySession(t *testing.T) {
	one := compareOf(recordFixture(1), "v0.1.16-m7", "v0.1.17-m7", "used.turn_rate", fxParis)
	two := compareOf(recordFixture(2), "v0.1.16-m7", "v0.1.17-m7", "used.turn_rate", fxParis)
	if two.Reading != "differs" || one.Diff == nil || two.Diff == nil || one.CI == nil || two.CI == nil {
		t.Fatalf("one %+v two %+v", one, two)
	}
	nearP(t, "diff", two.Diff, *one.Diff, 1e-9)
	ci(t, "ci", two.CI, one.CI[0], one.CI[1], 1e-9)
	if two.SessionsA != one.SessionsA || two.SessionsB != one.SessionsB {
		t.Errorf("sessions changed: %d/%d -> %d/%d", one.SessionsA, one.SessionsB, two.SessionsA, two.SessionsB)
	}
	if two.Strata[0].A.Den != 400 {
		t.Errorf("units were not doubled: %+v", two.Strata[0])
	}
	// The strata's own intervals are clustered the same way.
	ci(t, "stratum ci", two.Strata[0].CI, one.Strata[0].CI[0], one.Strata[0].CI[1], 1e-9)
}

func TestCompareTooFew(t *testing.T) {
	const a, b = "v17", "v18"
	read := func(g cmpGen) Comparison {
		t.Helper()
		c := compareOf(g.units, a, b, "used.turn_rate", fxParis)
		noNaN(t, c)
		if c.Reading != "too_few" || c.TooFew == nil || c.Diff != nil || c.CI != nil || c.PooledA != nil {
			t.Fatalf("want too_few without figures, got %+v", c)
		}
		return c
	}

	t.Run("days and units both unmet: days first, with a retry day", func(t *testing.T) {
		var g cmpGen
		g.add(a, "p", "prompt", 40, 20, 10, 0, 1)
		g.add(b, "p", "prompt", 11, 5, 3, 10, 1) // 3 days from day 10, 11 turns
		c := read(g)
		tf := c.TooFew
		if tf.Version != b || tf.Why != "days" || tf.Days != 3 || tf.Units != 11 {
			t.Errorf("too few = %+v, want v18, days, 3 days, 11 units", tf)
		}
		if want := time.Date(2026, 10, 17, 0, 0, 0, 0, fxParis); tf.RetryAfter == nil || !tf.RetryAfter.Equal(want) {
			t.Errorf("retry after %v, want %v (first active day + 6 days)", tf.RetryAfter, want)
		}
	})

	t.Run("enough days, not enough units: no retry day", func(t *testing.T) {
		var g cmpGen
		g.add(a, "p", "prompt", 40, 20, 10, 0, 1)
		g.add(b, "p", "prompt", 20, 5, 10, 10, 1)
		tf := read(g).TooFew
		if tf.Version != b || tf.Why != "units" || tf.Units != 20 || tf.Days != 10 || tf.RetryAfter != nil {
			t.Errorf("too few = %+v, want v18, units, 20, 10 days, no retry", tf)
		}
	})

	t.Run("fewer than 10 sessions in a version: sessions", func(t *testing.T) {
		var g cmpGen
		for _, v := range []string{a, b} {
			for i := range 40 { // 40 units in 8 sessions, 8 days
				g.units = append(g.units, cmpUnit{version: v, project: "p", path: "prompt",
					session: fmt.Sprintf("%s-%d", v, i%8), at: cmpDay(i % 8), hit: i%2 == 0})
			}
		}
		c := read(g)
		if c.TooFew.Why != "sessions" || c.TooFew.RetryAfter != nil {
			t.Errorf("too few = %+v, want sessions", c.TooFew)
		}
		if !c.Strata[0].Eligible {
			t.Errorf("a stratum with 8 sessions per version is eligible on its own: %+v", c.Strata[0])
		}
	})

	t.Run("a stratum in one session per version is not eligible", func(t *testing.T) {
		var g cmpGen
		g.add(a, "good", "prompt", 40, 10, 10, 0, 1)
		g.add(b, "good", "prompt", 40, 20, 10, 0, 1)
		for _, v := range []string{a, b} {
			for i := range 40 {
				g.units = append(g.units, cmpUnit{version: v, project: "one", path: "prompt", session: "only-" + v, at: cmpDay(i % 8), hit: i%2 == 0})
			}
		}
		c := compareOf(g.units, a, b, "used.turn_rate", fxParis)
		noNaN(t, c)
		if c.Reading != "differs" || len(c.Strata) != 2 {
			t.Fatalf("reading %q over %d strata", c.Reading, len(c.Strata))
		}
		one := c.Strata[1]
		if one.Project != "one" || one.Eligible || one.Why != "sessions" || one.Reading != "too_few" || one.Weight != nil || one.Diff != nil {
			t.Errorf("single-session stratum = %+v", one)
		}
		nearP(t, "diff comes from the good stratum alone", c.Diff, 0.25, 1e-12)
	})

	t.Run("a version with no unit at all", func(t *testing.T) {
		var g cmpGen
		g.add(a, "p", "prompt", 40, 20, 10, 0, 1)
		c := read(g)
		tf := c.TooFew
		if tf.Version != b || tf.Why != "days" || tf.Days != 0 || tf.Units != 0 || tf.RetryAfter != nil {
			t.Errorf("too few = %+v, want v18 with 0 days and 0 units", tf)
		}
		if c.RawA != (Rate{20, 40}) || c.RawB != (Rate{0, 0}) {
			t.Errorf("raw %+v %+v", c.RawA, c.RawB)
		}
	})

	t.Run("no unit in either", func(t *testing.T) {
		c := read(cmpGen{})
		if c.TooFew.Why != "days" || c.TooFew.Days != 0 || c.TooFew.Units != 0 || len(c.Strata) != 0 {
			t.Errorf("%+v", c)
		}
	})
}

// recall.hit_rate counts the prompts that recalled (hit: injected >= 1; a
// skipped prompt is no unit), verdict.wrong_rate counts verdicts (hit:
// wrong or outdated), used.turn_rate declared turns that showed memory
// (hit: a used credit); each read from the real index by the version of
// its row (a journal unit by its turn's).
func TestCompareMeasures(t *testing.T) {
	const a, b = "0.1.16", "0.1.17"
	// 30 units per version over 8 days, each in its own session; A has 12
	// hits (40 %), B has 21 (70 %): +30 pts, the CI is the one hand-computed
	// below from p(1-p)/n.
	days := func(i int) time.Time { return time.Date(2026, 10, 12+i%8, 9+i/8, 0, 0, 0, fxParis) }
	hits := map[string]int{a: 12, b: 21}
	check := func(t *testing.T, c Comparison, measure string) {
		t.Helper()
		noNaN(t, c)
		if c.Measure != measure || c.Reading != "differs" || len(c.Strata) != 1 {
			t.Fatalf("%+v", c)
		}
		s := c.Strata[0]
		if s.A != (StratumSide{12, 30, 8}) || s.B != (StratumSide{21, 30, 8}) || !s.Eligible {
			t.Errorf("stratum %+v, want A 12/30 8 d, B 21/30 8 d", s)
		}
		// Var = (G/(G-1)) (pa(1-pa)/30 + pb(1-pb)/30), G = 60.
		se := math.Sqrt(60.0 / 59 * (0.4*0.6/30 + 0.7*0.3/30))
		nearP(t, "diff", c.Diff, 0.3, 1e-9)
		ci(t, "ci", c.CI, 0.3-1.96*se, 0.3+1.96*se, 1e-9)
		if c.RawDisagrees {
			t.Error("raw agrees here")
		}
	}

	t.Run("recall.hit_rate: prompt recalls only, path prompt", func(t *testing.T) {
		x := newFx(t, fxNow)
		for _, v := range []string{a, b} {
			x.binary = v
			for i := range 30 {
				inj := 0
				if i < hits[v] {
					inj = 2
				}
				x.tel(days(i), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, Injected: inj}, fmt.Sprintf("%s-%d", v, i), "alpha")
			}
			// Prompts that are not recalls are no unit.
			x.tel(days(0), telemetry.Prompt{N: 2, Decision: "skip", Reason: "short"}, "skip-"+v, "alpha")
		}
		q := x.query(30)
		q.Compare, q.Measure = [2]string{a, b}, "recall.hit_rate"
		c := x.run("releases", q).Tables.Compare
		if c == nil {
			t.Fatal("no comparison")
		}
		check(t, *c, "recall.hit_rate")
		if c.Strata[0].Project != "alpha" || c.Strata[0].Path != "prompt" {
			t.Errorf("stratum %+v", c.Strata[0])
		}
	})

	journalUnits := func(x *fx, withStop bool, verdict func(hit bool) string) {
		for _, v := range []string{a, b} {
			x.binary = v
			for i := range 30 {
				s := fmt.Sprintf("%s-%d", v, i)
				at := days(i)
				x.turnEvent(at.Add(-time.Second), s, "alpha")
				x.shows(at, s, fxTurn, "", "alpha", "prompt", "", "m1")
				x.credit(at.Add(30*time.Second), s, "", "alpha", verdict(i < hits[v]), "m1")
				if withStop {
					x.stop(at.Add(time.Minute), s, "", "declared")
				}
			}
		}
	}

	t.Run("verdict.wrong_rate: verdicts, wrong or outdated is the hit", func(t *testing.T) {
		x := newFx(t, fxNow)
		journalUnits(x, false, func(hit bool) string {
			if hit {
				return "wrong"
			}
			return "used"
		})
		// Verdicts of other kinds are no unit: a credit that names nobody.
		x.binary = a
		x.journal(feedback.Event{TS: days(0), Kind: "credit", Session: "x", Turn: fxTurn, Project: "alpha", Verdict: "none"})
		q := x.query(30)
		q.Compare, q.Measure = [2]string{a, b}, "verdict.wrong_rate"
		c := x.run("releases", q).Tables.Compare
		check(t, *c, "verdict.wrong_rate")
	})

	t.Run("used.turn_rate: declared turns, a used credit is the hit", func(t *testing.T) {
		x := newFx(t, fxNow)
		journalUnits(x, true, func(hit bool) string {
			if hit {
				return "used"
			}
			return "none"
		})
		// An undeclared turn is no unit.
		x.binary = b
		x.turnEvent(days(0), "undeclared", "alpha")
		x.shows(days(0).Add(time.Second), "undeclared", fxTurn, "", "alpha", "prompt", "", "m1")
		x.stop(days(0).Add(time.Minute), "undeclared", "", "undeclared")
		q := x.query(30)
		q.Compare = [2]string{a, b} // the measure defaults to used.turn_rate
		c := x.run("releases", q).Tables.Compare
		check(t, *c, "used.turn_rate")
	})

	t.Run("the window bounds the units", func(t *testing.T) {
		x := newFx(t, fxNow)
		journalUnits(x, true, func(hit bool) string {
			if hit {
				return "used"
			}
			return "none"
		})
		q := x.query(3) // the units are from the 12th to the 19th: none left
		q.Compare = [2]string{a, b}
		c := x.run("releases", q).Tables.Compare
		if c.Reading != "too_few" || c.TooFew == nil || c.TooFew.Units != 0 || c.TooFew.Days != 0 {
			t.Errorf("%+v", c)
		}
	})

	t.Run("no clear difference: +1 pt, -4 to +6", func(t *testing.T) {
		// 800 units per version, each in its own session: A 400 (50.0 %),
		// B 408 (51.0 %). Var = 1600/1599 (.5*.5/800 + .51*.49/800) =
		// .00062505, SE .025001; D = .01 +- .049002.
		var g cmpGen
		g.add("v1", "p", "prompt", 800, 400, 20, 0, 1)
		g.add("v2", "p", "prompt", 800, 408, 20, 0, 1)
		c := compareOf(g.units, "v1", "v2", "recall.hit_rate", fxParis)
		noNaN(t, c)
		if c.Reading != "no_clear_difference" {
			t.Fatalf("reading %q", c.Reading)
		}
		nearP(t, "diff", c.Diff, 0.01, 1e-12)
		se := math.Sqrt(1600.0 / 1599 * (0.5*0.5/800 + 0.51*0.49/800))
		ci(t, "ci", c.CI, 0.01-1.96*se, 0.01+1.96*se, 1e-12)
		if lo, hi := math.Round(c.CI[0]*100), math.Round(c.CI[1]*100); lo != -4 || hi != 6 {
			t.Errorf("interval reads %v to %v pts, want -4 to +6", lo, hi)
		}
	})
}

func TestCompareSettings(t *testing.T) {
	const a, b = "0.1.16", "0.1.17"
	prompts := func(x *fx) {
		x.promptAt(mk(12, 9, 0), a)
		x.promptAt(mk(14, 9, 0), a)
		x.promptAt(mk(21, 9, 0), b)
		x.promptAt(mk(23, 9, 0), b)
	}
	query := func(x *fx) Query {
		q := x.query(30)
		q.Compare, q.Measure = [2]string{a, b}, "recall.hit_rate"
		return q
	}

	x := newFx(t, fxNow)
	x.configAt(mk(10, 8, 0), "a", map[string]float64{"recall.min_score": 0.5, "recall.k": 8})
	x.configAt(mk(20, 9, 30), "b", map[string]float64{"recall.min_score": 0.55, "recall.k": 8})
	prompts(x)
	c := x.run("releases", query(x)).Tables.Compare
	if c.SettingsUnknown || len(c.SettingsDiffer) != 1 {
		t.Fatalf("unknown %v, differ %+v, want one difference", c.SettingsUnknown, c.SettingsDiffer)
	}
	d := c.SettingsDiffer[0]
	if d.Key != "recall.min_score" || d.A == nil || *d.A != 0.5 || d.B == nil || *d.B != 0.55 || d.ChangedAt == nil || !d.ChangedAt.Equal(mk(20, 9, 30)) {
		t.Errorf("difference = %+v, want recall.min_score 0.5 -> 0.55 changed 2026-10-20 09:30", d)
	}

	// Both versions under the same record: nothing differs, and the list is [] not null.
	y := newFx(t, fxNow)
	y.configAt(mk(10, 8, 0), "a", map[string]float64{"recall.min_score": 0.5})
	prompts(y)
	c = y.run("releases", query(y)).Tables.Compare
	raw, _ := json.Marshal(c)
	if c.SettingsUnknown || len(c.SettingsDiffer) != 0 || c.SettingsDiffer == nil {
		t.Errorf("same record: %s", raw)
	}

	// No config row in or before A's span.
	z := newFx(t, fxNow)
	z.configAt(mk(20, 9, 30), "b", map[string]float64{"recall.min_score": 0.55})
	prompts(z)
	c = z.run("releases", query(z)).Tables.Compare
	if !c.SettingsUnknown || len(c.SettingsDiffer) != 0 {
		t.Errorf("no config before A: unknown %v differ %+v", c.SettingsUnknown, c.SettingsDiffer)
	}
}

// RF 4: degenerate comparisons never produce a NaN or an infinity.
func TestCompareDegenerate(t *testing.T) {
	const a, b = "v1", "v2"
	t.Run("both at 0 %", func(t *testing.T) {
		var g cmpGen
		g.add(a, "p", "prompt", 40, 0, 10, 0, 1)
		g.add(b, "p", "prompt", 40, 0, 10, 0, 1)
		c := compareOf(g.units, a, b, "verdict.wrong_rate", fxParis)
		noNaN(t, c)
		if c.Reading != "no_clear_difference" || c.Diff == nil || *c.Diff != 0 || c.CI == nil || *c.CI != [2]float64{0, 0} {
			t.Errorf("%+v", c)
		}
		if c.RawDisagrees {
			t.Error("raw 0 and D 0 agree")
		}
		if s := c.Strata[0]; s.Reading != "no_clear_difference" || s.CI == nil || *s.CI != [2]float64{0, 0} {
			t.Errorf("stratum %+v", s)
		}
	})
	t.Run("both at 100 %", func(t *testing.T) {
		var g cmpGen
		g.add(a, "p", "prompt", 40, 40, 10, 0, 1)
		g.add(b, "p", "prompt", 40, 40, 10, 0, 1)
		c := compareOf(g.units, a, b, "recall.hit_rate", fxParis)
		noNaN(t, c)
		if c.Reading != "no_clear_difference" || *c.Diff != 0 || *c.CI != [2]float64{0, 0} {
			t.Errorf("%+v", c)
		}
	})
	t.Run("0 % against 100 %: a zero-width interval that excludes 0", func(t *testing.T) {
		var g cmpGen
		g.add(a, "p", "prompt", 30, 0, 10, 0, 1)
		g.add(b, "p", "prompt", 30, 30, 10, 0, 1)
		// 15 sessions per version, two units each.
		for i := range g.units {
			g.units[i].session = fmt.Sprintf("%s-%d", g.units[i].version, i%30/2)
		}
		c := compareOf(g.units, a, b, "recall.hit_rate", fxParis)
		noNaN(t, c)
		if c.Reading != "differs" || *c.Diff != 1 || *c.CI != [2]float64{1, 1} {
			t.Errorf("%+v", c)
		}
		if c.SessionsA != 15 || c.SessionsB != 15 {
			t.Errorf("sessions %d/%d", c.SessionsA, c.SessionsB)
		}
	})
	t.Run("every unit in one session per version", func(t *testing.T) {
		var g cmpGen
		g.add(a, "p", "prompt", 40, 10, 10, 0, 1)
		g.add(b, "p", "prompt", 40, 30, 10, 0, 1)
		for i := range g.units {
			g.units[i].session = g.units[i].version
		}
		c := compareOf(g.units, a, b, "recall.hit_rate", fxParis)
		noNaN(t, c)
		if c.Reading != "too_few" || c.TooFew == nil || c.TooFew.Why != "sessions" {
			t.Errorf("%+v", c)
		}
	})
	t.Run("no shared stratum", func(t *testing.T) {
		var g cmpGen
		g.add(a, "p", "prompt", 40, 10, 10, 0, 1)
		g.add(b, "q", "prompt", 40, 30, 10, 0, 1)
		c := compareOf(g.units, a, b, "recall.hit_rate", fxParis)
		noNaN(t, c)
		if c.Reading != "too_few" || c.TooFew == nil || len(c.Strata) != 2 {
			t.Fatalf("%+v", c)
		}
		for _, s := range c.Strata {
			if s.Eligible || s.Weight != nil || s.Why != "days" {
				t.Errorf("stratum %+v", s)
			}
		}
		if c.RawA != (Rate{10, 40}) || c.RawB != (Rate{30, 40}) {
			t.Errorf("raw %+v %+v", c.RawA, c.RawB)
		}
	})
	t.Run("a version name with <", func(t *testing.T) {
		var g cmpGen
		g.add("<b>1", "<p>", "prompt", 40, 10, 10, 0, 1)
		g.add("v2&", "<p>", "prompt", 40, 30, 10, 0, 1)
		c := compareOf(g.units, "<b>1", "v2&", "recall.hit_rate", fxParis)
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var back Comparison
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if back.A != "<b>1" || back.B != "v2&" || back.Strata[0].Project != "<p>" {
			t.Errorf("round trip lost the names: %+v", back)
		}
	})
}

// A session that holds units in two strata is one cluster: its terms add up
// before they are squared. 40 sessions per version, each with one unit in
// "prompt" and one in "search" and the same outcome in both; A hits 20 of
// them, B 30. w = 20 per stratum, W = 40, c = +-1/80, so a session's term
// is +-(y - p)/40 (the two strata alike): sum of squares = 40 (.5/40)^2 +
// 30 (.25/40)^2 + 10 (.75/40)^2 = .0109375, G = 80, Var = 80/79 * that.
// Clustering each stratum on its own would halve the sum and shrink SE by
// sqrt(2).
func TestCompareSessionAcrossStrata(t *testing.T) {
	var units []cmpUnit
	for _, v := range []struct {
		name string
		hits int
	}{{"v1", 20}, {"v2", 30}} {
		for i := range 40 {
			for _, path := range []string{"prompt", "search"} {
				units = append(units, cmpUnit{version: v.name, project: "p", path: path,
					session: fmt.Sprintf("%s-%d", v.name, i), at: cmpDay(i % 8), hit: i < v.hits})
			}
		}
	}
	c := compareOf(units, "v1", "v2", "used.turn_rate", fxParis)
	noNaN(t, c)
	se := math.Sqrt(80.0 / 79 * 0.0109375)
	nearP(t, "diff", c.Diff, 0.25, 1e-12)
	ci(t, "ci", c.CI, 0.25-1.96*se, 0.25+1.96*se, 1e-12)
	if c.SessionsA != 40 || c.SessionsB != 40 {
		t.Errorf("sessions %d/%d, want 40/40 (a session is counted once across strata)", c.SessionsA, c.SessionsB)
	}
}

// The stratum of a unit is its turn's earliest non-briefing main-agent show:
// a later show does not move it, a briefing show or a subagent's is skipped,
// shows at the same instant go in the order prompt, gate, search, read,
// failure, sources; a verdict whose turn has no main-agent show is in
// "unknown". The project filter keeps the units of that project.
func TestCompareStratumPath(t *testing.T) {
	const v = "0.1.16"
	x := newFx(t, fxNow)
	x.binary = v
	t0 := mk(15, 10, 0)
	turn := func(session, project string, build func(at time.Time)) {
		x.turnEvent(t0.Add(-time.Second), session, project)
		build(t0)
		x.credit(t0.Add(30*time.Second), session, "", project, "used", "m1")
		x.stop(t0.Add(5*time.Minute), session, "", "declared")
	}
	turn("later-prompt", "alpha", func(at time.Time) { // search first, prompt later: search
		x.shows(at, "later-prompt", fxTurn, "", "alpha", "search", "", "m1")
		x.shows(at.Add(time.Minute), "later-prompt", fxTurn, "", "alpha", "prompt", "", "m1")
	})
	turn("tie", "alpha", func(at time.Time) { // same instant: gate before search
		x.shows(at, "tie", fxTurn, "", "alpha", "search", "", "m1")
		x.shows(at, "tie", fxTurn, "", "alpha", "gate", "", "m1")
	})
	turn("briefing-first", "alpha", func(at time.Time) { // briefing is skipped: read
		x.shows(at.Add(-time.Minute), "briefing-first", fxTurn, "", "alpha", "briefing", "", "m1")
		x.shows(at, "briefing-first", fxTurn, "", "alpha", "read", "", "m1")
	})
	turn("subagent-first", "alpha", func(at time.Time) { // a subagent's show is skipped: failure
		x.shows(at.Add(-time.Minute), "subagent-first", fxTurn, "sub", "alpha", "prompt", "", "m1")
		x.shows(at, "subagent-first", fxTurn, "", "alpha", "failure", "", "m1")
	})
	turn("other-project", "beta", func(at time.Time) {
		x.shows(at, "other-project", fxTurn, "", "beta", "prompt", "", "m1")
	})
	// a verdict on a turn with only a subagent show
	x.turnEvent(t0.Add(-time.Second), "sub-only", "alpha")
	x.shows(t0, "sub-only", fxTurn, "sub", "alpha", "prompt", "", "m1")
	x.credit(t0.Add(30*time.Second), "sub-only", "", "alpha", "wrong", "m1")

	read := func(measure, project string) map[string]string {
		t.Helper()
		q := x.query(30)
		q.Project = project
		var units []cmpUnit
		err := withTx(t.Context(), x.source(), q, nil, func(e env) error {
			var err error
			units, err = compareUnits(e, [2]string{v, "other"}, measure)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, u := range units {
			got[u.session] = u.project + "/" + u.path
		}
		return got
	}
	eq := func(name string, got, want map[string]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: %v, want %v", name, got, want)
			return
		}
		for k, w := range want {
			if got[k] != w {
				t.Errorf("%s: %v, want %v", name, got, want)
				return
			}
		}
	}
	eq("turn rate", read("used.turn_rate", ""), map[string]string{
		"later-prompt": "alpha/search", "tie": "alpha/gate", "briefing-first": "alpha/read",
		"subagent-first": "alpha/failure", "other-project": "beta/prompt"})
	eq("turn rate, project alpha", read("used.turn_rate", "alpha"), map[string]string{
		"later-prompt": "alpha/search", "tie": "alpha/gate", "briefing-first": "alpha/read", "subagent-first": "alpha/failure"})
	eq("verdicts", read("verdict.wrong_rate", "alpha"), map[string]string{
		"later-prompt": "alpha/search", "tie": "alpha/gate", "briefing-first": "alpha/read",
		"subagent-first": "alpha/failure", "sub-only": "alpha/unknown"})
	eq("verdicts, project beta", read("verdict.wrong_rate", "beta"), map[string]string{"other-project": "beta/prompt"})
}

// A setting that changes inside a version's span is flagged even when both
// spans start under the same record.
func TestCompareSettingsMidSpan(t *testing.T) {
	const a, b = "0.1.16", "0.1.17"
	x := newFx(t, fxNow)
	x.configAt(mk(10, 8, 0), "a", map[string]float64{"recall.min_score": 0.5, "recall.k": 8})
	x.configAt(mk(22, 9, 30), "b", map[string]float64{"recall.min_score": 0.55, "recall.k": 8}) // inside B's span
	x.promptAt(mk(12, 9, 0), a)
	x.promptAt(mk(14, 9, 0), a)
	x.promptAt(mk(21, 9, 0), b) // B starts under the record A ran under
	x.promptAt(mk(23, 9, 0), b)
	q := x.query(30)
	q.Compare, q.Measure = [2]string{a, b}, "recall.hit_rate"
	c := x.run("releases", q).Tables.Compare
	if c.SettingsUnknown || len(c.SettingsDiffer) != 1 {
		t.Fatalf("unknown %v, differ %+v, want recall.min_score only", c.SettingsUnknown, c.SettingsDiffer)
	}
	d := c.SettingsDiffer[0]
	if d.Key != "recall.min_score" || d.A == nil || *d.A != 0.5 || d.B == nil || *d.B != 0.55 || d.ChangedAt == nil || !d.ChangedAt.Equal(mk(22, 9, 30)) {
		t.Errorf("difference = %+v, want recall.min_score 0.5 -> 0.55 changed 2026-10-22 09:30", d)
	}
}
