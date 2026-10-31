package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
)

// The quality fixtures write the journal as the hooks do: a turn event,
// shows events (each item a show whose key is feedback.ShowsOf's), credit
// and stop events, and settle events naming the shows' keys. Times are
// Europe/Paris; the default window is the 7 days before fxNow, from
// since7 (2026-10-23T13:00+02:00).

func qualityOf(x *fx, q Query) map[string]Metric { return x.metrics(qualityIDs, q) }

// fxTurn is the turn id of the quality fixtures' turns, one per session.
const fxTurn = "t1"

// turnEvent journals session's turn in project at at.
func (x *fx) turnEvent(at time.Time, session, project string) {
	x.t.Helper()
	x.journal(feedback.Event{TS: at, Kind: "turn", Session: session, Turn: fxTurn, Project: project, N: 1})
}

// shows journals one shows event of turn by agent on path, each id an item
// ranked from 1 whose memory belongs to memProject (the session's project
// when empty), and returns the shows' keys.
func (x *fx) shows(at time.Time, session, turn, agent, project, path, memProject string, ids ...string) []string {
	x.t.Helper()
	e := feedback.Event{TS: at, Kind: "shows", Session: session, Turn: turn, Agent: agent, Project: project, Path: path}
	for i, id := range ids {
		e.Items = append(e.Items, feedback.Item{ID: id, Rank: i + 1, Project: memProject})
	}
	x.journal(e)
	var keys []string
	for _, s := range feedback.ShowsOf(e) {
		keys = append(keys, s.Key)
	}
	return keys
}

// rankedShow journals one show of id at rank (0: unranked) and returns its
// key.
func (x *fx) rankedShow(at time.Time, session, turn, path, id string, rank int) string {
	x.t.Helper()
	e := feedback.Event{TS: at, Kind: "shows", Session: session, Turn: turn, Project: "alpha", Path: path,
		Items: []feedback.Item{{ID: id, Rank: rank}}}
	x.journal(e)
	return feedback.ShowsOf(e)[0].Key
}

func (x *fx) stop(at time.Time, session, agent, outcome string) {
	x.t.Helper()
	x.journal(feedback.Event{TS: at, Kind: "stop", Session: session, Turn: fxTurn, Agent: agent, Outcome: outcome})
}

func (x *fx) credit(at time.Time, session, agent, project, verdict string, ids ...string) {
	x.t.Helper()
	x.journal(feedback.Event{TS: at, Kind: "credit", Session: session, Turn: fxTurn, Agent: agent, Project: project, Verdict: verdict, IDs: ids})
}

// settle journals a settle event giving each key its outcome; the key's
// show time is read back from the key.
func (x *fx) settle(at time.Time, outcomes map[string]string) {
	x.t.Helper()
	e := feedback.Event{TS: at, Kind: "settle"}
	keys := make([]string, 0, len(outcomes))
	for k := range outcomes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		parts := strings.Split(k, "|")
		var ns int64
		if _, err := fmt.Sscan(parts[2], &ns); err != nil {
			x.t.Fatal(err)
		}
		e.Settled = append(e.Settled, feedback.Settled{Key: k, ID: parts[3], At: time.Unix(0, ns).UTC(), Outcome: outcomes[k]})
	}
	x.journal(e)
}

// declaredTurn writes a main-agent turn of session (turn t1, project
// alpha) showing ids on the prompt path at at, ended by outcome a minute
// later ("" for no stop), with a credit of verdict naming ids 30 s after
// the show ("" for none).
func (x *fx) declaredTurn(at time.Time, session, outcome, verdict string, ids ...string) {
	x.t.Helper()
	x.turnEvent(at.Add(-time.Second), session, "alpha")
	x.shows(at, session, "t1", "", "alpha", "prompt", "", ids...)
	if verdict != "" {
		x.credit(at.Add(30*time.Second), session, "", "alpha", verdict, ids...)
	}
	if outcome != "" {
		x.stop(at.Add(time.Minute), session, "", outcome)
	}
}

// hoursAgo is the i-th hour before fxNow, from 3 h back (past the open
// turn limit).
func hoursAgo(i int) time.Time { return fxNow.Add(-time.Duration(i+3) * time.Hour) }

func num(m Metric) int64 {
	if m.Num == nil {
		return -1
	}
	return *m.Num
}

func den(m Metric) int64 {
	if m.Den == nil {
		return -1
	}
	return *m.Den
}

func val(m Metric) float64 {
	if m.Value == nil {
		return -1
	}
	return *m.Value
}

// extraRatio is a *float64 extra as a number, -1 when null or absent.
func extraRatio(m Metric, key string) float64 {
	p, ok := m.Extra[key].(*float64)
	if !ok || p == nil {
		return -1
	}
	return *p
}

// extraText prints m's extra with its ratios, not their addresses.
func extraText(m Metric) string {
	keys := make([]string, 0, len(m.Extra))
	for k := range m.Extra {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		v := m.Extra[k]
		if p, ok := v.(*float64); ok && p != nil {
			v = *p
		}
		fmt.Fprintf(&b, "%s=%v ", k, v)
	}
	return b.String()
}

