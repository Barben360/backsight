package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/telemetry"
)

// outcomes writes count outcomes of project, one per minute going back from
// at in sessions prefix-<i>; ids gives each its memories, contradicted
// marks the first n of them.
func (x *fx) outcomes(project, prefix string, at time.Time, count, contradicted int, ids []string) {
	x.t.Helper()
	for i := range count {
		x.tel(at.Add(-time.Duration(i+1)*time.Minute), telemetry.Outcome{N: 1, IDs: ids, Contradicted: i < contradicted},
			fmt.Sprintf("%s-%d", prefix, i), project)
	}
}

// TestContradicted: the corrections after memory was shown, out of the
// outcomes that had memory to follow (a prompt that injected nothing is not
// one), in the window and the project asked.
func TestContradicted(t *testing.T) {
	x := newFx(t, fxNow)
	x.outcomes("alpha", "a", fxNow, 167, 5, []string{"m1"})
	x.outcomes("alpha", "none", fxNow, 4, 4, nil)                                // nothing injected: not counted either way
	x.outcomes("beta", "b", fxNow, 3, 2, []string{"m2"})                         // another project
	x.outcomes("alpha", "old", fxNow.Add(-9*24*time.Hour), 6, 6, []string{"m1"}) // before the window

	q := x.query(7)
	q.Project = "alpha"
	m := x.metrics([]string{"injection.contradicted"}, q)["injection.contradicted"]
	if m.Num == nil || *m.Num != 5 || m.Den == nil || *m.Den != 167 {
		t.Fatalf("num/den %v/%v, want 5/167", m.Num, m.Den)
	}
	if m.Value == nil || !close4(*m.Value, 0.0299) {
		t.Errorf("value %v, want 0.0299", m.Value)
	}
	lo, hi := Wilson(5, 167)
	if m.CI == nil || m.CI[0] != lo || m.CI[1] != hi {
		t.Errorf("CI %v, want the Wilson interval %v..%v", m.CI, lo, hi)
	}
	if m.Status != StatusOK || len(m.Tags) != 0 {
		t.Errorf("status %q tags %v, want ok (no rule judges it) and no tag", m.Status, m.Tags)
	}

	all := x.metrics([]string{"injection.contradicted"}, x.query(7))["injection.contradicted"]
	if all.Num == nil || *all.Num != 7 || all.Den == nil || *all.Den != 170 {
		t.Errorf("all projects %v/%v, want 7/170", all.Num, all.Den)
	}
}

func TestContradictedWithoutOutcomes(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "p", fxNow, 3, nil)
	m := x.metrics([]string{"injection.contradicted", "boost.set_changed"}, x.query(7))
	for _, id := range []string{"injection.contradicted", "boost.set_changed"} {
		if m[id].Status != StatusNoData || m[id].Reason != ReasonNoEvents {
			t.Errorf("%s: status %q reason %q, want no_data, no_events", id, m[id].Status, m[id].Reason)
		}
	}
}

// TestBlockYield: the held turns whose agent then credited a memory (used
// or confirmed), out of the held turns of the window and project; by
// outcome, and by path (a block on two paths counts in each, each judged
// on its own blocks). Without a block: no data.
func TestBlockYield(t *testing.T) {
	const id = "stopcheck.block_yield"
	x := newFx(t, fxNow)
	if m := x.metrics([]string{id}, x.query(7))[id]; m.Status != StatusNoData || m.Reason != ReasonNoEvents {
		t.Errorf("no block: status %q reason %q, want no_data, no_events", m.Status, m.Reason)
	}
	block := func(i int, project, outcome string, paths ...string) {
		x.tel(fxNow.Add(-time.Duration(i+1)*time.Minute), telemetry.StopBlock{Outcome: outcome, Paths: paths, Shown: 1}, fmt.Sprintf("s%d", i), project)
	}
	for i := range 20 {
		block(i, "alpha", "used", "search") // 20 search blocks credited
	}
	block(20, "alpha", "confirmed", "prompt", "search")
	block(21, "alpha", "none", "prompt")
	block(22, "alpha", "refused", "prompt")
	block(23, "alpha", "corrected", "briefing")
	block(24, "beta", "used", "prompt")                                                                                // another project
	x.tel(fxNow.Add(-9*24*time.Hour), telemetry.StopBlock{Outcome: "used", Paths: []string{"prompt"}}, "old", "alpha") // before the window
	q := x.query(7)
	q.Project = "alpha"
	m := x.metrics([]string{id}, q)[id]
	if m.Num == nil || *m.Num != 21 || m.Den == nil || *m.Den != 24 {
		t.Fatalf("num/den %v/%v, want 21/24", m.Num, m.Den)
	}
	if got := fmt.Sprint(m.Extra["by_outcome"]); got != "map[confirmed:1 corrected:1 none:1 refused:1 used:20]" {
		t.Errorf("by_outcome %s", got)
	}
	paths, _ := m.Extra["by_path"].(map[string]pathStat)
	if s := paths["search"]; s.Num != 21 || s.Den != 21 || s.Status != StatusOK {
		t.Errorf("search %+v, want 21/21 judged", s)
	}
	if s := paths["prompt"]; s.Num != 1 || s.Den != 3 || s.Status != StatusTooFew {
		t.Errorf("prompt %+v, want 1/3, too few", s)
	}
	if s := paths["briefing"]; s.Num != 0 || s.Den != 1 {
		t.Errorf("briefing %+v, want 0/1", s)
	}
}

