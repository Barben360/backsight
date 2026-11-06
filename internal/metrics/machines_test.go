package metrics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/machines"
	"github.com/Barben360/backsight/internal/telemetry"
)

const (
	pcThis   = "aaaaaaaa-0000-4000-8000-000000000001"
	pcOther1 = "bbbbbbbb-0000-4000-8000-000000000002"
	pcOther2 = "cccccccc-0000-4000-8000-000000000003"
)

// writeIndex runs stmts on the fixture's index through its own connection,
// as Task 6's refresh would have filled the pc_* tables.
func (x *fx) writeIndex(stmt string, args ...any) {
	x.t.Helper()
	x.source() // opens the index
	db, err := sql.Open("sqlite", "file:"+x.ix.Path())
	if err != nil {
		x.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(stmt, args...); err != nil {
		x.t.Fatal(err)
	}
}

// pcDoc adds a counted-or-not other PC document last sent at at.
func (x *fx) pcDoc(machine, first string, at time.Time, until string) {
	x.t.Helper()
	x.writeIndex(`INSERT INTO pc_docs(machine, os, first_seen, at, binary_version, until_day) VALUES (?, 'linux', ?, ?, '0.1.30', ?)`,
		machine, first, at.UnixMilli(), until)
}

// pcDay adds an other PC's day totals: prompts typed, hit_n of hit_d.
func (x *fx) pcDay(machine, day string, prompts, hitN, hitD int) {
	x.t.Helper()
	d, err := time.Parse("2006-01-02", day)
	if err != nil {
		x.t.Fatal(err)
	}
	x.writeIndex(`INSERT INTO pc_days(machine, day, version, noon, prompts, hit_n, hit_d, used_n, used_d, cov_n, cov_d, wrong_n, wrong_d,
 ctx_chars, ctx_sessions, flow_n, flow_d, lat0, lat1, lat2, lat3, lat4, lat5, lat6, lat7, lat8)
 VALUES (?, ?, '', ?, ?, ?, ?, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)`,
		machine, day, d.Add(12*time.Hour).UnixMilli(), prompts, hitN, hitD)
}

func (x *fx) pcIgnored(pos int, machine, reason string) {
	x.t.Helper()
	x.writeIndex(`INSERT INTO pc_ignored(pos, machine, reason) VALUES (?, ?, ?)`, pos, machine, reason)
}

// runAs runs view for q with this PC identified as pcThis.
func (x *fx) runAs(view string, q Query) Result {
	x.t.Helper()
	src := x.source()
	src.Self = func() SelfPC {
		return SelfPC{Machine: pcThis, OS: "linux", FirstSeen: "2026-09-01", BinaryVersion: "0.1.30"}
	}
	r, err := Run(context.Background(), view, src, q)
	if err != nil {
		x.t.Fatalf("%s: %v", view, err)
	}
	return r
}

func (x *fx) allQuery() Query {
	q := x.query(7)
	q.Machines = "all"
	return q
}

func TestScopePCs(t *testing.T) {
	t.Run("two others, one stale, one ignored", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.prompts("alpha", "p", fxNow, 3, nil)
		x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "2026-10-29")
		x.pcDoc(pcOther2, "2026-09-05", fxNow.Add(-72*time.Hour), "2026-10-27")
		x.pcDay(pcOther1, "2026-10-28", 5, 1, 5)
		x.pcDay(pcOther2, "2026-10-01", 5, 1, 5) // outside the 7 days: idle
		x.pcIgnored(0, "deadbeef", "not_valid")
		r := x.runAs("overview", x.allQuery())
		p := r.Scope.PCs
		if p == nil || !p.Sharing || p.Count != 3 || p.Stale != 1 || p.Ignored != 1 || p.Active != 2 {
			t.Fatalf("pcs = %+v", p)
		}
		if p.Until == nil || *p.Until != "2026-10-27" {
			t.Errorf("until %v, want the oldest, 2026-10-27", p.Until)
		}
		if p.FetchedAt != nil {
			t.Errorf("fetched_at %v with no pc_fetch row", p.FetchedAt)
		}
		want := []PCRow{
			{Machine: "aaaaaaaa", This: true, OS: "linux", FirstSeen: "2026-09-01"},
			{Machine: "cccccccc", OS: "linux", FirstSeen: "2026-09-05", Until: ptr("2026-10-27"), Stale: true},
			{Machine: "bbbbbbbb", OS: "linux", FirstSeen: "2026-09-10", Until: ptr("2026-10-29")},
		}
		if !reflect.DeepEqual(p.List, want) {
			t.Errorf("list\n got %+v\nwant %+v", p.List, want)
		}
	})
	t.Run("the fetch time", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.writeIndex(`INSERT INTO pc_fetch(id, fetched_at, size, mtime, sha256, ingested_at) VALUES (1, ?, 1, 1, 'x', 1)`, fxNow.Add(-time.Hour).UnixMilli())
		r := x.runAs("quality", x.query(7))
		if f := r.Scope.PCs.FetchedAt; f == nil || !f.Equal(fxNow.Add(-time.Hour)) || f.Location() != fxParis {
			t.Errorf("fetched_at %v", f)
		}
	})
	t.Run("the edges of counted and stale", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-48*time.Hour), "")                                         // exactly 48 h: counted, not stale
		x.pcDoc(pcOther2, "2026-09-11", fxNow.Add(-48*time.Hour-time.Millisecond), "")                        // stale
		x.pcDoc("dddddddd-0000-4000-8000-000000000004", "", fxNow.Add(-30*24*time.Hour), "")                  // exactly 30 d: counted
		x.pcDoc("eeeeeeee-0000-4000-8000-000000000005", "", fxNow.Add(-30*24*time.Hour-time.Millisecond), "") // dropped
		x.pcDoc("ffffffff-0000-4000-8000-000000000006", "", fxNow.Add(24*time.Hour), "")                      // exactly +24 h: counted
		x.pcDoc("99999999-0000-4000-8000-000000000007", "", fxNow.Add(24*time.Hour+time.Millisecond), "")     // from the future: dropped
		p := x.runAs("overview", x.query(7)).Scope.PCs
		if p.Count != 5 || p.Stale != 2 { // this + 4 others; stale: 48h+1ms and 30 d
			t.Errorf("count %d stale %d, want 5 and 2", p.Count, p.Stale)
		}
		if p.Until != nil {
			t.Errorf("until %v with no until_day", *p.Until)
		}
	})
	t.Run("a server-only host", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.cfg = func(c *config.Config) { c.Server.Role = config.RoleServer }
		x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
		x.pcDoc(pcOther2, "2026-09-05", fxNow.Add(-time.Hour), "")
		p := x.runAs("overview", x.query(7)).Scope.PCs
		if p.Count != 2 || len(p.List) != 2 || p.List[0].This || p.List[1].This {
			t.Errorf("pcs = %+v", p)
		}
	})
	t.Run("no other PC", func(t *testing.T) {
		x := newFx(t, fxNow)
		p := x.runAs("bank", x.query(7)).Scope.PCs
		if p.Count != 1 || p.Stale != 0 || p.Ignored != 0 || len(p.List) != 1 || !p.List[0].This {
			t.Errorf("pcs = %+v", p)
		}
	})
	t.Run("short ids that collide show 13 characters", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.pcDoc("3f9c2a17-1b2c-4000-8000-000000000001", "2026-09-10", fxNow, "")
		x.pcDoc("3f9c2a17-9999-4000-8000-000000000002", "2026-09-11", fxNow, "")
		x.pcDoc(pcOther1, "2026-09-12", fxNow, "")
		p := x.runAs("overview", x.query(7)).Scope.PCs
		var got []string
		for _, r := range p.List {
			got = append(got, r.Machine)
		}
		if want := []string{"aaaaaaaa", "3f9c2a17-1b2c", "3f9c2a17-9999", "bbbbbbbb"}; !reflect.DeepEqual(got, want) {
			t.Errorf("machines %v, want %v", got, want)
		}
	})
	t.Run("this PC counts in the collision check", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.pcDoc("aaaaaaaa-1111-4000-8000-000000000009", "2026-09-10", fxNow, "")
		p := x.runAs("overview", x.query(7)).Scope.PCs
		if p.List[0].Machine != "aaaaaaaa-0000" || p.List[1].Machine != "aaaaaaaa-1111" {
			t.Errorf("list %+v", p.List)
		}
	})
	t.Run("an unknown self still counts", func(t *testing.T) {
		x := newFx(t, fxNow)
		r, err := Run(context.Background(), "overview", x.source(), x.query(7)) // Source.Self is nil
		if err != nil {
			t.Fatal(err)
		}
		if p := r.Scope.PCs; p.Count != 1 || p.List[0].Machine != "" || !p.List[0].This || p.List[0].FirstSeen != "" {
			t.Errorf("pcs = %+v", p)
		}
	})
}