// TestUsedTurnRate: 33 declared turns, 12 with a used credit (one of them
// credited by a subagent: any agent counts), 4 declaring only wrong,
// outdated or none; 3 undeclared; 2 pending (no stop, shown 1 h ago); 1
// check_skipped; 1 subagent turn. used.turn_rate = 12/33 = 0.3636, worst
// case 12/(33+3) = 0.3333; declare.coverage = 33/36 = 0.9167, interval
// [0.7817, 0.9713]: its upper bound is above 0.80, so no unreliable tag.
func TestUsedTurnRate(t *testing.T) {
	x := newFx(t, fxNow)
	h := 0
	next := func() time.Time { h++; return hoursAgo(h) }
	for i := range 33 {
		verdict := ""
		switch {
		case i < 11:
			verdict = "used"
		case i == 12:
			verdict = "wrong"
		case i == 13:
			verdict = "outdated"
		case i == 14 || i == 15:
			verdict = "none"
		}
		at := next()
		s := fmt.Sprintf("d-%d", i)
		x.declaredTurn(at, s, "declared", verdict, fmt.Sprintf("m%d", i))
		if i == 11 { // the 12th used credit, by a subagent of the turn
			x.credit(at.Add(40*time.Second), s, "agent-1", "alpha", "used", "m11")
		}
	}
	for i := range 3 {
		x.declaredTurn(next(), fmt.Sprintf("u-%d", i), "undeclared", "", "mu")
	}
	x.declaredTurn(next(), "skip", "check_skipped", "", "ms")
	for i := range 2 {
		x.declaredTurn(fxNow.Add(-time.Hour), fmt.Sprintf("p-%d", i), "", "", "mp")
	}
	at := next()
	x.turnEvent(at, "sub", "alpha")
	x.shows(at, "sub", "t1", "agent-2", "alpha", "search", "", "mx")

	m := qualityOf(x, x.query(7))
	r := m["used.turn_rate"]
	if num(r) != 12 || den(r) != 33 || !close4(val(r), 0.3636) {
		t.Fatalf("used.turn_rate = %d/%d (%v), want 12/33 = 0.3636", num(r), den(r), val(r))
	}
	if w := extraRatio(r, "worst_case"); !close4(w, 0.3333) {
		t.Errorf("worst_case = %v, want 12/36 = 0.3333", w)
	}
	for k, want := range map[string]int64{"pending": 2, "check_skipped": 1, "subagent_turns": 1} {
		if r.Extra[k] != want {
			t.Errorf("extra.%s = %v, want %d", k, r.Extra[k], want)
		}
	}
	if slices.Contains(r.Tags, tagUnreliable) {
		t.Errorf("tags = %v: coverage 33/36 has an upper bound above 0.80", r.Tags)
	}
	if r.CI == nil || !close4(r.CI[0], 0.2219) || !close4(r.CI[1], 0.5338) {
		t.Errorf("ci = %v, want [0.2219 0.5338]", r.CI)
	}
	var sn, sd int64
	for _, p := range r.Series {
		sn, sd = sn+*p.Num, sd+*p.Den
	}
	if len(r.Series) != 8 || sn != 12 || sd != 33 {
		t.Errorf("series: %d points summing to %d/%d, want 8 summing to 12/33", len(r.Series), sn, sd)
	}

	c := m["declare.coverage"]
	if num(c) != 33 || den(c) != 36 || c.Status != StatusOK {
		t.Errorf("declare.coverage = %d/%d %s, want 33/36 ok", num(c), den(c), c.Status)
	}
	if c.CI == nil || !close4(c.CI[0], 0.7817) || !close4(c.CI[1], 0.9713) {
		t.Errorf("coverage ci = %v, want [0.7817 0.9713]", c.CI)
	}
	if c.Extra["pending"] != int64(2) || c.Extra["check_skipped"] != int64(1) {
		t.Errorf("coverage extra = %v, want pending 2, check_skipped 1", c.Extra)
	}
}

// TestTurnWithoutStopAfterTwoHours: 20 declared turns, 10 used. A turn
// without a stop whose latest show is 1h59m old (its first 3 h old) is
// pending, in no denominator; one whose only show is 2h01m old counts as
// undeclared: coverage 20/21, turn rate still 10/20, worst case 10/21.
func TestTurnWithoutStopAfterTwoHours(t *testing.T) {
	base := func(t *testing.T) *fx {
		x := newFx(t, fxNow)
		for i := range 20 {
			v := ""
			if i < 10 {
				v = "used"
			}
			x.declaredTurn(hoursAgo(i+1), fmt.Sprintf("d-%d", i), "declared", v, "m")
		}
		return x
	}
	t.Run("pending", func(t *testing.T) {
		x := base(t)
		x.turnEvent(fxNow.Add(-3*time.Hour-time.Second), "open", "alpha")
		x.shows(fxNow.Add(-3*time.Hour), "open", "t1", "", "alpha", "prompt", "", "m1")
		x.shows(fxNow.Add(-time.Hour-59*time.Minute), "open", "t1", "", "alpha", "search", "", "m2")
		m := qualityOf(x, x.query(7))
		r, c := m["used.turn_rate"], m["declare.coverage"]
		if den(c) != 20 || den(r) != 20 || r.Extra["pending"] != int64(1) || !close4(extraRatio(r, "worst_case"), 0.5) {
			t.Errorf("coverage den %d, rate den %d, pending %v, worst case %v; want 20, 20, 1, 10/20",
				den(c), den(r), r.Extra["pending"], extraRatio(r, "worst_case"))
		}
	})
	t.Run("undeclared", func(t *testing.T) {
		x := base(t)
		x.declaredTurn(fxNow.Add(-2*time.Hour-time.Minute), "open", "", "", "m1")
		m := qualityOf(x, x.query(7))
		r, c := m["used.turn_rate"], m["declare.coverage"]
		if num(c) != 20 || den(c) != 21 || den(r) != 20 || r.Extra["pending"] != int64(0) || !close4(extraRatio(r, "worst_case"), 10.0/21) {
			t.Errorf("coverage %d/%d, rate den %d, pending %v, worst case %v; want 20/21, 20, 0, 10/21",
				num(c), den(c), den(r), r.Extra["pending"], extraRatio(r, "worst_case"))
		}
	})
}