// shows journals count prompt-path shows lines of one memory a minute apart
// back from at; flagged gets the flags.
func (x *fx) promptLines(at time.Time, session, path string, count int, flags func(i int) []string) {
	x.t.Helper()
	for i := range count {
		x.journal(feedback.Event{TS: at.Add(-time.Duration(i+1) * time.Minute), Kind: "shows", Session: session, Turn: fxTurn, Project: "alpha", Path: path,
			Items: []feedback.Item{{ID: "m1", Rank: 1}}, Flags: flags(i)})
	}
}

func (x *fx) searchLines(at time.Time, session string, count int, flags func(i int) []string) {
	x.t.Helper()
	for i := range count {
		x.journal(feedback.Event{TS: at.Add(-time.Duration(i+1) * time.Second), Kind: feedback.KindRecall, Session: session, Turn: fxTurn, Path: "search",
			Items: []feedback.Item{{ID: "m1", Rank: 1}}, Flags: flags(i)})
	}
}

func flagged(n int, flags ...string) func(int) []string {
	return func(i int) []string {
		if i < n {
			return flags
		}
		return nil
	}
}

// TestSetChanged: the prompt recalls and the searches whose shown set the
// learned ranking changed. A line that only reordered, a gate show, a
// search's own shows and what is outside the window are not counted.
func TestSetChanged(t *testing.T) {
	x := newFx(t, fxNow)
	set, order := feedback.FlagSetChanged, feedback.FlagOrderChanged
	x.promptLines(fxNow, "s1", "prompt", 140, func(i int) []string {
		switch {
		case i < 3:
			return []string{set, order} // both flags: counts once
		case i < 25:
			return []string{set}
		case i < 30:
			return []string{order} // reordered only
		}
		return nil
	})
	x.searchLines(fxNow.Add(-3*time.Hour), "s1", 27, flagged(5, set))
	x.promptLines(fxNow.Add(-5*time.Hour), "s1", "gate", 4, flagged(4, set))   // not a recall
	x.promptLines(fxNow.Add(-6*time.Hour), "s1", "search", 4, flagged(4, set)) // a search's shows: its recall line counts
	x.promptLines(fxNow.Add(-9*24*time.Hour), "s1", "prompt", 8, flagged(8, set))

	m := x.metrics([]string{"boost.set_changed"}, x.query(7))["boost.set_changed"]
	if m.Num == nil || *m.Num != 30 || m.Den == nil || *m.Den != 167 {
		t.Fatalf("num/den %v/%v, want 30/167", m.Num, m.Den)
	}
	if m.Value == nil || !close4(*m.Value, 30.0/167) {
		t.Errorf("value %v, want %v", m.Value, 30.0/167)
	}
	want := map[string]int64{"prompts": 140, "prompts_set_changed": 25, "searches": 27, "searches_set_changed": 5}
	for k, v := range want {
		if got, ok := m.Extra[k].(int64); !ok || got != v {
			t.Errorf("extra %s = %v, want %d", k, m.Extra[k], v)
		}
	}
	if m.Status != StatusOK || slices.Contains(m.Tags, "partial") {
		t.Errorf("status %q tags %v, want ok and no partial tag", m.Status, m.Tags)
	}
}