// TestScopePCsIsInEveryView: every view carries the block, in both modes.
func TestScopePCsIsInEveryView(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "p", fxNow, 3, nil)
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	for _, mode := range []string{"this", "all"} {
		q := x.query(7)
		q.Machines = mode
		for _, view := range Views {
			r := x.runAs(view, q)
			if r.Scope.PCs == nil || r.Scope.PCs.Count != 2 || r.Scope.PCs.List == nil {
				t.Errorf("%s/%s: pcs = %+v", view, mode, r.Scope.PCs)
			}
		}
	}
}

func TestSharingOffAnswersThisPC(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "p", fxNow, 30, func(i int, p *telemetry.Prompt) { p.Injected = i % 2 })
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	for d := 1; d <= 6; d++ {
		x.pcDay(pcOther1, fxNow.AddDate(0, 0, -d).UTC().Format("2006-01-02"), 100, 0, 100)
	}
	x.cfg = func(c *config.Config) { c.Feedback.ShareLedger = false }
	all := x.runAs("quality", x.allQuery())
	this := x.runAs("quality", x.query(7))
	if all.Scope.Machines != "this" || all.Scope.PCs.Sharing || all.Scope.PCs.Count != 0 || all.Scope.OthersUnfiltered {
		t.Errorf("scope %+v pcs %+v", all.Scope, all.Scope.PCs)
	}
	if !reflect.DeepEqual(all.Metrics, this.Metrics) || !reflect.DeepEqual(all.Tables, this.Tables) {
		t.Errorf("sharing off: all differs from this")
	}
	x.cfg = nil
	if r := x.runAs("quality", x.allQuery()); r.Scope.Machines != "all" || !r.Scope.PCs.Sharing {
		t.Errorf("sharing on: scope %+v", r.Scope)
	}
}

func TestBrowseViewsStayThisPC(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "p", fxNow, 3, nil)
	x.declaredTurn(fxNow.Add(-3*time.Hour), "sess-item-1", "declared", "used", "mem-item-1")
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	for _, view := range []string{"projects", "sessions", "memories"} {
		q := x.allQuery()
		if r := x.runAs(view, q); r.Scope.Machines != "this" || r.Scope.OthersUnfiltered || r.Scope.PCs.Count != 2 {
			t.Errorf("%s list: scope %+v pcs %+v", view, r.Scope, r.Scope.PCs)
		}
		q.ID = "alpha"
		if view == "sessions" {
			q.ID = "sess-item-1"
		}
		if view == "memories" {
			q.ID = "mem-item-1"
		}
		q.Project = "alpha"
		r, err := Run(context.Background(), view, x.srcAs(), q)
		if err != nil {
			t.Fatalf("%s item %q: %v", view, q.ID, err)
		}
		if r.Scope.Machines != "this" || r.Scope.OthersUnfiltered {
			t.Errorf("%s item: scope %+v", view, r.Scope)
		}
	}
	// the views that merge say all
	if r := x.runAs("cost", x.allQuery()); r.Scope.Machines != "all" {
		t.Errorf("cost: machines %q", r.Scope.Machines)
	}
}

func (x *fx) srcAs() Source {
	src := x.source()
	src.Self = func() SelfPC { return SelfPC{Machine: pcThis} }
	return src
}

// TestVerdictIgnoresTheSwitch: the other PC's days (a hit rate of 0 over
// 14 days, which would put Found in attention if they merged) change
// neither the verdict, the attention list nor the info items.
func TestVerdictIgnoresTheSwitch(t *testing.T) {
	x := newFx(t, fxNow)
	firstPrompt(x)
	recalls(x, "cur", fxNow, 40, 40, 0, 0)
	recalls(x, "prev", since7, 40, 40, 0, 0)
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	for d := 1; d <= 14; d++ {
		x.pcDay(pcOther1, fxNow.AddDate(0, 0, -d).UTC().Format("2006-01-02"), 500, 0, 500)
	}
	this := x.runAs("overview", x.query(7))
	all := x.runAs("overview", x.allQuery())
	if this.Verdict == nil || this.Verdict.Axes["found"].State != okay {
		t.Fatalf("this PC's found is %+v, want ok", this.Verdict)
	}
	if !reflect.DeepEqual(all.Verdict, this.Verdict) || !reflect.DeepEqual(all.Attention, this.Attention) || !reflect.DeepEqual(all.Info, this.Info) {
		t.Errorf("the switch changed the verdict:\nall  %+v %+v %+v\nthis %+v %+v %+v", all.Verdict, all.Attention, all.Info, this.Verdict, this.Attention, this.Info)
	}
	if all.Scope.Machines != "all" || all.Scope.PCs.Count != 2 {
		t.Errorf("scope %+v pcs %+v", all.Scope, all.Scope.PCs)
	}
}

// The verdict reads this PC's whatever the env says, and its runs are keyed
// apart from the merged ones.
func TestAssessRunsOnThisPC(t *testing.T) {
	x := newFx(t, fxNow)
	e := env{q: x.allQuery()}
	if e.this().q.Machines != "this" || e.q.Machines != "all" || e.this().machines() != "this" || e.machines() != "all" {
		t.Errorf("this() must copy: %q %q", e.this().q.Machines, e.q.Machines)
	}
	k1, k2 := runKey{machines: "this"}, runKey{machines: "all"}
	if k1 == k2 {
		t.Error("the memo key does not hold machines")
	}
	// only "all", on a view that merges, with sharing on, merges
	on := config.Config{Feedback: config.Feedback{Enabled: true, ShareLedger: true}}
	for _, c := range []struct {
		view, machines string
		cfg            config.Config
		want           bool
	}{
		{"overview", "all", on, true}, {"releases", "all", on, true}, {"days", "all", on, true},
		{"overview", "this", on, false}, {"overview", "all", config.Config{}, false},
		{"bank", "all", on, false}, {"system", "all", on, false}, {"sessions", "all", on, false},
	} {
		m := env{view: c.view, q: Query{Machines: c.machines}, src: Source{Config: c.cfg}}
		if m.merging() != c.want {
			t.Errorf("%s/%s: merging() = %v", c.view, c.machines, !c.want)
		}
		if m.this().merging() {
			t.Errorf("%s/%s: this() must not merge", c.view, c.machines)
		}
	}
}

func TestOthersUnfiltered(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "p", fxNow, 3, nil)
	q := x.allQuery()
	q.Project = "alpha"
	if r := x.runAs("quality", q); r.Scope.OthersUnfiltered {
		t.Error("no other PC: others_unfiltered must be absent")
	}
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	if r := x.runAs("quality", q); !r.Scope.OthersUnfiltered || r.Scope.Project != "alpha" {
		t.Errorf("all + project + another PC: scope %+v", r.Scope)
	}
	q.Machines = "this"
	if r := x.runAs("quality", q); r.Scope.OthersUnfiltered {
		t.Error("this: others_unfiltered must be absent")
	}
	qa := x.allQuery()
	if r := x.runAs("quality", qa); r.Scope.OthersUnfiltered {
		t.Error("no project filter: others_unfiltered must be absent")
	}
	x.cfg = func(c *config.Config) { c.Feedback.ShareLedger = false }
	q.Machines = "all"
	if r := x.runAs("quality", q); r.Scope.OthersUnfiltered {
		t.Error("sharing off: others_unfiltered must be absent")
	}
}

// mergedOf runs view for q as all PCs and returns the metrics.
func (x *fx) mergedOf(view string, q Query) map[string]Metric {
	x.t.Helper()
	q.Machines = "all"
	return x.runAs(view, q).Metrics
}

// seriesOf is a metric's daily series as day -> [num, den].
func seriesOf(m Metric) map[string][2]int64 {
	out := map[string][2]int64{}
	for _, p := range m.Series {
		if p.Num != nil && p.Den != nil {
			out[p.Day+p.Hour] = [2]int64{*p.Num, *p.Den}
		}
	}
	return out
}

func sameSeries(t *testing.T, id string, m Metric, want map[string][2]int64) {
	t.Helper()
	got := seriesOf(m)
	for k, v := range got {
		if v != want[k] {
			t.Errorf("%s series %s = %v, want %v", id, k, v, want[k])
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s series %s = %v, want %v", id, k, got[k], v)
		}
	}
}

