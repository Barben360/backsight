package metrics

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// sourced journals one shows event of turn on path in project alpha, its
// items as given (rank and source each item's own).
func (x *fx) sourced(at time.Time, session, turn, path string, items ...feedback.Item) {
	x.t.Helper()
	x.journal(feedback.Event{TS: at, Kind: "shows", Session: session, Turn: turn, Project: "alpha", Path: path, Items: items})
}

// recallLine journals a memory_search's recall line of turn, its items as
// given: the MCP server's own record of the hits and their sources.
func (x *fx) recallLine(at time.Time, session, turn string, items ...feedback.Item) {
	x.t.Helper()
	x.journal(feedback.Event{TS: at, Kind: feedback.KindRecall, Session: session, Turn: turn, Project: "alpha", Path: "search", Items: items})
}

// bySource is used.by_source's breakdown, as a view reads it.
func bySource(t *testing.T, m Metric) map[string]map[string]sourcePath {
	t.Helper()
	bs, ok := m.Extra["by_source"].(map[string]map[string]sourcePath)
	if !ok {
		t.Fatalf("by_source = %#v", m.Extra["by_source"])
	}
	return bs
}

func srcCell(s sourceStat) string {
	v := "nil"
	if s.Value != nil {
		v = fmt.Sprintf("%.4f", *s.Value)
	}
	return fmt.Sprintf("%d/%d=%s", s.Num, s.Den, v)
}