// fxNov is a clock after the month change: 2026-11-02T12:00+01:00.
var fxNov = time.Date(2026, 11, 2, 12, 0, 0, 0, fxParis)

// journalFile fails unless the journal month file holds a line of kind.
func journalFile(x *fx, month, kind string) {
	x.t.Helper()
	raw, err := os.ReadFile(filepath.Join(x.dirs.FeedbackDir(), "journal-"+month+".jsonl"))
	if err != nil || !strings.Contains(string(raw), `"kind":"`+kind+`"`) {
		x.t.Fatalf("journal-%s.jsonl lacks a %s line (%v)", month, kind, err)
	}
}

// TestBlockedThenDeclaredCountsOnce: blocked then declared is one
// declared turn; a lone blocked is one undeclared, whatever a subagent
// of the turn declared; blocked in October's
// file and declared in November's is one declared turn.
func TestBlockedThenDeclaredCountsOnce(t *testing.T) {
	x := newFx(t, fxNow)
	x.declaredTurn(hoursAgo(1), "twice", "blocked", "", "m1")
	x.stop(hoursAgo(1).Add(2*time.Minute), "twice", "", "declared")
	x.declaredTurn(hoursAgo(2), "lone", "blocked", "", "m1")
	x.stop(hoursAgo(2).Add(2*time.Minute), "lone", "agent-1", "declared") // a subagent's: not the turn's
	c := qualityOf(x, x.query(7))["declare.coverage"]
	if num(c) != 1 || den(c) != 2 {
		t.Errorf("coverage = %d/%d, want 1 declared of 2", num(c), den(c))
	}

	y := newFx(t, fxNov)
	shown := time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC)
	y.declaredTurn(shown, "across", "", "", "m1")
	y.stop(time.Date(2026, 10, 31, 23, 50, 0, 0, time.UTC), "across", "", "blocked")
	y.stop(time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC), "across", "", "declared")
	journalFile(y, "2026-10", "stop")
	journalFile(y, "2026-11", "stop")
	m := qualityOf(y, y.query(7))
	if c, r := m["declare.coverage"], m["used.turn_rate"]; num(c) != 1 || den(c) != 1 || den(r) != 1 {
		t.Errorf("across the month: coverage %d/%d, rate den %d; want 1/1 and 1", num(c), den(c), den(r))
	}
}

// TestFinalStopFollowsJournalOrder pins the journal-order definition of a
// turn's final stop: a blocked in November's file, then a declared written
// later with an October time (so in October's file), reads undeclared:
// November's file comes later in the journal. No writer produces such a
// line: the hook appends a stop at its own time.
func TestFinalStopFollowsJournalOrder(t *testing.T) {
	x := newFx(t, fxNov)
	x.declaredTurn(time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC), "back", "", "", "m1")
	x.stop(time.Date(2026, 11, 1, 0, 10, 0, 0, time.UTC), "back", "", "blocked")
	x.stop(time.Date(2026, 10, 31, 23, 55, 0, 0, time.UTC), "back", "", "declared")
	journalFile(x, "2026-10", "stop")
	journalFile(x, "2026-11", "stop")
	if c := qualityOf(x, x.query(7))["declare.coverage"]; num(c) != 0 || den(c) != 1 {
		t.Errorf("coverage = %d/%d, want 0 declared of 1 (the November blocked is last)", num(c), den(c))
	}
}

// TestQualityFilterUsesTheSessionsProject: sessions of alpha and beta
// both show m1, a memory of gamma (shows.project is gamma). The filter
// reads the session's project: alpha counts its own turn, gamma none.
func TestQualityFilterUsesTheSessionsProject(t *testing.T) {
	x := newFx(t, fxNow)
	for i, p := range []string{"alpha", "beta"} {
		s := "s-" + p
		at := hoursAgo(i)
		x.turnEvent(at.Add(-time.Second), s, p)
		keys := x.shows(at, s, "t1", "", p, "prompt", "gamma", "m1")
		verdict, outcome := "used", feedback.OutcomeCredit
		if p == "beta" {
			verdict, outcome = "none", feedback.OutcomeMiss
		}
		x.credit(at.Add(30*time.Second), s, "", p, verdict, "m1")
		x.stop(at.Add(time.Minute), s, "", "declared")
		x.settle(at.Add(time.Hour), map[string]string{keys[0]: outcome})
	}
	q := x.query(7)
	q.Project = "alpha"
	m := qualityOf(x, q)
	if r := m["used.turn_rate"]; num(r) != 1 || den(r) != 1 {
		t.Errorf("alpha turn rate = %d/%d, want 1/1", num(r), den(r))
	}
	if s := m["used.share_confident"]; num(s) != 1 || den(s) != 1 {
		t.Errorf("alpha share confident = %d/%d, want 1/1", num(s), den(s))
	}
	if w := m["verdict.wrong_rate"]; den(w) != 1 {
		t.Errorf("alpha wrong rate den = %d, want 1 (alpha's used credit)", den(w))
	}
	q.Project = "beta"
	if r := qualityOf(x, q)["used.turn_rate"]; num(r) != 0 || den(r) != 1 {
		t.Errorf("beta turn rate = %d/%d, want 0/1", num(r), den(r))
	}
	q.Project = "gamma"
	for id, mm := range qualityOf(x, q) {
		if strings.HasPrefix(id, "stale.") {
			continue
		}
		if mm.Status != StatusNoData || mm.Reason != ReasonNoEvents {
			t.Errorf("gamma (the memory's project, no session's): %s = %s %q, want no_data no_events", id, mm.Status, mm.Reason)
		}
	}
	if r := qualityOf(x, x.query(7))["used.turn_rate"]; den(r) != 2 {
		t.Errorf("no filter: den = %d, want 2", den(r))
	}
}