func num0(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func isNum(t *testing.T, id string, m Metric, num, den int64) {
	t.Helper()
	if m.Num == nil || m.Den == nil || *m.Num != num || *m.Den != den {
		t.Fatalf("%s = %d/%d (status %s %q), want %d/%d", id, num0(m.Num), num0(m.Den), m.Status, m.Reason, num, den)
	}
	if m.Value == nil || !close4(*m.Value, float64(num)/float64(den)) {
		t.Errorf("%s value %v, want %d/%d", id, m.Value, num, den)
	}
}

// thisFigures writes this PC's part of the two-PC fixture: 30 recalls of
// which 12 injected, 9 declared turns of which 5 used, 2 undeclared turns
// (decided 11), 6 used verdicts and none wrong, 3 documents sent, all ok.
// Paris days (the window's zone), hand-counted:
//
//	Oct 30: recalls at 11:00 down to 00:00 = 12, all injected; declared turns
//	        at 09:00..01:00 = 9, the first 5 credited used (5 verdicts); 3 documents
//	Oct 29: the other 18 recalls (23:00..06:00), none injected; the 2
//	        undeclared turns at 03:00 and 02:00, the first credited used (1 verdict)
func thisFigures(x *fx) {
	x.prompts("alpha", "cur", fxNow, 30, injecting(12))
	turns(x, "d", fxNow, 9, "declared", firstK(5, "used", ""))
	turns(x, "u", fxNow.Add(-30*time.Hour), 2, "undeclared", firstK(1, "used", ""))
	for i, h := range []time.Duration{5, 6, 7} {
		x.retained(fxNow.Add(-h*time.Hour), fmt.Sprintf("doc-%d", i), "ok", 100)
	}
}

// The other PC of the fixture, over two days. Sums: hit 16+10 / 40+30 =
// 26/70, used 6+3 / 12+9 = 9/21, coverage 12+9 / 14+11 = 21/25, wrong 1+0 /
// 8+6 = 1/14, flow 3+2 / 3+2 = 5/5.
func otherPC(x *fx) {
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "2026-10-29")
	x.pcRows(
		pcRow{machine: pcOther1, day: "2026-10-29", f: machines.Figures{Prompts: 40, Hit: machines.Pair{16, 40}, Used: machines.Pair{6, 12},
			Coverage: machines.Pair{12, 14}, Wrong: machines.Pair{1, 8}}, flow: machines.Pair{3, 3}},
		pcRow{machine: pcOther1, day: "2026-10-27", f: machines.Figures{Prompts: 30, Hit: machines.Pair{10, 30}, Used: machines.Pair{3, 9},
			Coverage: machines.Pair{9, 11}, Wrong: machines.Pair{0, 6}}, flow: machines.Pair{2, 2}},
	)
}

// TestMergeTwoPCsGivesExactSums: this PC 12/30 + the other 26/70 = 38/100,
// and so for each rate and the sessions sent; every series day is the sum of
// both, the interval, MDC and status come from the merged counts.
func TestMergeTwoPCsGivesExactSums(t *testing.T) {
	x := newFx(t, fxNow)
	thisFigures(x)
	otherPC(x)

	// this PC alone: the fixture's own arithmetic
	this := x.runAs("quality", x.query(7)).Metrics
	isNum(t, "this hit", this["recall.hit_rate"], 12, 30)
	isNum(t, "this used", this["used.turn_rate"], 5, 9)
	isNum(t, "this coverage", this["declare.coverage"], 9, 11)
	isNum(t, "this wrong", this["verdict.wrong_rate"], 0, 6)
	isNum(t, "this flow", x.runAs("pipeline", x.query(7)).Metrics["pipeline.flow"], 3, 3)

	m := x.mergedOf("quality", x.query(7))
	hit := m["recall.hit_rate"]
	isNum(t, "hit", hit, 38, 100)
	lo, hi := Wilson(38, 100)
	if hit.CI == nil || hit.CI[0] != lo || hit.CI[1] != hi {
		t.Errorf("hit ci %v, want Wilson(38,100) = [%v %v]", hit.CI, lo, hi)
	}
	if hit.MDC == nil || *hit.MDC != MDC(100) {
		t.Errorf("hit mdc %v, want MDC(100)", hit.MDC)
	}
	// 38% with an interval of about [29%, 48%]: not under 10%, no baseline
	if hit.Status != StatusOK {
		t.Errorf("hit status %s, want ok", hit.Status)
	}
	sameSeries(t, "hit", hit, map[string][2]int64{"2026-10-30": {12, 12}, "2026-10-29": {16, 58}, "2026-10-27": {10, 30},
		"2026-10-23": {0, 0}, "2026-10-24": {0, 0}, "2026-10-25": {0, 0}, "2026-10-26": {0, 0}, "2026-10-28": {0, 0}})

	used := m["used.turn_rate"]
	isNum(t, "used", used, 14, 30)
	if wc, _ := used.Extra["worst_case"].(*float64); wc == nil || !close4(*wc, 14.0/36) {
		t.Errorf("worst_case %v, want 14/36", used.Extra["worst_case"])
	}
	if used.Extra["pending"] != int64(0) {
		t.Errorf("pending %v: this PC's", used.Extra["pending"])
	}
	if slices.Contains(used.Tags, tagUnreliable) {
		t.Errorf("used tags %v: coverage 30/36 is reliable", used.Tags)
	}
	sameSeries(t, "used", used, map[string][2]int64{"2026-10-30": {5, 9}, "2026-10-29": {6, 12}, "2026-10-27": {3, 9},
		"2026-10-23": {0, 0}, "2026-10-24": {0, 0}, "2026-10-25": {0, 0}, "2026-10-26": {0, 0}, "2026-10-28": {0, 0}})
	cov := m["declare.coverage"]
	isNum(t, "coverage", cov, 30, 36)
	sameSeries(t, "coverage", cov, map[string][2]int64{"2026-10-30": {9, 9}, "2026-10-29": {12, 16}, "2026-10-27": {9, 11},
		"2026-10-23": {0, 0}, "2026-10-24": {0, 0}, "2026-10-25": {0, 0}, "2026-10-26": {0, 0}, "2026-10-28": {0, 0}})
	wrong := m["verdict.wrong_rate"]
	isNum(t, "wrong", wrong, 1, 20)
	lo, hi = Wilson(1, 20)
	if wrong.CI == nil || wrong.CI[0] != lo || wrong.CI[1] != hi || wrong.Status != StatusOK {
		t.Errorf("wrong ci %v status %s, want Wilson(1,20) and ok (its lower bound %.3f is under 5%%)", wrong.CI, wrong.Status, lo)
	}
	sameSeries(t, "wrong", wrong, map[string][2]int64{"2026-10-30": {0, 5}, "2026-10-29": {1, 9}, "2026-10-27": {0, 6},
		"2026-10-23": {0, 0}, "2026-10-24": {0, 0}, "2026-10-25": {0, 0}, "2026-10-26": {0, 0}, "2026-10-28": {0, 0}})

	flow := x.mergedOf("pipeline", x.query(7))["pipeline.flow"]
	isNum(t, "flow", flow, 8, 8)
	if flow.Status != StatusOK || !slices.Contains(flow.Tags, tagStatusThisPC) {
		t.Errorf("flow status %s tags %v, want this PC's ok with status_this_pc", flow.Status, flow.Tags)
	}
	sameSeries(t, "flow", flow, map[string][2]int64{"2026-10-30": {3, 3}, "2026-10-29": {3, 3}, "2026-10-27": {2, 2},
		"2026-10-23": {0, 0}, "2026-10-24": {0, 0}, "2026-10-25": {0, 0}, "2026-10-26": {0, 0}, "2026-10-28": {0, 0}})

	// this PC's mode is untouched by the same index
	isNum(t, "this hit again", x.runAs("quality", x.query(7)).Metrics["recall.hit_rate"], 12, 30)
}