// TestBySourceCountsEachShowCreditedInItsSession: a memory's first show of
// the session (prompt or search) counts once, credited when the session
// credits it used or confirmed at or after that show, in any turn. Not a
// credit of another session, one before the show, a wrong or none
// verdict, a later show of the same memory, a briefing or store show, or
// a first show before the window. A search show takes its source from
// the session's recall line when it has none of its own. Ranks past 8
// count only at all ranks; the headline is ranks 1 to 8, both paths.
func TestBySourceCountsEachShowCreditedInItsSession(t *testing.T) {
	x := newFx(t, fxNow)
	at := hoursAgo(1)
	for _, s := range []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"} {
		x.turnEvent(at.Add(-time.Minute), s, "alpha")
	}
	// s1: mA from a conversation, shown on the prompt, credited two turns
	// later; shown again by a search, which does not count twice.
	x.sourced(at, "s1", fxTurn, "prompt", feedback.Item{ID: "mA", Rank: 1, Source: "session"})
	x.sourced(at.Add(time.Minute), "s1", "t2", "search", feedback.Item{ID: "mA", Rank: 1, Source: "session"})
	x.journal(feedback.Event{TS: at.Add(2 * time.Minute), Kind: "credit", Session: "s1", Turn: "t3", Project: "alpha", Verdict: "used", IDs: []string{"mA"}})
	// s2: mB from a note, found by a search, credited in another session
	// only (s3, which never showed it).
	x.sourced(at, "s2", fxTurn, "search", feedback.Item{ID: "mB", Rank: 3, Source: "note"})
	x.credit(at.Add(time.Minute), "s3", "", "alpha", "used", "mB")
	// s4: mC at rank 9, its source on the recall line only, confirmed.
	x.recallLine(at.Add(-time.Second), "s4", fxTurn, feedback.Item{ID: "mC", Rank: 9, Source: "note+session"})
	x.sourced(at, "s4", fxTurn, "search", feedback.Item{ID: "mC", Rank: 9})
	x.credit(at.Add(time.Minute), "s4", "", "alpha", "confirmed", "mC")
	// s5: mD, no source anywhere, reported wrong.
	x.sourced(at, "s5", fxTurn, "search", feedback.Item{ID: "mD", Rank: 2})
	x.credit(at.Add(time.Minute), "s5", "", "alpha", "wrong", "mD")
	// s6: mE from a conversation and a subagent's report, credited before
	// it was shown (a credit of an earlier show it never had).
	x.credit(at.Add(-30*time.Second), "s6", "", "alpha", "used", "mE")
	x.sourced(at, "s6", fxTurn, "prompt", feedback.Item{ID: "mE", Rank: 2, Source: "session+subagent"})
	// s7: a briefing and a store answer's similar memory, both credited:
	// not a recall's show.
	x.sourced(at, "s7", fxTurn, "briefing", feedback.Item{ID: "mF", Rank: 1, Source: "session"})
	x.sourced(at, "s7", fxTurn, feedback.PathStore, feedback.Item{ID: "mG", Rank: 1, Source: "session"})
	x.credit(at.Add(time.Minute), "s7", "", "alpha", "used", "mF", "mG")
	// s8: mH first shown 8 days ago, shown again and credited in the
	// window: its first show is not the window's.
	old := fxNow.Add(-8 * 24 * time.Hour)
	x.turnEvent(old.Add(-time.Minute), "s8", "alpha")
	x.sourced(old, "s8", fxTurn, "prompt", feedback.Item{ID: "mH", Rank: 1, Source: "note"})
	x.sourced(at, "s8", fxTurn, "prompt", feedback.Item{ID: "mH", Rank: 1, Source: "note"})
	x.credit(at.Add(time.Minute), "s8", "", "alpha", "used", "mH")

	m := qualityOf(x, x.query(7))["used.by_source"]
	// ranks 1 to 8: mA credited, mB, mD, mE not; mC is rank 9
	if num(m) != 1 || den(m) != 4 || !close4(val(m), 0.25) || m.Status != StatusTooFew {
		t.Errorf("headline %d/%d = %v %s, want 1/4 = 0.25 too_few", num(m), den(m), val(m), m.Status)
	}
	bs := bySource(t, m)
	want := map[string]map[string][2]string{
		"conversation":      {"prompt": {"1/1=1.0000", "1/1=1.0000"}},
		"note":              {"search": {"0/1=0.0000", "0/1=0.0000"}},
		"conversation+note": {"search": {"1/1=1.0000", "0/0=nil"}},
		"subagent":          {"prompt": {"0/1=0.0000", "0/1=0.0000"}},
		"unknown":           {"search": {"0/1=0.0000", "0/1=0.0000"}},
	}
	if len(bs) != len(want) {
		t.Errorf("sources %v, want %v", bs, want)
	}
	for src, paths := range want {
		if len(bs[src]) != len(paths) {
			t.Errorf("%s: paths %v, want %v", src, bs[src], paths)
		}
		for p, w := range paths {
			got := bs[src][p]
			if srcCell(got.All) != w[0] || srcCell(got.Top8) != w[1] {
				t.Errorf("%s/%s: all %s, ranks 1-8 %s, want %s and %s", src, p, srcCell(got.All), srcCell(got.Top8), w[0], w[1])
			}
		}
	}
	if a, ok := m.Extra["all_ranks"].(sourceStat); !ok || srcCell(a) != "2/5=0.4000" {
		t.Errorf("all ranks %#v, want 2/5", m.Extra["all_ranks"])
	}
	if bp, ok := m.Extra["by_project"].(map[string]sourceStat); !ok || len(bp) != 1 || srcCell(bp["alpha"]) != "1/4=0.2500" {
		t.Errorf("by project %#v, want alpha 1/4", m.Extra["by_project"])
	}
}

