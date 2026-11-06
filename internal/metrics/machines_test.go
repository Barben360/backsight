package metrics

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
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
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	for _, view := range []string{"projects", "sessions", "memories"} {
		q := x.allQuery()
		if r := x.runAs(view, q); r.Scope.Machines != "this" || r.Scope.OthersUnfiltered || r.Scope.PCs.Count != 2 {
			t.Errorf("%s list: scope %+v pcs %+v", view, r.Scope, r.Scope.PCs)
		}
		q.ID = "alpha"
		if view == "sessions" {
			q.ID = "p-0"
		}
		if view == "memories" {
			q.ID = "m1"
		}
		q.Project = "alpha"
		r, err := Run(context.Background(), view, x.srcAs(), q)
		if err == nil && (r.Scope.Machines != "this" || r.Scope.OthersUnfiltered) {
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
	if got := othersAt(e, e.q.Window); len(got) != 0 {
		t.Errorf("othersAt = %v: Task 8 fills it", got)
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