// The status of a rate is read from the merged counts: this PC alone reads
// 12/30 (ok), with 200 more recalls of which none injected the merged 12/230
// has an interval under 10% (upper bound about 8.9%): watch.
func TestMergedStatusComesFromTheMergedCounts(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "cur", fxNow, 30, injecting(12))
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcRows(pcRow{machine: pcOther1, day: "2026-10-28", f: machines.Figures{Prompts: 200, Hit: machines.Pair{0, 200}}})
	if s := x.runAs("quality", x.query(7)).Metrics["recall.hit_rate"].Status; s != StatusOK {
		t.Fatalf("this PC alone reads %s, want ok", s)
	}
	hit := x.mergedOf("quality", x.query(7))["recall.hit_rate"]
	isNum(t, "hit", hit, 12, 230)
	if hit.Status != StatusWatch {
		t.Errorf("merged status %s, want watch", hit.Status)
	}
	if hit.CI == nil || hit.CI[1] >= 0.10 || hit.CI[1] < 0.085 {
		t.Errorf("merged ci %v, want an upper bound near 0.089", hit.CI)
	}
}

// The failed document stays this PC's: 2 ok + 1 failed here, 5/5 there is
// 7/8 whose status is this PC's watch.
func TestMergedFlowKeepsThisPCsStatus(t *testing.T) {
	x := newFx(t, fxNow)
	x.retained(fxNow.Add(-5*time.Hour), "d1", "ok", 100)
	x.retained(fxNow.Add(-6*time.Hour), "d2", "ok", 100)
	x.retained(fxNow.Add(-7*time.Hour), "d3", "failed", 100)
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcRows(pcRow{machine: pcOther1, day: "2026-10-28", flow: machines.Pair{5, 5}})
	if s := x.runAs("pipeline", x.query(7)).Metrics["pipeline.flow"]; s.Status != StatusWatch {
		t.Fatalf("this PC reads %s, want watch (one failed)", s.Status)
	}
	flow := x.mergedOf("pipeline", x.query(7))["pipeline.flow"]
	isNum(t, "flow", flow, 7, 8)
	if flow.Status != StatusWatch || !slices.Contains(flow.Tags, tagStatusThisPC) {
		t.Errorf("flow status %s tags %v, want watch and status_this_pc", flow.Status, flow.Tags)
	}
	if flow.Extra["failed"] != int64(1) || flow.Extra["ok"] != int64(2) {
		t.Errorf("flow extra %v: this PC's", flow.Extra)
	}
}

// used.turn_rate is unreliable while the merged coverage's upper bound is
// under 80%: this PC declares 20 of 20 (bound 1), the other PC 0 of 180,
// so 20/200 (upper bound about 0.15).
func TestMergedUnreliableReadsTheMergedCoverage(t *testing.T) {
	x := newFx(t, fxNow)
	turns(x, "d", fxNow, 20, "declared", nil)
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcRows(pcRow{machine: pcOther1, day: "2026-10-28", f: machines.Figures{Prompts: 1, Coverage: machines.Pair{0, 180}}})
	if m := x.runAs("quality", x.query(7)).Metrics["used.turn_rate"]; slices.Contains(m.Tags, tagUnreliable) {
		t.Fatalf("this PC alone is reliable, tags %v", m.Tags)
	}
	m := x.mergedOf("quality", x.query(7))
	isNum(t, "coverage", m["declare.coverage"], 20, 200)
	if used := m["used.turn_rate"]; !slices.Contains(used.Tags, tagUnreliable) {
		t.Errorf("used tags %v, want unreliable from the merged coverage", used.Tags)
	}
}

// TestMiddayRuleTakesNDays: a PC with a day a row, windows of 7 days ending
// 00:30, 11:59, 12:00 and 23:59 UTC in three zones all hold exactly 7 of its
// days (a day counts when its 12:00 UTC is in the window); a 24 h window
// exactly 1; a local day of 25 h exactly 1.
func TestMiddayRuleTakesNDays(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	auckland, _ := time.LoadLocation("Pacific/Auckland")
	anchorage, _ := time.LoadLocation("America/Anchorage")
	rows := func(machine string) []pcRow {
		var out []pcRow
		for d := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); d.Before(time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)); d = d.AddDate(0, 0, 1) {
			out = append(out, pcRow{machine: machine, day: d.Format("2006-01-02"), f: machines.Figures{Prompts: 1, Hit: machines.Pair{1, 1}}})
		}
		return out
	}
	run := func(t *testing.T, now time.Time, w Window) Metric {
		x := newFx(t, now)
		x.pcDoc(pcOther1, "2026-09-10", now.Add(-time.Hour), "")
		x.pcRows(rows(pcOther1)...)
		return x.mergedOf("quality", Query{Window: w})["recall.hit_rate"]
	}
	for _, now := range []time.Time{
		time.Date(2026, 10, 30, 0, 30, 0, 0, time.UTC), time.Date(2026, 10, 30, 11, 59, 0, 0, time.UTC),
		time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 30, 23, 59, 0, 0, time.UTC)} {
		for name, loc := range map[string]*time.Location{"UTC": time.UTC, "Paris": paris, "Auckland": auckland} {
			t.Run(fmt.Sprintf("%s %s", name, now.Format("15:04")), func(t *testing.T) {
				w7, err := NewWindow(now, 7, time.Time{}, time.Time{}, loc, name)
				if err != nil {
					t.Fatal(err)
				}
				isNum(t, "7 days", run(t, now, w7), 7, 7)
				w1, _ := NewWindow(now, 1, time.Time{}, time.Time{}, loc, name)
				m := run(t, now, w1)
				isNum(t, "24 h", m, 1, 1)
				if !slices.Contains(m.Tags, tagSeriesThisPC) {
					t.Errorf("24 h tags %v, want series_this_pc", m.Tags)
				}
			})
		}
	}
	// A day lands in the bucket holding its noon: at 12:00 UTC on Oct 30 the
	// window's noons are Oct 23..29. Paris (CEST, then CET from Oct 25 03:00
	// local) is 14:00/13:00 on the same date; Auckland (+13) is 01:00 of the
	// next date.
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		loc  *time.Location
		name string
		days []string
	}{
		{paris, "Europe/Paris", []string{"2026-10-23", "2026-10-24", "2026-10-25", "2026-10-26", "2026-10-27", "2026-10-28", "2026-10-29"}},
		{auckland, "Pacific/Auckland", []string{"2026-10-24", "2026-10-25", "2026-10-26", "2026-10-27", "2026-10-28", "2026-10-29", "2026-10-30"}},
	} {
		w, _ := NewWindow(now, 7, time.Time{}, time.Time{}, c.loc, c.name)
		want := map[string][2]int64{}
		for _, d := range c.days {
			want[d] = [2]int64{1, 1}
		}
		m := run(t, now, w)
		for _, b := range w.Buckets() {
			if _, ok := want[b.Key]; !ok {
				want[b.Key] = [2]int64{0, 0}
			}
		}
		sameSeries(t, c.name, m, want)
	}
	// the day page of a 25 h local day, and a 25 h window holding two noons
	// (Nov 1 11:30Z .. Nov 2 12:30Z): the first only
	late := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name         string
		since, until time.Time
		loc          *time.Location
	}{
		{"Paris 2026-10-25", time.Date(2026, 10, 25, 0, 0, 0, 0, paris), time.Date(2026, 10, 26, 0, 0, 0, 0, paris), paris},
		{"Anchorage 2026-11-01", time.Date(2026, 11, 1, 0, 0, 0, 0, anchorage), time.Date(2026, 11, 2, 0, 0, 0, 0, anchorage), anchorage},
		{"two noons", time.Date(2026, 11, 1, 11, 30, 0, 0, time.UTC), time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC), time.UTC},
	} {
		w, err := NewWindow(late, 0, c.since, c.until, c.loc, c.name)
		if err != nil {
			t.Fatal(err)
		}
		if w.Length() != 25*time.Hour {
			t.Fatalf("%s: window of %v, want 25 h", c.name, w.Length())
		}
		isNum(t, c.name, run(t, late, w), 1, 1)
	}
}