// Each cell is judged on its own shows: 20 note shows on the prompt path
// (5 credited) have the 20 they need and their smallest detectable change,
// 1.96·√(0.5/20); a cell of 3 has neither. by_project splits by the
// session's project, and a project filter keeps that project's sessions.
func TestBySourceJudgesEachCellAndSplitsByProject(t *testing.T) {
	x := newFx(t, fxNow)
	for i := range 20 {
		s := fmt.Sprintf("n-%d", i)
		at := hoursAgo(i)
		x.turnEvent(at.Add(-time.Second), s, "alpha")
		x.sourced(at, s, fxTurn, "prompt", feedback.Item{ID: "m" + s, Rank: 1, Source: "note"})
		if i < 5 {
			x.credit(at.Add(time.Minute), s, "", "alpha", "used", "m"+s)
		}
	}
	for i := range 3 {
		s := fmt.Sprintf("b-%d", i)
		at := hoursAgo(i)
		x.turnEvent(at.Add(-time.Second), s, "beta")
		x.journal(feedback.Event{TS: at, Kind: "shows", Session: s, Turn: fxTurn, Project: "beta", Path: "search",
			Items: []feedback.Item{{ID: "m" + s, Rank: 1, Source: "session"}}})
		x.credit(at.Add(time.Minute), s, "", "beta", "used", "m"+s)
	}
	m := qualityOf(x, x.query(7))["used.by_source"]
	note := bySource(t, m)["note"]["prompt"].Top8
	if srcCell(note) != "5/20=0.2500" || note.MDC == nil || math.Abs(*note.MDC-1.96*math.Sqrt(0.5/20)) > 1e-9 || note.Status != StatusOK {
		t.Errorf("note/prompt %s mdc %v %s, want 5/20, mdc 0.3099, ok", srcCell(note), note.MDC, note.Status)
	}
	conv := bySource(t, m)["conversation"]["search"].Top8
	if srcCell(conv) != "3/3=1.0000" || conv.MDC != nil || conv.Status != StatusTooFew {
		t.Errorf("conversation/search %s mdc %v %s, want 3/3, no mdc, too_few", srcCell(conv), conv.MDC, conv.Status)
	}
	bp, _ := m.Extra["by_project"].(map[string]sourceStat)
	if srcCell(bp["alpha"]) != "5/20=0.2500" || srcCell(bp["beta"]) != "3/3=1.0000" || bp["alpha"].MDC == nil || bp["beta"].MDC != nil {
		t.Errorf("by project %v", bp)
	}
	if num(m) != 8 || den(m) != 23 || m.MDC == nil {
		t.Errorf("headline %d/%d mdc %v, want 8/23 with its mdc", num(m), den(m), m.MDC)
	}
	q := x.query(7)
	q.Project = "beta"
	mb := qualityOf(x, q)["used.by_source"]
	if num(mb) != 3 || den(mb) != 3 || len(bySource(t, mb)) != 1 {
		t.Errorf("beta: %d/%d %v, want 3/3 from conversations only", num(mb), den(mb), bySource(t, mb))
	}
}