// TestCoverageGatesReliability: used.turn_rate is unreliable while the
// coverage has its 20 turns and an upper bound under 0.80. 16 declared of
// 25: 0.64, [0.4452, 0.7975] → coverage watch, tagged. 17 of 25: [0.4841,
// 0.8280] → ok, not tagged. 1 of 5: [0.0362, 0.6245] but 5 < 20 → too_few,
// not tagged.
func TestCoverageGatesReliability(t *testing.T) {
	for _, c := range []struct {
		declared, of int
		status       Status
		hi           float64
		tagged       bool
	}{
		{16, 25, StatusWatch, 0.7975, true},
		{17, 25, StatusOK, 0.8280, false},
		{1, 5, StatusTooFew, 0.6245, false},
	} {
		t.Run(fmt.Sprintf("%d of %d", c.declared, c.of), func(t *testing.T) {
			x := newFx(t, fxNow)
			for i := range c.of {
				outcome := "undeclared"
				if i < c.declared {
					outcome = "declared"
				}
				x.declaredTurn(hoursAgo(i), fmt.Sprintf("s-%d", i), outcome, "used", "m1")
			}
			m := qualityOf(x, x.query(7))
			cov := m["declare.coverage"]
			if num(cov) != int64(c.declared) || den(cov) != int64(c.of) || cov.Status != c.status || cov.CI == nil || !close4(cov.CI[1], c.hi) {
				t.Errorf("coverage = %d/%d %s ci %v, want %d/%d %s hi %.4f", num(cov), den(cov), cov.Status, cov.CI, c.declared, c.of, c.status, c.hi)
			}
			if got := slices.Contains(m["used.turn_rate"].Tags, tagUnreliable); got != c.tagged {
				t.Errorf("used.turn_rate tags = %v, want unreliable: %v", m["used.turn_rate"].Tags, c.tagged)
			}
		})
	}
}

// TestBriefingOnlyTurnsDoNotCount: a turn that showed only the briefing
// is in no denominator; a turn's time is its earliest non-briefing show,
// so a briefing before the window does not move a turn out of it.
func TestBriefingOnlyTurnsDoNotCount(t *testing.T) {
	x := newFx(t, fxNow)
	x.turnEvent(hoursAgo(1), "brief", "alpha")
	x.shows(hoursAgo(1), "brief", "t1", "", "alpha", "briefing", "", "b1", "b2")
	x.credit(hoursAgo(1).Add(time.Minute), "brief", "", "alpha", "used", "b1", "b2")
	x.stop(hoursAgo(1).Add(2*time.Minute), "brief", "", "declared")

	x.turnEvent(since7.Add(-time.Hour), "early", "alpha")
	x.shows(since7.Add(-time.Hour), "early", "t1", "", "alpha", "briefing", "", "b1")
	x.shows(since7.Add(30*time.Minute), "early", "t1", "", "alpha", "prompt", "", "m1")
	x.stop(since7.Add(31*time.Minute), "early", "", "declared")

	m := qualityOf(x, x.query(7))
	for _, id := range []string{"used.turn_rate", "declare.coverage"} {
		if den(m[id]) != 1 {
			t.Errorf("%s den = %d, want 1 (the early turn only)", id, den(m[id]))
		}
	}
	if s := m["used.rubber_stamp"]; den(s) != 0 {
		t.Errorf("rubber stamp den = %d, want 0 (the briefing's two memories do not count)", den(s))
	}
}

// TestShowTimeAttribution: a turn shown 2026-10-22T23:50, before the
// window, whose stop and used credit land an hour into it, counts in the
// previous window, not this one: in used.turn_rate, declare.coverage and
// verdict.wrong_rate.
func TestShowTimeAttribution(t *testing.T) {
	x := newFx(t, fxNow)
	shown := time.Date(2026, 10, 22, 23, 50, 0, 0, fxParis)
	x.turnEvent(shown.Add(-time.Second), "s", "alpha")
	x.shows(shown, "s", "t1", "", "alpha", "prompt", "", "m1")
	x.credit(since7.Add(time.Hour), "s", "", "alpha", "used", "m1")
	x.stop(since7.Add(time.Hour), "s", "", "declared")

	q := x.query(7)
	cur := qualityOf(x, q)
	prev := qualityOf(x, Query{Window: q.Window.Previous(), Machines: "this"})
	for _, id := range []string{"used.turn_rate", "declare.coverage", "verdict.wrong_rate"} {
		if den(cur[id]) != 0 {
			t.Errorf("%s in the window: den %d, want 0", id, den(cur[id]))
		}
		if den(prev[id]) != 1 {
			t.Errorf("%s in the previous window: den %d, want 1", id, den(prev[id]))
		}
	}
	if num(prev["used.turn_rate"]) != 1 {
		t.Errorf("previous turn rate num = %d, want 1", num(prev["used.turn_rate"]))
	}
}