// The previous window and the four baselines are merged runs too. This PC:
// 4/40 in every week; the other PC 40/160 now and 120/160 in each of the four
// weeks before. Merged now 44/200 = 0.22, before 124/200 = 0.62 each: previous
// 0.62, delta -0.40, and the relative rule watches (upper bound of 44/200 is
// about 0.28, under 0.7 x 0.62 = 0.434). This PC alone, 0.1 against 0.1, is
// fine, so only merged baselines can trigger the watch.
func TestMergedPreviousAndBaselines(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "cur", fxNow, 40, injecting(4))
	for k := 1; k <= 4; k++ {
		x.prompts("alpha", fmt.Sprintf("b%d", k), since7.Add(-time.Duration(7*(k-1))*day), 40, injecting(4))
	}
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	rows := []pcRow{{machine: pcOther1, day: "2026-10-27", f: machines.Figures{Prompts: 160, Hit: machines.Pair{40, 160}}}}
	for _, d := range []string{"2026-10-20", "2026-10-12", "2026-10-05", "2026-09-28"} {
		rows = append(rows, pcRow{machine: pcOther1, day: d, f: machines.Figures{Prompts: 160, Hit: machines.Pair{120, 160}}})
	}
	x.pcRows(rows...)

	this := x.runAs("quality", x.query(7)).Metrics["recall.hit_rate"]
	if this.Status != StatusOK || this.Previous == nil || !close4(*this.Previous, 0.1) || this.Delta == nil || !close4(*this.Delta, 0) {
		t.Fatalf("this PC: status %s previous %v delta %v, want ok, 0.1 and 0", this.Status, this.Previous, this.Delta)
	}
	m := x.mergedOf("quality", x.query(7))["recall.hit_rate"]
	isNum(t, "hit", m, 44, 200)
	if m.Previous == nil || !close4(*m.Previous, 0.62) {
		t.Errorf("previous %v, want 124/200 = 0.62", m.Previous)
	}
	if m.Delta == nil || !close4(*m.Delta, -0.40) {
		t.Errorf("delta %v, want 0.22 - 0.62 = -0.40", m.Delta)
	}
	if m.Status != StatusWatch {
		t.Errorf("status %s, want watch from the merged baselines", m.Status)
	}
}

// A counted PC with no day in the window adds nothing and is not active.
func TestMergeWithAnIdlePC(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "cur", fxNow, 30, injecting(12))
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcDoc(pcOther2, "2026-09-11", fxNow.Add(-time.Hour), "")
	x.pcRows(
		pcRow{machine: pcOther1, day: "2026-10-28", f: machines.Figures{Prompts: 70, Hit: machines.Pair{26, 70}}},
		pcRow{machine: pcOther2, day: "2026-09-01", f: machines.Figures{Prompts: 500, Hit: machines.Pair{500, 500}}}) // 59 days back
	r := x.runAs("quality", x.allQuery())
	isNum(t, "hit", r.Metrics["recall.hit_rate"], 38, 100)
	if p := r.Scope.PCs; p.Count != 3 || p.Active != 2 {
		t.Errorf("pcs count %d active %d, want 3 and 2", p.Count, p.Active)
	}
	// with the idle PC alone nothing differs from this PC's own figures
	y := newFx(t, fxNow)
	y.prompts("alpha", "cur", fxNow, 30, injecting(12))
	y.pcDoc(pcOther2, "2026-09-11", fxNow.Add(-time.Hour), "")
	y.pcRows(pcRow{machine: pcOther2, day: "2026-09-01", f: machines.Figures{Prompts: 500, Hit: machines.Pair{500, 500}}})
	if all, this := y.runAs("quality", y.allQuery()).Metrics, y.runAs("quality", y.query(7)).Metrics; !reflect.DeepEqual(all, this) {
		t.Errorf("an idle PC changed the figures:\nall  %+v\nthis %+v", all["recall.hit_rate"], this["recall.hit_rate"])
	}
}

func TestMergeWithNoPCData(t *testing.T) {
	x := newFx(t, fxNow)
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcRows(pcRow{machine: pcOther1, day: "2026-10-28", f: machines.Figures{Prompts: 70, Hit: machines.Pair{26, 70}}})
	if m := x.runAs("quality", x.query(7)).Metrics["recall.hit_rate"]; m.Status != StatusNoData || m.Reason != ReasonNoEvents {
		t.Fatalf("this PC has no prompt: %s %q, want no_data no_events", m.Status, m.Reason)
	}
	hit := x.mergedOf("quality", x.query(7))["recall.hit_rate"]
	isNum(t, "hit", hit, 26, 70)
	if hit.Status != StatusOK {
		t.Errorf("status %s %q, want ok", hit.Status, hit.Reason)
	}
	// the PC with data in another figure only: this one stays without data
	if u := x.mergedOf("quality", x.query(7))["used.turn_rate"]; u.Status != StatusNoData || u.Reason != ReasonNoEvents || u.Num != nil {
		t.Errorf("used: %s %q num %v, want no_data no_events", u.Status, u.Reason, u.Num)
	}
	// neither has prompts: still no data
	y := newFx(t, fxNow)
	y.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	y.pcRows(pcRow{machine: pcOther1, day: "2026-10-28", f: machines.Figures{Prompts: 3}})
	if m := y.mergedOf("quality", y.query(7))["recall.hit_rate"]; m.Status != StatusNoData || m.Reason != ReasonNoEvents {
		t.Errorf("neither has: %s %q, want no_data no_events", m.Status, m.Reason)
	}
}

// A cache that cannot refresh keeps ageing by now: a document 3 days old is
// counted and stale, one 31 days old (fetched 29 days ago) is neither
// counted nor listed, though its rows lie in the window.
func TestOldCacheAgesByNow(t *testing.T) {
	x := newFx(t, fxNow)
	x.writeIndex(`INSERT INTO pc_fetch(id, fetched_at, size, mtime, sha256, ingested_at) VALUES (1, ?, 1, 1, 'x', 1)`, fxNow.Add(-29*24*time.Hour).UnixMilli())
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-3*24*time.Hour), "2026-10-27")
	x.pcDoc(pcOther2, "2026-09-11", fxNow.Add(-31*24*time.Hour), "2026-10-28")
	x.pcRows(
		pcRow{machine: pcOther1, day: "2026-10-27", f: machines.Figures{Prompts: 10, Hit: machines.Pair{5, 10}}},
		pcRow{machine: pcOther2, day: "2026-10-28", f: machines.Figures{Prompts: 10, Hit: machines.Pair{7, 10}}})
	r := x.runAs("quality", x.allQuery())
	isNum(t, "hit", r.Metrics["recall.hit_rate"], 5, 10)
	p := r.Scope.PCs
	if p.Count != 2 || p.Stale != 1 || len(p.List) != 2 || p.List[1].Machine != "bbbbbbbb" || !p.List[1].Stale {
		t.Errorf("pcs = %+v", p)
	}
}

// The edges of counted and stale, as the merge reads them: 48 h is not
// stale, 48 h + 1 s is; 30 d is counted, 30 d + 1 s is dropped; now + 24 h is
// counted, now + 24 h + 1 s is not. Each PC has one day, 1/1.
func TestStaleAndDropBoundaries(t *testing.T) {
	x := newFx(t, fxNow)
	ids := []string{"11111111-0000-4000-8000-000000000001", "22222222-0000-4000-8000-000000000002", "33333333-0000-4000-8000-000000000003",
		"44444444-0000-4000-8000-000000000004", "55555555-0000-4000-8000-000000000005", "66666666-0000-4000-8000-000000000006"}
	ats := []time.Time{fxNow.Add(-48 * time.Hour), fxNow.Add(-48*time.Hour - time.Second), fxNow.Add(-30 * 24 * time.Hour),
		fxNow.Add(-30*24*time.Hour - time.Second), fxNow.Add(24 * time.Hour), fxNow.Add(24*time.Hour + time.Second)}
	var rows []pcRow
	for i, id := range ids {
		x.pcDoc(id, fmt.Sprintf("2026-09-%02d", i+1), ats[i], "")
		rows = append(rows, pcRow{machine: id, day: "2026-10-28", f: machines.Figures{Prompts: 1, Hit: machines.Pair{1, 1}}})
	}
	x.pcRows(rows...)
	r := x.runAs("quality", x.allQuery())
	// counted: 11111111, 22222222, 33333333, 55555555
	isNum(t, "hit", r.Metrics["recall.hit_rate"], 4, 4)
	stale := map[string]bool{}
	for _, row := range r.Scope.PCs.List {
		stale[row.Machine] = row.Stale
	}
	want := map[string]bool{"aaaaaaaa": false, "11111111": false, "22222222": true, "33333333": true, "55555555": false}
	if !reflect.DeepEqual(stale, want) {
		t.Errorf("listed (stale) %v, want %v", stale, want)
	}
}