// The edges of a show's count: a none or outdated credit is no credit; a
// credit at the very time of the show is one; an unranked show counts at
// all ranks only; a session whose turn names no project is the unknown
// project; of two recall lines of the memory, the nearest in time gives
// the source, whichever came last; an import reads other and a note with
// a subagent's report subagent.
func TestBySourceEdges(t *testing.T) {
	x := newFx(t, fxNow)
	at := hoursAgo(1)
	for _, s := range []string{"e1", "e2", "e3", "e5", "e6", "e7"} {
		x.turnEvent(at.Add(-time.Minute), s, "alpha")
	}
	x.turnEvent(at.Add(-time.Minute), "e4", "")
	x.sourced(at, "e1", fxTurn, "prompt", feedback.Item{ID: "mN", Rank: 1, Source: "session"})
	x.credit(at.Add(time.Minute), "e1", "", "alpha", "none", "mN")
	x.sourced(at, "e2", fxTurn, "prompt", feedback.Item{ID: "mO", Rank: 1, Source: "session"})
	x.credit(at.Add(time.Minute), "e2", "", "alpha", "outdated", "mO")
	x.sourced(at, "e3", fxTurn, "prompt", feedback.Item{ID: "mZ", Source: "note"}) // unranked
	x.credit(at.Add(time.Minute), "e3", "", "alpha", "used", "mZ")
	x.journal(feedback.Event{TS: at, Kind: "shows", Session: "e4", Turn: fxTurn, Path: "prompt", Items: []feedback.Item{{ID: "mP", Rank: 1, Source: "note"}}})
	x.credit(at.Add(time.Minute), "e4", "", "", "used", "mP")
	x.recallLine(at.Add(-time.Second), "e5", fxTurn, feedback.Item{ID: "mR", Rank: 2, Source: "session"})
	x.recallLine(at.Add(50*time.Minute), "e5", "t2", feedback.Item{ID: "mR", Rank: 1, Source: "note"})
	x.sourced(at, "e5", fxTurn, "search", feedback.Item{ID: "mR", Rank: 2})
	x.sourced(at, "e6", fxTurn, "search", feedback.Item{ID: "mI", Rank: 1, Source: "import"}, feedback.Item{ID: "mJ", Rank: 2, Source: "note+subagent"})
	x.sourced(at, "e7", fxTurn, "prompt", feedback.Item{ID: "mT", Rank: 1, Source: "session"})
	x.credit(at, "e7", "", "alpha", "used", "mT")

	m := qualityOf(x, x.query(7))["used.by_source"]
	want := map[string]map[string][2]string{
		"conversation": {"prompt": {"1/3=0.3333", "1/3=0.3333"}, "search": {"0/1=0.0000", "0/1=0.0000"}},
		"note":         {"prompt": {"2/2=1.0000", "1/1=1.0000"}},
		"other":        {"search": {"0/1=0.0000", "0/1=0.0000"}},
		"subagent":     {"search": {"0/1=0.0000", "0/1=0.0000"}},
	}
	bs := bySource(t, m)
	if len(bs) != len(want) {
		t.Errorf("sources %v, want %v", bs, want)
	}
	for src, paths := range want {
		if len(bs[src]) != len(paths) {
			t.Errorf("%s: paths %v, want %v", src, bs[src], paths)
		}
		for p, w := range paths {
			got := bs[src][p]
			if srcCell(got.All) != w[0] || srcCell(got.Top8) != w[1] {
				t.Errorf("%s/%s: all %s, ranks 1-8 %s, want %s and %s", src, p, srcCell(got.All), srcCell(got.Top8), w[0], w[1])
			}
		}
	}
	if num(m) != 2 || den(m) != 7 {
		t.Errorf("headline %d/%d, want 2/7", num(m), den(m))
	}
	if a, _ := m.Extra["all_ranks"].(sourceStat); srcCell(a) != "3/8=0.3750" {
		t.Errorf("all ranks %s, want 3/8", srcCell(a))
	}
	bp, _ := m.Extra["by_project"].(map[string]sourceStat)
	if len(bp) != 2 || srcCell(bp["alpha"]) != "1/6=0.1667" || srcCell(bp[unknownKey]) != "1/1=1.0000" {
		t.Errorf("by project %v, want alpha 1/6 and unknown 1/1", bp)
	}
}

// Without a show of the window, the breakdown is empty and the headline
// has no value; a briefing page is never a memory shown.
func TestBySourceEmptyAndPagesLeftOut(t *testing.T) {
	x := newFx(t, fxNow)
	at := hoursAgo(1)
	x.turnEvent(at.Add(-time.Second), "s1", "alpha")
	x.sourced(at, "s1", fxTurn, "prompt", feedback.Item{ID: feedback.BriefingPagePrefix + "alpha", Rank: 1, Source: "session"})
	x.credit(at.Add(time.Minute), "s1", "", "alpha", "used", feedback.BriefingPagePrefix+"alpha")
	m := qualityOf(x, x.query(7))["used.by_source"]
	if den(m) != 0 || m.Value != nil || len(bySource(t, m)) != 0 {
		t.Errorf("%d/%d %v %v, want nothing counted", num(m), den(m), m.Value, bySource(t, m))
	}
}