// TestBackDatedJournalLine: after October's and November's journals are
// ingested, a used credit appended with an October time lands in
// October's file, after its cursor; the next catch-up reads it, and the
// window [10-24, 10-31) counts it: used.turn_rate 0/1 → 1/1,
// verdict.wrong_rate no rows → 0/1.
func TestBackDatedJournalLine(t *testing.T) {
	x := newFx(t, fxNov)
	shown := time.Date(2026, 10, 29, 10, 0, 0, 0, fxParis)
	x.declaredTurn(shown, "oct", "declared", "", "m1")
	x.declaredTurn(time.Date(2026, 11, 1, 10, 0, 0, 0, fxParis), "nov", "declared", "", "m2")
	journalFile(x, "2026-11", "shows")
	w, err := NewWindow(x.now, 0, time.Date(2026, 10, 24, 0, 0, 0, 0, fxParis), time.Date(2026, 10, 31, 0, 0, 0, 0, fxParis), fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	q := Query{Window: w, Machines: "this"}
	before := qualityOf(x, q)
	if r := before["used.turn_rate"]; num(r) != 0 || den(r) != 1 {
		t.Fatalf("before: turn rate %d/%d, want 0/1", num(r), den(r))
	}
	if wr := before["verdict.wrong_rate"]; wr.Status != StatusNoData {
		t.Fatalf("before: wrong rate %s, want no_data (no credit yet)", wr.Status)
	}

	x.credit(shown.Add(3*time.Minute), "oct", "", "alpha", "used", "m1")
	journalFile(x, "2026-10", "credit")
	after := qualityOf(x, q)
	if r := after["used.turn_rate"]; num(r) != 1 || den(r) != 1 {
		t.Errorf("after: turn rate %d/%d, want 1/1", num(r), den(r))
	}
	if wr := after["verdict.wrong_rate"]; num(wr) != 0 || den(wr) != 1 || wr.Extra["used"] != int64(1) {
		t.Errorf("after: wrong rate %d/%d %v, want 0/1 with one used", num(wr), den(wr), wr.Extra)
	}
}

// TestSubagentShowInMainTurn: a subagent's show in a main turn's (session,
// turn) is a turn of its own and moves nothing of the main turn.
// Session "open": main show of m1 4 h ago, no stop; a subagent shows m2
// 1 h ago. The main turn's latest show stays 4 h ago, so it is undeclared
// (a leak would make it pending: latest show under 2 h old). Session
// "stamp": declared, shows m1 only, credited; a subagent later shows m2.
// The main turn has 1 memory, so it is no rubber-stamp candidate (a leak
// would make 2 memories, 1 credited: den 1).
func TestSubagentShowInMainTurn(t *testing.T) {
	x := newFx(t, fxNow)
	x.declaredTurn(hoursAgo(1), "open", "", "", "m1")
	x.shows(fxNow.Add(-time.Hour), "open", "t1", "agent-1", "alpha", "search", "", "m2")
	x.declaredTurn(hoursAgo(2), "stamp", "declared", "used", "m1")
	x.shows(hoursAgo(2).Add(2*time.Minute), "stamp", "t1", "agent-1", "alpha", "search", "", "m2")

	m := qualityOf(x, x.query(7))
	r := m["used.turn_rate"]
	if r.Extra["pending"] != int64(0) {
		t.Errorf("pending = %v, want 0: the subagent show must not extend the main turn", r.Extra["pending"])
	}
	if c := m["declare.coverage"]; num(c) != 1 || den(c) != 2 {
		t.Errorf("coverage = %d/%d, want 1/2 (stamp declared, open undeclared)", num(c), den(c))
	}
	if r.Extra["subagent_turns"] != int64(2) {
		t.Errorf("subagent_turns = %v, want 2", r.Extra["subagent_turns"])
	}
	if rs := m["used.rubber_stamp"]; den(rs) != 0 {
		t.Errorf("rubber_stamp den = %d, want 0: the main turn shows one memory", den(rs))
	}
}

// TestCreditAfterUntil: the window [10-24, 10-31) holds a show of m1 on
// 10-29; its wrong credit is written on 11-01, after Until, and still
// counts: verdict.wrong_rate 1/1.
func TestCreditAfterUntil(t *testing.T) {
	x := newFx(t, fxNov)
	shown := time.Date(2026, 10, 29, 10, 0, 0, 0, fxParis)
	x.declaredTurn(shown, "oct", "declared", "", "m1")
	x.credit(time.Date(2026, 11, 1, 10, 0, 0, 0, fxParis), "oct", "", "alpha", "wrong", "m1")
	w, err := NewWindow(x.now, 0, time.Date(2026, 10, 24, 0, 0, 0, 0, fxParis), time.Date(2026, 10, 31, 0, 0, 0, 0, fxParis), fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	wr := qualityOf(x, Query{Window: w, Machines: "this"})["verdict.wrong_rate"]
	if num(wr) != 1 || den(wr) != 1 {
		t.Errorf("wrong rate = %d/%d, want 1/1", num(wr), den(wr))
	}
}

// promptShows writes count prompt shows, one session each, the i-th of
// memory m<i> an hour apart, and returns their keys.
func (x *fx) promptShows(prefix, path string, count int) []string {
	x.t.Helper()
	var keys []string
	for i := range count {
		s := fmt.Sprintf("%s-%d", prefix, i)
		at := hoursAgo(i)
		x.turnEvent(at.Add(-time.Second), s, "alpha")
		keys = append(keys, x.shows(at, s, "t1", "", "alpha", path, "", fmt.Sprintf("m%d", i))...)
	}
	return keys
}

// settleAs settles keys in order: the first n[0] with outcomes[0], the
// next n[1] with outcomes[1], …
func (x *fx) settleAs(keys []string, n []int, outcomes []string) {
	x.t.Helper()
	m := map[string]string{}
	k := 0
	for i, c := range n {
		for range c {
			m[keys[k]] = outcomes[i]
			k++
		}
	}
	x.settle(fxNow.Add(-time.Minute), m)
}

// TestPendingShows: 20 settled prompt shows (12 credit, 6 confident_miss,
// 2 unknown) and 3 not settled yet; 2 credited search shows are another
// path. used.share_confident = 12/18 = 0.6667, no interval, too_few (18 <
// 20), unknown 2, pending 3.
func TestPendingShows(t *testing.T) {
	x := newFx(t, fxNow)
	keys := x.promptShows("p", "prompt", 23)
	x.settleAs(keys, []int{12, 6, 2}, []string{feedback.OutcomeCredit, feedback.OutcomeMiss, feedback.OutcomeUnknown})
	x.settleAs(x.promptShows("s", "search", 2), []int{2}, []string{feedback.OutcomeCredit})

	m := qualityOf(x, x.query(7))["used.share_confident"]
	if num(m) != 12 || den(m) != 18 || !close4(val(m), 0.6667) || m.CI != nil || m.Status != StatusTooFew {
		t.Errorf("share confident = %d/%d (%v) ci %v %s, want 12/18 = 0.6667, no ci, too_few", num(m), den(m), val(m), m.CI, m.Status)
	}
	for k, want := range map[string]any{"unknown": int64(2), "pending": int64(3), "shown": int64(0), "forgotten": int64(0), "path": "prompt"} {
		if m.Extra[k] != want {
			t.Errorf("extra.%s = %v, want %v", k, m.Extra[k], want)
		}
	}
	q := x.query(7)
	q.Path = "search"
	if s := qualityOf(x, q)["used.share_confident"]; num(s) != 2 || den(s) != 2 || s.Extra["path"] != "search" {
		t.Errorf("--path search: %d/%d path %v, want 2/2 search", num(s), den(s), s.Extra["path"])
	}
}

// TestImportedShowsExcluded: an import event with 50 credited items in
// the window lands in the imports table and changes neither
// used.share_confident nor used.rank.
func TestImportedShowsExcluded(t *testing.T) {
	x := newFx(t, fxNow)
	keys := x.promptShows("p", "prompt", 4)
	x.settleAs(keys, []int{3, 1}, []string{feedback.OutcomeCredit, feedback.OutcomeMiss})
	before := qualityOf(x, x.query(7))

	e := feedback.Event{TS: hoursAgo(1), Kind: feedback.KindImport, Source: "telemetry"}
	for i := range 50 {
		id := fmt.Sprintf("imp%d", i)
		e.Settled = append(e.Settled, feedback.Settled{Key: "old|ref|prompt|" + id, ID: id, At: hoursAgo(2), Outcome: feedback.OutcomeCredit, Rank: 1})
	}
	x.journal(e)
	after := qualityOf(x, x.query(7))
	var imported int
	if err := x.ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM imports WHERE outcome = 'credit'`).Scan(&imported)
	}); err != nil {
		t.Fatal(err)
	}
	if imported != 50 {
		t.Fatalf("%d imported credits in the index, want 50", imported)
	}
	for _, id := range []string{"used.share_confident", "used.rank"} {
		b, a := before[id], after[id]
		if num(a) != num(b) || den(a) != den(b) || val(a) != val(b) || extraText(a) != extraText(b) {
			t.Errorf("%s moved with the import: %d/%d %s → %d/%d %s", id, num(b), den(b), extraText(b), num(a), den(a), extraText(a))
		}
	}
	if s := after["used.share_confident"]; num(s) != 3 || den(s) != 4 {
		t.Errorf("share confident = %d/%d, want 3/4", num(s), den(s))
	}
}

// TestRankHistogramAndHits: credited shows at ranks 1, 1, 2, 4, 7 and one
// unranked in 5 turns (the 7 and the unranked in the same turn), and a
// confident miss at rank 1 in the rank-4 turn → histogram {1: 2, 2: 1,
// 4: 1, 6+: 1, unranked: 1}, hit@1 = 2/5 = 0.4, hit@3 = 3/5 = 0.6; the
// value counts the 6 credited shows, the trials the 5 turns (too_few).
func TestRankHistogramAndHits(t *testing.T) {
	x := newFx(t, fxNow)
	out := map[string]string{}
	for i, ranks := range [][]int{{1}, {1}, {2}, {4}, {7, 0}} {
		s := fmt.Sprintf("r-%d", i)
		for j, r := range ranks {
			out[x.rankedShow(hoursAgo(i).Add(time.Duration(j)*time.Minute), s, "t1", "prompt", fmt.Sprintf("m%d%d", i, j), r)] = feedback.OutcomeCredit
		}
	}
	out[x.rankedShow(hoursAgo(3).Add(5*time.Minute), "r-3", "t1", "prompt", "miss", 1)] = feedback.OutcomeMiss
	x.settle(fxNow.Add(-time.Minute), out)

	m := qualityOf(x, x.query(7))["used.rank"]
	want := map[string]int64{"1": 2, "2": 1, "3": 0, "4": 1, "5": 0, "6+": 1, "unranked": 1}
	if !sameMap(m.Extra["histogram"], want) {
		t.Errorf("histogram = %v, want %v", m.Extra["histogram"], want)
	}
	if h1, h3 := extraRatio(m, "hit_at_1"), extraRatio(m, "hit_at_3"); !close4(h1, 0.4) || !close4(h3, 0.6) {
		t.Errorf("hit@1 = %v, hit@3 = %v, want 0.4 and 0.6", h1, h3)
	}
	if val(m) != 6 || den(m) != 5 || m.Status != StatusTooFew {
		t.Errorf("rank value %v den %d %s, want 6 credited shows over 5 turns, too_few", val(m), den(m), m.Status)
	}

	// A turn whose only credited show is unranked counts in the trials,
	// never as a hit: hit@1 = hit@3 = 0/1.
	y := newFx(t, fxNow)
	y.settle(fxNow.Add(-time.Minute), map[string]string{y.rankedShow(hoursAgo(1), "u", "t1", "prompt", "m1", 0): feedback.OutcomeCredit})
	u := qualityOf(y, y.query(7))["used.rank"]
	if den(u) != 1 || extraRatio(u, "hit_at_1") != 0 || extraRatio(u, "hit_at_3") != 0 {
		t.Errorf("unranked only: den %d hit@1 %v hit@3 %v, want 1, 0, 0", den(u), extraRatio(u, "hit_at_1"), extraRatio(u, "hit_at_3"))
	}
}

// TestByPath: prompt 14 credit of 20 decided, search 3 of 5 → each path
// judged on its own trials; the value is the prompt path's.
func TestByPath(t *testing.T) {
	x := newFx(t, fxNow)
	x.settleAs(x.promptShows("p", "prompt", 20), []int{14, 6}, []string{feedback.OutcomeCredit, feedback.OutcomeMiss})
	x.settleAs(x.promptShows("s", "search", 5), []int{3, 2}, []string{feedback.OutcomeCredit, feedback.OutcomeMiss})

	m := qualityOf(x, x.query(7))["used.by_path"]
	bp, ok := m.Extra["by_path"].(map[string]pathStat)
	if !ok || len(bp) != 2 {
		t.Fatalf("by_path = %#v, want the prompt and search paths", m.Extra["by_path"])
	}
	for p, want := range map[string]struct {
		num, den int64
		value    float64
		status   Status
	}{"prompt": {14, 20, 0.7, StatusOK}, "search": {3, 5, 0.6, StatusTooFew}} {
		got := bp[p]
		if got.Num != want.num || got.Den != want.den || got.Value == nil || !close4(*got.Value, want.value) || got.Status != want.status {
			t.Errorf("by_path.%s = %+v, want %d/%d = %v %s", p, got, want.num, want.den, want.value, want.status)
		}
	}
	if !close4(val(m), 0.7) || m.Status != StatusOK {
		t.Errorf("value %v %s, want the prompt path's 0.7, ok", val(m), m.Status)
	}
}

// TestRubberStamp: 30 declared turns showing two memories, 29 crediting
// both → 29/30, lower bound 0.8333 > 0.80 → watch. A declared turn with
// one memory and an undeclared one crediting both are not counted.
func TestRubberStamp(t *testing.T) {
	x := newFx(t, fxNow)
	for i := range 30 {
		at := hoursAgo(i)
		s := fmt.Sprintf("s-%d", i)
		x.declaredTurn(at, s, "declared", "", "ma", "mb")
		ids := []string{"ma", "mb"}
		if i == 0 {
			ids = ids[:1]
		}
		x.credit(at.Add(30*time.Second), s, "", "alpha", "used", ids...)
	}
	x.declaredTurn(hoursAgo(31), "one", "declared", "used", "ma")
	x.declaredTurn(hoursAgo(32), "undeclared", "undeclared", "used", "ma", "mb")

	m := qualityOf(x, x.query(7))["used.rubber_stamp"]
	if num(m) != 29 || den(m) != 30 || m.Status != StatusWatch || m.CI == nil || !close4(m.CI[0], 0.8333) {
		t.Errorf("rubber stamp = %d/%d %s ci %v, want 29/30 watch, lower bound 0.8333", num(m), den(m), m.Status, m.CI)
	}
}

// TestRescueRate: in turns with a prompt show, 4 credited shows, 1 of them
// on the search path → 0.25, den 4, too_few; a credited search show in a
// turn without a prompt show is not counted.
func TestRescueRate(t *testing.T) {
	x := newFx(t, fxNow)
	out := map[string]string{}
	set := func(keys ...string) {
		for _, k := range keys {
			out[k] = feedback.OutcomeCredit
		}
	}
	set(x.shows(hoursAgo(1), "a", "t1", "", "alpha", "prompt", "", "m1")...)
	set(x.shows(hoursAgo(1).Add(time.Minute), "a", "t1", "", "alpha", "search", "", "m2")...)
	set(x.shows(hoursAgo(2), "b", "t1", "", "alpha", "prompt", "", "m3", "m4")...)
	set(x.shows(hoursAgo(3), "c", "t1", "", "alpha", "search", "", "m5")...)
	x.settle(fxNow.Add(-time.Minute), out)

	m := qualityOf(x, x.query(7))["search.rescue_rate"]
	if num(m) != 1 || den(m) != 4 || !close4(val(m), 0.25) || m.Status != StatusTooFew {
		t.Errorf("rescue rate = %d/%d (%v) %s, want 1/4 = 0.25 too_few", num(m), den(m), val(m), m.Status)
	}
}

// TestWrongRate: per-memory credits 15 used, 4 wrong, 1 outdated → 5/20 =
// 0.25, interval [0.1119, 0.4687]: the lower bound is above 0.05 (watch)
// and under 0.15 while the point is above it (borderline). A wrong credit
// naming no memory and a none verdict are not counted.
func TestWrongRate(t *testing.T) {
	x := newFx(t, fxNow)
	for i := range 20 {
		verdict := "used"
		switch {
		case i >= 19:
			verdict = "outdated"
		case i >= 15:
			verdict = "wrong"
		}
		x.declaredTurn(hoursAgo(i), fmt.Sprintf("s-%d", i), "declared", verdict, fmt.Sprintf("m%d", i))
	}
	x.credit(hoursAgo(0).Add(time.Minute), "s-0", "", "alpha", "wrong")
	x.credit(hoursAgo(1).Add(time.Minute), "s-1", "", "alpha", "none", "m1")

	m := qualityOf(x, x.query(7))["verdict.wrong_rate"]
	if num(m) != 5 || den(m) != 20 || !close4(val(m), 0.25) {
		t.Fatalf("wrong rate = %d/%d (%v), want 5/20", num(m), den(m), val(m))
	}
	if m.Status != StatusWatch || !m.Borderline || m.CI == nil || !close4(m.CI[0], 0.1119) {
		t.Errorf("status %s borderline %v ci %v, want watch, borderline, lower bound 0.1119", m.Status, m.Borderline, m.CI)
	}
	for k, want := range map[string]int64{"used": 15, "wrong": 4, "outdated": 1} {
		if m.Extra[k] != want {
			t.Errorf("extra.%s = %v, want %d", k, m.Extra[k], want)
		}
	}
}

// staleLedger is TestFeedbackStaleIsMemoryStale's ledger, at fxNow: 3
// never used, 1 hidden for 8 days, 2 reported; an imported-only memory, a
// used one and one hidden for 2 days are not listed.
func staleLedger() feedback.Ledger {
	return feedback.Ledger{Memories: map[string]feedback.Entry{
		"n-b":   {Shown: 12},
		"n-a":   {Shown: 15},
		"n-c":   {Shown: 12},
		"imp":   {Shown: 30, Imported: feedback.Imported{Shown: 25}},
		"used":  {Shown: 40, Credits: 0.5},
		"h-old": {Shown: 3, Hidden: &feedback.Hidden{Since: fxNow.Add(-8 * day)}},
		"h-new": {Shown: 3, Hidden: &feedback.Hidden{Since: fxNow.Add(-2 * day)}},
		"r-x":   {Shown: 2, Wrong: 1},
		"r-y":   {Shown: 4, Wrong: 2},
	}}
}

// TestStaleCounts: the stale counts are memory stale's lists through the
// index's ledger: 3, 1 and 2, status info, recent-weighted; under a
// project filter they are tagged not filtered (the ledger has no project).
func TestStaleCounts(t *testing.T) {
	x := newFx(t, fxNow)
	x.ledger(staleLedger())
	m := qualityOf(x, x.query(7))
	for id, want := range map[string]float64{"stale.never_used": 3, "stale.hidden": 1, "stale.reported": 2} {
		s := m[id]
		if val(s) != want || s.Status != StatusInfo || s.Aggregation != "recent_weighted" || len(s.Tags) != 0 {
			t.Errorf("%s = %v %s %s tags %v, want %v info recent_weighted", id, val(s), s.Status, s.Aggregation, s.Tags, want)
		}
	}
	q := x.query(7)
	q.Project = "alpha"
	if s := qualityOf(x, q)["stale.never_used"]; val(s) != 3 || !slices.Contains(s.Tags, tagNotFiltered) {
		t.Errorf("under a project filter: %v tags %v, want 3 tagged not_filtered", val(s), s.Tags)
	}
	y := newFx(t, fxNow)
	y.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{}})
	if s := qualityOf(y, y.query(7))["stale.hidden"]; s.Status != StatusNoData || s.Reason != ReasonNoEvents {
		t.Errorf("empty ledger: %s %q, want no_data no_events", s.Status, s.Reason)
	}
}

// TestFeedbackOff: with feedback off, or on a server-only host, every
// quality metric reads no_data with its reason, whatever the journal holds.
func TestFeedbackOff(t *testing.T) {
	for _, c := range []struct {
		edit   func(*config.Config)
		reason string
	}{
		{func(c *config.Config) { c.Feedback.Enabled = false }, ReasonFeedbackOff},
		{func(c *config.Config) { c.Server.Role = config.RoleServer }, ReasonServerOnlyHost},
	} {
		x := newFx(t, fxNow)
		x.declaredTurn(hoursAgo(1), "s", "declared", "used", "m1")
		x.ledger(staleLedger())
		x.cfg = c.edit
		m := qualityOf(x, x.query(7))
		if len(m) != len(qualityIDs) {
			t.Fatalf("%d metrics, want %d", len(m), len(qualityIDs))
		}
		for id, mm := range m {
			if mm.Status != StatusNoData || mm.Reason != c.reason {
				t.Errorf("%s = %s %q, want no_data %s", id, mm.Status, mm.Reason, c.reason)
			}
		}
	}
}

// TestMemoryProjectIsItsLatestShow: a memory's project is that of its
// latest show carrying one; unknown memories have none.
func TestMemoryProjectIsItsLatestShow(t *testing.T) {
	x := newFx(t, fxNow)
	x.shows(hoursAgo(2), "s", "t1", "", "alpha", "prompt", "gamma", "m1")
	x.shows(hoursAgo(1), "s", "t2", "", "alpha", "prompt", "delta", "m1")
	x.source()
	var got, none string
	if err := x.ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
		got, none = memoryProject(tx, "m1"), memoryProject(tx, "m-unknown")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != "delta" || none != "" {
		t.Errorf("memoryProject = %q and %q, want delta and empty", got, none)
	}
}