// A server-only host has no part of its own; its figures on all PCs are the
// others' sums, on this PC no_data server_only_host.
func TestMergeOnAServerOnlyHost(t *testing.T) {
	x := newFx(t, fxNow)
	x.cfg = func(c *config.Config) { c.Server.Role = config.RoleServer }
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcDoc(pcOther2, "2026-09-05", fxNow.Add(-time.Hour), "")
	x.pcRows(
		pcRow{machine: pcOther1, day: "2026-10-28", f: machines.Figures{Prompts: 10, Hit: machines.Pair{4, 10}, Used: machines.Pair{3, 5}, Coverage: machines.Pair{5, 6}}},
		pcRow{machine: pcOther2, day: "2026-10-28", f: machines.Figures{Prompts: 20, Hit: machines.Pair{6, 20}, Used: machines.Pair{2, 5}, Coverage: machines.Pair{5, 6}}})
	this := x.runAs("quality", x.query(7))
	if m := this.Metrics["recall.hit_rate"]; m.Status != StatusNoData || m.Reason != ReasonServerOnlyHost {
		t.Fatalf("this: %s %q, want no_data server_only_host", m.Status, m.Reason)
	}
	r := x.runAs("quality", x.allQuery())
	isNum(t, "hit", r.Metrics["recall.hit_rate"], 10, 30)
	if r.Metrics["recall.hit_rate"].Status != StatusOK {
		t.Errorf("status %s %q, want ok", r.Metrics["recall.hit_rate"].Status, r.Metrics["recall.hit_rate"].Reason)
	}
	used := r.Metrics["used.turn_rate"]
	isNum(t, "used", used, 5, 10)
	if wc, _ := used.Extra["worst_case"].(*float64); wc == nil || !close4(*wc, 5.0/12) {
		t.Errorf("worst_case %v, want 5/12", used.Extra["worst_case"])
	}
	if r.Scope.PCs.Count != 2 {
		t.Errorf("pcs.count %d, want 2", r.Scope.PCs.Count)
	}
}

// Under a project filter this PC's figures are the project's and the other
// PC's are whole: 6/10 + 5/20 = 11/30, tagged; without the filter 8/30 +
// 5/20 = 13/50 and no tag.
func TestProjectFilterAddsOthersWhole(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("backsight", "bs", fxNow, 10, injecting(6))
	x.prompts("alpha", "al", fxNow.Add(-12*time.Hour), 20, injecting(2))
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcRows(pcRow{machine: pcOther1, day: "2026-10-28", f: machines.Figures{Prompts: 20, Hit: machines.Pair{5, 20}}})
	q := x.allQuery()
	q.Project = "backsight"
	r := x.runAs("quality", q)
	hit := r.Metrics["recall.hit_rate"]
	isNum(t, "hit", hit, 11, 30)
	if !slices.Contains(hit.Tags, tagOthersNotFiltered) || !r.Scope.OthersUnfiltered {
		t.Errorf("tags %v unfiltered %v, want others_not_filtered", hit.Tags, r.Scope.OthersUnfiltered)
	}
	all := x.mergedOf("quality", x.query(7))["recall.hit_rate"]
	isNum(t, "no filter", all, 13, 50)
	if slices.Contains(all.Tags, tagOthersNotFiltered) {
		t.Errorf("no filter, tags %v", all.Tags)
	}
}

// In a window drawn by the hour the totals add the days whose noon is in it
// but the series stay this PC's: 4 of 10 here, 6 of 9 there (Oct 29 12:00Z
// is in the last 24 h, Oct 30 12:00Z is after the window's end).
func TestHourlySeriesStayThisPC(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "h", fxNow, 10, injecting(4))
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcRows(
		pcRow{machine: pcOther1, day: "2026-10-29", f: machines.Figures{Prompts: 9, Hit: machines.Pair{6, 9}}},
		pcRow{machine: pcOther1, day: "2026-10-30", f: machines.Figures{Prompts: 50, Hit: machines.Pair{50, 50}}})
	m := x.mergedOf("quality", x.query(1))["recall.hit_rate"]
	isNum(t, "hit", m, 10, 19)
	var num, den int64
	for _, p := range m.Series {
		if p.Hour == "" {
			t.Fatalf("point %+v is not an hour", p)
		}
		num += *p.Num
		den += *p.Den
	}
	if num != 4 || den != 10 {
		t.Errorf("series sums %d/%d, want this PC's 4/10", num, den)
	}
	if !slices.Contains(m.Tags, tagSeriesThisPC) {
		t.Errorf("tags %v, want series_this_pc", m.Tags)
	}
	if this := x.runAs("quality", x.query(1)).Metrics["recall.hit_rate"]; slices.Contains(this.Tags, tagSeriesThisPC) {
		t.Errorf("this PC's tags %v", this.Tags)
	}
}

// The merged copy never writes the memo's maps: this PC's run, made before
// and after, is the same; the merged one differs and is memoized apart.
func TestMergeLeavesThisPCsRunAlone(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "cur", fxNow, 30, injecting(12))
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcRows(pcRow{machine: pcOther1, day: "2026-10-28", f: machines.Figures{Prompts: 70, Hit: machines.Pair{26, 70}}})
	q := x.allQuery()
	err := withTx(context.Background(), x.srcAs(), q, nil, func(e env) error {
		e.view = "quality"
		fam := families.byID["recall.hit_rate"]
		snap := func(e env) string {
			obs, met, err := e.runIDs(fam, q.Window, nil)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal([]any{obs, met})
			return string(b)
		}
		before := snap(e.this())
		merged := snap(e)
		if after := snap(e.this()); after != before {
			t.Errorf("this PC's run changed:\nbefore %s\nafter  %s", before, after)
		}
		if merged == before {
			t.Error("the merged run equals this PC's")
		}
		if snap(e) != merged {
			t.Error("the merged run changed on a second read")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// latDay is an other PC's day with the latency counts lat (and prompts
// typed).
func latRow(machine, day string, prompts int64, lat [9]int64) pcRow {
	return pcRow{machine: machine, day: day, f: machines.Figures{Prompts: prompts, Latency: lat}}
}

func edgeRange(t *testing.T, m Metric, key string) [2]*int64 {
	t.Helper()
	r, ok := m.Extra[key].(*[2]*int64)
	if !ok || r == nil {
		t.Fatalf("extra.%s = %#v, want a range", key, m.Extra[key])
	}
	return *r
}

func wantRange(t *testing.T, got [2]*int64, lo int64, hi *int64) {
	t.Helper()
	if got[0] == nil || *got[0] != lo || (hi == nil) != (got[1] == nil) || (hi != nil && *got[1] != *hi) {
		t.Errorf("range %v, want [%d, %v]", got, lo, hi)
	}
}

// This PC: 3 sessions of one 3 000-character prompt = 9 000 characters,
// below 100 sessions so its p95 is null. The other PC: 21 000 characters
// over 7 sessions. Average = (9 000 + 21 000) / (3 + 7) = 3 000, num
// 30 000, den 10; tokens 30 000 / 4 = 7 500.
func TestMergedContextIsTheAverage(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "c", fxNow, 3, func(i int, p *telemetry.Prompt) { p.Chars = 3000 })
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "2026-10-28")
	x.pcRows(pcRow{machine: pcOther1, day: "2026-10-28", f: machines.Figures{Prompts: 7, Context: machines.Pair{21000, 7}}})

	m := x.mergedOf("cost", x.query(7))["context.per_session"]
	if m.Value == nil || !close4(*m.Value, 3000) {
		t.Fatalf("value %v, want 3000", m.Value)
	}
	if m.Num == nil || *m.Num != 30000 || m.Den == nil || *m.Den != 10 {
		t.Errorf("num/den %v/%v, want 30000/10", num0(m.Num), num0(m.Den))
	}
	if p95, _ := m.Extra["p95"].(*float64); p95 != nil {
		t.Errorf("extra.p95 %v, want this PC's null", *p95)
	}
	if avg, _ := m.Extra["average"].(*float64); avg == nil || !close4(*avg, 3000) {
		t.Errorf("extra.average %v, want 3000", m.Extra["average"])
	}
	if m.Status != StatusTooFew {
		t.Errorf("status %s, want this PC's too_few", m.Status)
	}
	for _, tag := range []string{tagAverage, tagStatusThisPC} {
		if !slices.Contains(m.Tags, tag) {
			t.Errorf("tags %v lack %s", m.Tags, tag)
		}
	}
	if h, _ := m.Extra["histogram"].([]histBucket); len(h) == 0 || h[3].N != 3 {
		t.Errorf("histogram %v, want this PC's (3 sessions in 3 000-3 999)", m.Extra["histogram"])
	}

	s := x.runAs("overview", x.allQuery()).Summary
	if s.ContextChars != 30000 || s.TokensApprox != 7500 {
		t.Errorf("all PCs summary %d chars %d tokens, want 30000 and 7500", s.ContextChars, s.TokensApprox)
	}
	s = x.runAs("overview", x.query(7)).Summary
	if s.ContextChars != 9000 || s.TokensApprox != 2250 {
		t.Errorf("this PC summary %d chars %d tokens, want 9000 and 2250", s.ContextChars, s.TokensApprox)
	}
	this := x.runAs("cost", x.query(7)).Metrics["context.per_session"]
	if this.Num != nil || slices.Contains(this.Tags, tagAverage) || this.Extra["average"] != nil {
		t.Errorf("this PC's metric carries the merge: %+v", this)
	}
}

// Other PC day 1 (Oct 20, before the 7 days) 6 000 chars in 1 session, day 2
// (Oct 27) 6 000 in 0: the window holds day 2 only, so (9 000 + 6 000) / 3 =
// 5 000, the bias R7 names.
func TestContextAverageWithASpanningSession(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "c", fxNow, 3, func(i int, p *telemetry.Prompt) { p.Chars = 3000 })
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "2026-10-27")
	x.pcRows(
		pcRow{machine: pcOther1, day: "2026-10-20", f: machines.Figures{Prompts: 1, Context: machines.Pair{6000, 1}}},
		pcRow{machine: pcOther1, day: "2026-10-27", f: machines.Figures{Prompts: 1, Context: machines.Pair{6000, 0}}})
	m := x.mergedOf("cost", x.query(7))["context.per_session"]
	if m.Value == nil || !close4(*m.Value, 5000) || num0(m.Num) != 15000 || num0(m.Den) != 3 {
		t.Errorf("value %v num/den %d/%d, want 5000 and 15000/3", m.Value, num0(m.Num), num0(m.Den))
	}
}