// A window that starts before the first shown set the index holds, while
// older shows exist, undercounts: the metric says so. An index that began
// with the window does not.
func TestSetChangedPartial(t *testing.T) {
	x := newFx(t, fxNow)
	base := x.window(7).Since
	set := feedback.FlagSetChanged
	x.promptLines(base.Add(6*time.Hour), "s1", "prompt", 30, flagged(10, set)) // base+5h30 .. base+6h
	x.promptLines(fxNow, "s1", "prompt", 30, flagged(10, set))

	m := x.metrics([]string{"boost.set_changed"}, x.query(7))["boost.set_changed"]
	if slices.Contains(m.Tags, "partial") {
		t.Fatalf("tags %v: every shown set is recorded, want no partial tag", m.Tags)
	}
	if m.Den == nil || *m.Den != 60 {
		t.Fatalf("den %v, want 60", m.Den)
	}

	// The index of an install from before this record: the older shows have
	// no shown-set row.
	db, err := sql.Open("sqlite", "file:"+x.ix.Path()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`DELETE FROM show_sets WHERE ts < ?`, base.Add(7*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	m = x.metrics([]string{"boost.set_changed"}, x.query(7))["boost.set_changed"]
	if !slices.Contains(m.Tags, "partial") {
		t.Errorf("tags %v, want partial: shows exist before the first shown set", m.Tags)
	}
	if m.Den == nil || *m.Den != 30 || m.Num == nil || *m.Num != 10 {
		t.Errorf("num/den %v/%v, want the 10/30 that are recorded", m.Num, m.Den)
	}

	// A window that opens after the first shown set is whole.
	q := x.query(7)
	q.Window = x.window(3)
	m = x.metrics([]string{"boost.set_changed"}, q)["boost.set_changed"]
	if slices.Contains(m.Tags, "partial") {
		t.Errorf("a window opening after the first recorded set: tags %v, want none", m.Tags)
	}
}

// The project filter reads the project of the turn, for lines and searches.
func TestSetChangedProject(t *testing.T) {
	x := newFx(t, fxNow)
	x.turnEvent(fxNow.Add(-time.Hour), "sa", "alpha")
	x.turnEvent(fxNow.Add(-time.Hour), "sb", "beta")
	set := feedback.FlagSetChanged
	x.promptLines(fxNow, "sa", "prompt", 3, flagged(1, set))
	x.searchLines(fxNow, "sa", 2, flagged(2, set))
	x.promptLines(fxNow, "sb", "prompt", 4, flagged(4, set))
	x.searchLines(fxNow, "sb", 5, flagged(5, set))
	q := x.query(7)
	q.Project = "alpha"
	m := x.metrics([]string{"boost.set_changed"}, q)["boost.set_changed"]
	if m.Num == nil || *m.Num != 3 || m.Den == nil || *m.Den != 5 {
		t.Errorf("num/den %v/%v, want 3/5 for alpha", m.Num, m.Den)
	}
}

// byLangFixture writes prompts per language: n recalls, inj of them injecting.
func byLangFixture(t *testing.T, langs ...[3]any) Metric {
	t.Helper()
	x := newFx(t, fxNow)
	for i, l := range langs {
		lang, n, inj := l[0].(string), l[1].(int), l[2].(int)
		x.prompts("alpha", fmt.Sprintf("%s%d", lang, i), fxNow.Add(-time.Duration(i)*time.Minute), n, func(j int, p *telemetry.Prompt) {
			p.Lang = lang
			if j < inj {
				p.Injected = 1
			}
		})
	}
	return x.metrics([]string{"recall.by_lang"}, x.query(30))["recall.by_lang"]
}

func testOf(t *testing.T, m Metric) langTest {
	t.Helper()
	lt, ok := m.Extra["test"].(langTest)
	if !ok {
		t.Fatalf("extra test = %T", m.Extra["test"])
	}
	return lt
}

// frGap is the gap of fr in the test, which every fixture holds.
func frGap(t *testing.T, lt langTest) langGap {
	t.Helper()
	for _, g := range lt.Gaps {
		if g.Lang == "fr" {
			return g
		}
	}
	t.Fatalf("no gap for fr in %+v", lt.Gaps)
	return langGap{}
}

// TestByLanguageTest: the language with the most recalls is the reference;
// each other language with 20 recalls is compared to it. Watch needs a gap
// over 15 points whose interval excludes 0.
func TestByLanguageTest(t *testing.T) {
	m := byLangFixture(t, [3]any{"en", 271, 84}, [3]any{"fr", 84, 11}, [3]any{"de", 9, 3})
	lt := testOf(t, m)
	if lt.Reference != "en" {
		t.Errorf("reference %q, want en", lt.Reference)
	}
	fr := frGap(t, lt)
	if !close4(fr.Gap, 0.17901) || !close4(fr.Interval[0], 0.08825) || !close4(fr.Interval[1], 0.26977) {
		t.Errorf("fr gap %v interval %v, want 0.17901 (0.08825 to 0.26977)", fr.Gap, fr.Interval)
	}
	if fr.Num != 11 || fr.Den != 84 {
		t.Errorf("fr %d/%d, want 11/84", fr.Num, fr.Den)
	}
	if len(lt.Gaps) != 1 {
		t.Errorf("%d languages compared, want fr alone", len(lt.Gaps))
	}
	if len(lt.LeftOut) != 1 || lt.LeftOut[0].Lang != "de" || lt.LeftOut[0].Num != 3 || lt.LeftOut[0].Den != 9 {
		t.Errorf("left out %+v, want de 3/9", lt.LeftOut)
	}
	if m.Status != StatusWatch {
		t.Errorf("status %q, want watch: fr is 17.9 points behind and the interval excludes 0", m.Status)
	}

	// 20 of 84: a gap of 7 points, inside the 15.
	m = byLangFixture(t, [3]any{"en", 271, 84}, [3]any{"fr", 84, 20})
	fr = frGap(t, testOf(t, m))
	if !close4(fr.Gap, 0.07187) || !close4(fr.Interval[0], -0.03457) || !close4(fr.Interval[1], 0.17830) {
		t.Errorf("fr gap %v interval %v, want 0.07187 (-0.03457 to 0.17830)", fr.Gap, fr.Interval)
	}
	if m.Status != StatusOK {
		t.Errorf("status %q, want ok: the gap is under 15 points", m.Status)
	}
}

// A gap over 15 points whose interval holds 0 is not enough, and a language
// ahead of the reference counts by its distance too.
func TestByLanguageTestNeedsBothConditions(t *testing.T) {
	m := byLangFixture(t, [3]any{"en", 100, 50}, [3]any{"fr", 20, 6})
	fr := frGap(t, testOf(t, m))
	if !close4(fr.Gap, 0.2) || !close4(fr.Interval[0], -0.02347) || !close4(fr.Interval[1], 0.42347) {
		t.Errorf("fr gap %v interval %v, want 0.2 (-0.02347 to 0.42347)", fr.Gap, fr.Interval)
	}
	if m.Status != StatusOK {
		t.Errorf("status %q, want ok: the interval holds 0", m.Status)
	}

	m = byLangFixture(t, [3]any{"en", 100, 20}, [3]any{"fr", 25, 15})
	fr = frGap(t, testOf(t, m))
	if !close4(fr.Gap, -0.4) || !close4(fr.Interval[0], -0.60743) || !close4(fr.Interval[1], -0.19257) {
		t.Errorf("fr gap %v interval %v, want -0.4 (-0.60743 to -0.19257)", fr.Gap, fr.Interval)
	}
	if m.Status != StatusWatch {
		t.Errorf("status %q, want watch: fr is 40 points ahead of the reference", m.Status)
	}
}

// Prompts without a language are not a language: they never become the
// reference, and the test does not run on them.
func TestByLanguageTestSkipsUnknown(t *testing.T) {
	m := byLangFixture(t, [3]any{"", 300, 200}, [3]any{"en", 100, 50}, [3]any{"fr", 40, 10})
	lt := testOf(t, m)
	if lt.Reference != "en" || len(lt.Gaps) != 1 || lt.Gaps[0].Lang != "fr" {
		t.Errorf("reference %q gaps %+v, want en and fr", lt.Reference, lt.Gaps)
	}
}

func TestHasContradicted(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "s", fxNow, 5, nil) // s-0 .. s-4
	out := func(session string, ago time.Duration, ids []string, contradicted bool) {
		x.tel(fxNow.Add(-ago), telemetry.Outcome{N: 1, IDs: ids, Contradicted: contradicted}, session, "alpha")
	}
	out("s-0", time.Hour, []string{"m1"}, true)      // kept
	out("s-1", time.Hour, []string{"m1"}, false)     // no correction
	out("s-2", time.Hour, nil, true)                 // nothing was injected
	out("s-4", 9*24*time.Hour, []string{"m1"}, true) // before the window
	out("s-3", 2*time.Hour, []string{"m1"}, true)    // kept

	q := x.query(7)
	q.Has = "contradicted"
	var got []string
	for _, r := range x.run("sessions", q).Tables.Sessions {
		got = append(got, r.ID)
	}
	slices.Sort(got)
	if want := []string{"s-0", "s-3"}; !slices.Equal(got, want) {
		t.Errorf("sessions %v, want %v", got, want)
	}
	q.Has = "bogus"
	if _, err := Run(context.Background(), "sessions", x.source(), q); err == nil {
		t.Error("an unknown --has was accepted")
	}
}