// This PC: 50 prompts of 800 ms (bucket 4, (500, 1000]) and 10 of 1 800 ms
// (bucket 5, (1000, 2000]) = [0,0,0,0,50,10,0,0,0]. The other PC
// [0,0,0,0,30,8,2,0,0]. Merged [0,0,0,0,80,18,2,0,0], n 100: rank 95 is the
// 95th, 80 < 95 <= 98 so bucket 5; rank 50 is in bucket 4.
func TestMergedLatencyFromBuckets(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "l", fxNow, 60, func(i int, p *telemetry.Prompt) {
		p.MS = 800
		if i >= 50 {
			p.MS = 1800
		}
	})
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "2026-10-28")
	x.pcRows(latRow(pcOther1, "2026-10-28", 40, [9]int64{0, 0, 0, 0, 30, 8, 2, 0, 0}))

	m := x.mergedOf("cost", x.query(7))["recall.latency"]
	if m.Value == nil || *m.Value != 2000 || num0(m.Den) != 100 {
		t.Fatalf("value %v den %d, want 2000 and 100", m.Value, num0(m.Den))
	}
	two, one := int64(2000), int64(1000)
	wantRange(t, edgeRange(t, m, "range_ms"), 1000, &two)
	wantRange(t, edgeRange(t, m, "p50_range_ms"), 500, &one)
	if b, _ := m.Extra["buckets"].([9]int64); b != [9]int64{0, 0, 0, 0, 80, 18, 2, 0, 0} {
		t.Errorf("buckets %v", m.Extra["buckets"])
	}
	if p95, _ := m.Extra["p95"].(*float64); p95 != nil { // 60 samples: below 100
		t.Errorf("extra.p95 %v, want this PC's null (60 samples)", *p95)
	}
	if p50, _ := m.Extra["p50"].(*float64); p50 == nil || *p50 != 800 {
		t.Errorf("extra.p50 %v, want this PC's exact 800", m.Extra["p50"])
	}
	if !slices.Contains(m.Tags, tagApproximate) || m.Status != StatusOK {
		t.Errorf("status %s tags %v, want ok and approximate", m.Status, m.Tags)
	}
	this := x.runAs("cost", x.query(7)).Metrics["recall.latency"]
	if this.Extra["range_ms"] != nil || slices.Contains(this.Tags, tagApproximate) {
		t.Errorf("this PC's latency carries the merge: %+v", this)
	}
}

// The other PC alone fills the buckets (this PC has no prompt): 100 samples
// in bucket 6 (2000, 2500] -> p95 edge 2500, not above 2500: watch; bucket 7
// (2500, 5000] -> bad; the open bucket -> bad with [5000, null] and value
// 5000; 99 samples -> too few. A this-PC sample of exactly 2000 ms lands in
// (1000, 2000].
func TestLatencyStatusIsExactAtTheEdges(t *testing.T) {
	five, twoFive := int64(5000), int64(2500)
	for _, c := range []struct {
		name   string
		lat    [9]int64
		status Status
		value  float64
		lo     int64
		hi     *int64
	}{
		{"2000-2500 watches", [9]int64{6: 100}, StatusWatch, 2500, 2000, &twoFive},
		{"2500-5000 is bad", [9]int64{7: 100}, StatusBad, 5000, 2500, &five},
		{"open is bad", [9]int64{8: 100}, StatusBad, 5000, 5000, nil},
		{"1000-2000 is ok", [9]int64{5: 100}, StatusOK, 2000, 1000, ptr(int64(2000))},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newFx(t, fxNow)
			x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
			x.pcRows(latRow(pcOther1, "2026-10-28", 100, c.lat))
			m := x.mergedOf("cost", x.query(7))["recall.latency"]
			if m.Status != c.status || m.Value == nil || *m.Value != c.value {
				t.Fatalf("status %s value %v, want %s %v", m.Status, m.Value, c.status, c.value)
			}
			wantRange(t, edgeRange(t, m, "range_ms"), c.lo, c.hi)
		})
	}
	t.Run("99 samples are too few", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
		x.pcRows(latRow(pcOther1, "2026-10-28", 99, [9]int64{7: 99}))
		m := x.mergedOf("cost", x.query(7))["recall.latency"]
		if m.Status != StatusTooFew || m.Value != nil {
			t.Errorf("status %s value %v, want too_few and null", m.Status, m.Value)
		}
	})
	t.Run("a sample of exactly 2000 ms is in (1000, 2000]", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.prompts("alpha", "e", fxNow, 100, func(i int, p *telemetry.Prompt) { p.MS = 2000 })
		x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
		x.pcRows(latRow(pcOther1, "2026-10-28", 1, [9]int64{}))
		m := x.mergedOf("cost", x.query(7))["recall.latency"]
		if b, _ := m.Extra["buckets"].([9]int64); b != [9]int64{5: 100} || m.Status != StatusOK || *m.Value != 2000 {
			t.Errorf("buckets %v status %s value %v, want [0,0,0,0,0,100,0,0,0] ok 2000", m.Extra["buckets"], m.Status, m.Value)
		}
	})
	t.Run("2001 ms is in (2000, 2500]", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.prompts("alpha", "e", fxNow, 100, func(i int, p *telemetry.Prompt) { p.MS = 2001 })
		x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
		x.pcRows(latRow(pcOther1, "2026-10-28", 1, [9]int64{}))
		m := x.mergedOf("cost", x.query(7))["recall.latency"]
		if m.Status != StatusWatch || *m.Value != 2500 {
			t.Errorf("status %s value %v, want watch 2500", m.Status, m.Value)
		}
	})
}

// Oct 29: this PC 800 and 1 800 ms (buckets 4 and 5), the other PC one in
// bucket 6: merged [0,0,0,0,1,1,1,0,0], n 3, rank ceil(2.85) = 3 -> bucket 6
// (2000, 2500]; this PC's exact p95 of the day is 1 800 (rank 2 of 2).
// Oct 27: the other PC alone, 2 in the open bucket: n 2, lo 5000, no hi.
// Oct 28: no sample on any PC.
func TestMergedLatencySeries(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "s", fxNow.Add(-24*time.Hour), 2, func(i int, p *telemetry.Prompt) { p.MS = 800 + 1000*i })
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "2026-10-29")
	x.pcRows(latRow(pcOther1, "2026-10-29", 1, [9]int64{6: 1}), latRow(pcOther1, "2026-10-27", 2, [9]int64{8: 2}))
	m := x.mergedOf("cost", x.query(7))["recall.latency"]
	byDay := map[string]Point{}
	for _, p := range m.Series {
		byDay[p.Day] = p
	}
	p := byDay["2026-10-29"]
	if num0(p.N) != 3 || p.Lo == nil || *p.Lo != 2000 || p.Hi == nil || *p.Hi != 2500 || p.ThisP95 == nil || *p.ThisP95 != 1800 {
		t.Errorf("Oct 29 = %+v, want n 3, (2000, 2500], this_p95 1800", p)
	}
	if p.P50 != nil || p.P95 != nil {
		t.Errorf("Oct 29 keeps p50/p95: %+v", p)
	}
	p = byDay["2026-10-27"]
	if num0(p.N) != 2 || p.Lo == nil || *p.Lo != 5000 || p.Hi != nil || p.ThisP95 != nil {
		t.Errorf("Oct 27 = %+v, want n 2, lo 5000, no hi, no this_p95", p)
	}
	p = byDay["2026-10-28"]
	if num0(p.N) != 0 || p.Lo != nil || p.Hi != nil {
		t.Errorf("Oct 28 = %+v, want n 0 and no lo/hi", p)
	}
	raw, _ := json.Marshal(byDay["2026-10-29"])
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	for _, k := range []string{"p50", "p95"} {
		if _, ok := keys[k]; ok {
			t.Errorf("point JSON has %q: %s", k, raw)
		}
	}
	for _, k := range []string{"lo", "hi", "this_p95"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("point JSON lacks %q: %s", k, raw)
		}
	}
}

// This PC alone: 100 prompts, 95 of 3 000 ms and 5 of 1 000 ms -> p95 3 000
// (bad); the last 24 h hold 30 prompts of 1 000 ms -> recovered, read watch
// "improving". Merged with an idle PC (a day with only an empty latency) the
// observation has no Recent, so it reads bad, no improving tag.
func TestMergedLatencySkipsRecovery(t *testing.T) {
	x := newFx(t, fxNow)
	// 30 newest (inside the last 24 h): 1 000 ms; the 70 older: 3 000 ms
	// except 5 of 1 000 ms, so 35 of 100 at 1 000 and 65 at 3 000: the 95th
	// of 100 is 3 000.
	x.prompts("alpha", "r", fxNow, 100, func(i int, p *telemetry.Prompt) {
		p.MS = 3000
		if i < 30 || (i >= 30 && i < 35) {
			p.MS = 1000
		}
	})
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcRows(latRow(pcOther1, "2026-10-28", 1, [9]int64{}))
	this := x.runAs("cost", x.query(7)).Metrics["recall.latency"]
	if this.Status != StatusWatch || !slices.Contains(this.Tags, TagImproving) {
		t.Fatalf("this PC: status %s tags %v, want watch improving", this.Status, this.Tags)
	}
	m := x.mergedOf("cost", x.query(7))["recall.latency"]
	if m.Status != StatusBad || slices.Contains(m.Tags, TagImproving) {
		t.Errorf("merged: status %s tags %v, want bad without improving", m.Status, m.Tags)
	}
}

// Cost view on all PCs. This PC: 100 prompts of 800 ms (p50 = p95 = 800).
// bbbbbbbb: 100 prompts [0,0,0,0,60,38,2,0,0] -> rank 95 in bucket 5
// (1000, 2000], rank 50 in bucket 4 (500, 1000]. cccccccc: counted, no day in
// the window. dddddddd: stale, 5 prompts in bucket 2: below 20 samples, so
// no range.
func TestByPCRows(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "b", fxNow, 100, func(i int, p *telemetry.Prompt) { p.MS = 800 })
	d := "dddddddd-0000-4000-8000-000000000004"
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "2026-10-29")
	x.pcDoc(pcOther2, "2026-09-12", fxNow.Add(-time.Hour), "2026-10-20")
	x.pcDoc(d, "2026-09-15", fxNow.Add(-72*time.Hour), "2026-10-27")
	x.pcRows(latRow(pcOther1, "2026-10-29", 100, [9]int64{4: 60, 5: 38, 6: 2}), latRow(d, "2026-10-27", 5, [9]int64{2: 5}))

	rows := x.runAs("cost", x.allQuery()).Tables.ByPC
	if len(rows) != 4 {
		t.Fatalf("rows %+v, want 4", rows)
	}
	t0 := rows[0]
	if !t0.This || t0.Machine != "aaaaaaaa" || t0.Prompts != 100 || !t0.Active || t0.P95 == nil || *t0.P95 != 800 || t0.P50 == nil || *t0.P50 != 800 ||
		t0.P95RangeMS != nil || t0.P50RangeMS != nil || t0.Stale || t0.Until != nil {
		t.Errorf("this PC row %+v", t0)
	}
	one, two := int64(1000), int64(2000)
	o1 := rows[1]
	if o1.This || o1.Machine != "bbbbbbbb" || o1.Prompts != 100 || !o1.Active || o1.P95 != nil || o1.P50 != nil || o1.Stale || o1.Until == nil || *o1.Until != "2026-10-29" {
		t.Errorf("bbbbbbbb row %+v", o1)
	}
	if o1.P95RangeMS == nil || o1.P50RangeMS == nil {
		t.Fatalf("bbbbbbbb ranges %v %v", o1.P95RangeMS, o1.P50RangeMS)
	}
	wantRange(t, *o1.P95RangeMS, 1000, &two)
	wantRange(t, *o1.P50RangeMS, 500, &one)
	if o2 := rows[2]; o2.Machine != "cccccccc" || o2.Active || o2.Prompts != 0 || o2.P95RangeMS != nil || o2.Stale {
		t.Errorf("cccccccc row %+v, want idle", o2)
	}
	if o3 := rows[3]; o3.Machine != "dddddddd" || !o3.Stale || !o3.Active || o3.Prompts != 5 || o3.P95RangeMS != nil || o3.Until == nil || *o3.Until != "2026-10-27" {
		t.Errorf("dddddddd row %+v, want stale and active with until", o3)
	}

	if r := x.runAs("cost", x.query(7)); r.Tables.ByPC != nil {
		t.Errorf("this PC: by_pc %+v, want absent", r.Tables.ByPC)
	}
	if r := x.runAs("quality", x.allQuery()); r.Tables.ByPC != nil {
		t.Errorf("quality view: by_pc %+v, want absent", r.Tables.ByPC)
	}
	raw, _ := json.Marshal(x.runAs("cost", x.query(7)).Tables)
	if strings.Contains(string(raw), "by_pc") {
		t.Errorf("this PC's tables JSON has by_pc: %s", raw)
	}
}

// The merged cost run never writes the memo's maps: this PC's run is the
// same before and after, the merged one differs and is stable.
func TestMergeCostLeavesThisPCsRunAlone(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "m", fxNow, 100, func(i int, p *telemetry.Prompt) { p.MS, p.Chars = 800, 100 })
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcRows(pcRow{machine: pcOther1, day: "2026-10-28", f: machines.Figures{Prompts: 5, Context: machines.Pair{500, 5}, Latency: [9]int64{8: 5}}})
	q := x.allQuery()
	err := withTx(context.Background(), x.srcAs(), q, nil, func(e env) error {
		e.view = "cost"
		fam := families.byID["recall.latency"]
		snap := func(e env) string {
			obs, met, err := e.runIDs(fam, q.Window, nil)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal([]any{obs, met})
			return string(b)
		}
		before := snap(e.this())
		merged := snap(e)
		if after := snap(e.this()); after != before {
			t.Errorf("this PC's run changed:\nbefore %s\nafter  %s", before, after)
		}
		if merged == before {
			t.Error("the merged run equals this PC's")
		}
		if snap(e) != merged {
			t.Error("the merged run changed on a second read")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Under a project filter this PC's By PC row counts the project's typed
// prompts only (A8: this PC filtered, the others whole). This PC: 4 prompts
// of "alpha" and 3 of "beta"; filter beta -> 3, filter gamma -> 0 and not
// active. The other PC keeps its whole 40 prompts either way.
func TestByPCRowFollowsTheProjectFilter(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "a", fxNow, 4, nil)
	x.prompts("beta", "b", fxNow, 3, nil)
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "2026-10-29")
	x.pcRows(latRow(pcOther1, "2026-10-29", 40, [9]int64{}))
	for _, c := range []struct {
		project string
		prompts int64
		active  bool
	}{{"beta", 3, true}, {"gamma", 0, false}, {"", 7, true}} {
		q := x.allQuery()
		q.Project = c.project
		rows := x.runAs("cost", q).Tables.ByPC
		if len(rows) != 2 {
			t.Fatalf("project %q: rows %+v", c.project, rows)
		}
		if rows[0].Prompts != c.prompts || rows[0].Active != c.active {
			t.Errorf("project %q: this PC prompts %d active %v, want %d %v", c.project, rows[0].Prompts, rows[0].Active, c.prompts, c.active)
		}
		if rows[1].Prompts != 40 || !rows[1].Active {
			t.Errorf("project %q: other PC %+v, want whole 40 and active", c.project, rows[1])
		}
	}
}
