package metrics

import (
	"context"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/machines"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/testtool"
)

// heavyHistory is why the tests on indexgen.Budget skip: its first build
// takes minutes under -race. make test-sweep runs them without it, and
// test-windows.
const heavyHistory = "13 months of a heavy PC: not under -race or -short"

// TestViewsOnAGeneratedHistory runs the page views on 13 months of a
// heavy PC's generated history (indexgen.Budget, with 8 other PCs), the
// default 7-day window, a server that answers at once and an instant
// doctor: each view reads the generated week (a figure only that history
// gives), the views with machines=all merge the other PCs, and the browse
// views list and page it. What they take is not a test's: a loaded
// machine would fail any time limit.
func TestViewsOnAGeneratedHistory(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip(heavyHistory)
	}
	home := testtool.BenchTempDir(t)
	t.Setenv("BACKSIGHT_HOME", home)
	t.Setenv("BACKSIGHT_STATE_DIR", "")
	dirs, err := paths.Resolve(runtime.GOOS, home, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	spec := indexgen.Budget
	spec.OtherPCs = 8 // the other PCs' cache the views with machines=all merge
	indexgen.Generate(t, dirs.State, spec)
	ctx := context.Background()
	ix, err := index.Open(ctx, index.Options{Dirs: dirs, Home: home, Getenv: func(string) string { return "" },
		Now:      func() time.Time { return indexgen.Now },
		Machines: func() (machines.Cache, error) { return machines.LoadCache(dirs.MetricsDir()) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	fr, err := ix.CatchUp(ctx, time.Hour)
	if err != nil || fr.Partial {
		t.Fatalf("catch-up: %v, partial %v", err, fr.Partial)
	}
	cfg := config.Default(dirs, home)
	cfg.Feedback.Enabled = true
	srv := newFakeServer(t)
	extractionServer(&fx{server: srv}) // the cost view's LLM stats (it only sets the fake's answers)
	checks := []DoctorCheck{{ID: "server", Status: "ok"}, {ID: "update", Status: "warn"}}
	src := Source{Index: ix, Fresh: fr, Now: func() time.Time { return indexgen.Now }, Config: cfg, Configured: true,
		Server: srv, ServerHost: fxHost, Doctor: func(context.Context) ([]DoctorCheck, error) { return checks, nil }}
	w, err := NewWindow(indexgen.Now, 7, time.Time{}, time.Time{}, fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	q := Query{Window: w, Machines: "this"}
	thisOnly := map[string]Result{}

	// runView runs view for query and returns its result.
	runView := func(name, view string, q Query) Result {
		r, err := Run(ctx, view, src, q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return r
	}
	for _, view := range Views[:6] {
		r := runView(view, view, q)
		thisOnly[view] = r
		if view == "overview" { // the block's reads: time to recall (the sessions' way) and Helped most
			f, _ := r.Metrics["pipeline.time_to_recall"].Extra["funnel"].(map[string]int64)
			if f["ended"] < 100 {
				t.Errorf("overview: pipeline.time_to_recall funnel %v, want the week's ended sessions (>= 100)", f)
			}
			if len(r.Tables.Helped) != 3 {
				t.Errorf("overview: %d memories that helped most, want 3", len(r.Tables.Helped))
			}
		}
		// each view read the generated week
		if c := realWork[view]; !c.ok(r) {
			t.Errorf("%s: %s = %+v, want %s", view, c.id, r.Metrics[c.id], c.what)
		}
	}

	// The views that merge the other PCs, with all of them: 8 other PCs over
	// the 13 months, and each read what it merges.
	aq := q
	aq.Machines = "all"
	for _, view := range []string{"overview", "quality", "cost", "days", "releases"} {
		r := runView(view+" --machines all", view, aq)
		if p := r.Scope.PCs; r.Scope.Machines != "all" || p == nil || p.Count != 9 || p.Stale != 1 {
			t.Errorf("%s --machines all: scope %+v, pcs %+v, want 9 PCs, one of them stale", view, r.Scope, p)
		}
		switch view {
		case "overview", "quality":
			a, b := r.Metrics["recall.hit_rate"].Den, thisOnly[view].Metrics["recall.hit_rate"].Den
			if view == "quality" {
				a, b = r.Metrics["used.turn_rate"].Den, thisOnly[view].Metrics["used.turn_rate"].Den
			}
			if a == nil || b == nil || *a <= *b {
				t.Errorf("%s --machines all: n %v, this PC's %v, want the other PCs' added", view, a, b)
			}
		case "cost":
			if m := r.Metrics["recall.latency"]; !slices.Contains(m.Tags, "approximate") {
				t.Errorf("cost --machines all: latency tags %v, want approximate (merged from the buckets)", m.Tags)
			}
		case "days":
			merged := int64(0)
			for _, c := range r.Tables.Calendar {
				merged += c.Prompts
			}
			ra, err := Run(ctx, "days", src, q)
			if err != nil {
				t.Fatal(err)
			}
			alone := int64(0)
			for _, c := range ra.Tables.Calendar {
				alone += c.Prompts
			}
			if merged <= alone {
				t.Errorf("days --machines all: %d prompts in the calendar, this PC's %d, want the other PCs' added", merged, alone)
			}
		}
	}

	// The browse views: the lists, then a page of the first row of each.
	projects := runView("projects", "projects", q).Tables
	sessions := runView("sessions", "sessions", q).Tables
	memories := runView("memories", "memories", q).Tables
	days := runView("days", "days", q).Tables
	releases := runView("releases", "releases", q).Tables
	defs := runView("defs", "defs", q).Tables
	if len(projects.Projects) == 0 || projects.Pooled.Totals.Prompts < 1000 || len(sessions.Sessions) != q.EffectiveLimit() || sessions.Page.Total < 100 ||
		len(memories.Memories) != q.EffectiveLimit() || memories.Page.Total < 100 || len(days.Days) != 8 || len(releases.Releases) == 0 || len(defs.Defs) != len(Catalog) {
		t.Errorf("the browse views did not read the generated week: %d projects (%d prompts), %d of %d sessions, %d of %d memories, %d days, %d releases, %d defs",
			len(projects.Projects), projects.Pooled.Totals.Prompts, len(sessions.Sessions), sessions.Page.Total, len(memories.Memories), memories.Page.Total,
			len(days.Days), len(releases.Releases), len(defs.Defs))
		return
	}
	// the days view reads the calendar of 13 months, and the releases view
	// compares two versions on the week's turns
	if len(days.Calendar) < 390 {
		t.Errorf("days: %d days in the calendar, want 13 months", len(days.Calendar))
	}
	recorded := 0
	for _, c := range days.Calendar {
		if c.Recorded {
			recorded++
		}
	}
	if recorded < 300 {
		t.Errorf("days: %d days recorded in the calendar, want the generated history", recorded)
	}
	cq := q
	cq.Compare = [2]string{"v0.1.16", "v0.1.17"}
	if len(releases.Releases) > 1 {
		cq.Compare = [2]string{releases.Releases[1].Version, releases.Releases[0].Version}
	}
	if c := runView("releases --compare", "releases", cq).Tables.Compare; c == nil {
		t.Errorf("releases --compare: no comparison")
	}
	for _, p := range []struct {
		name, view, id string
	}{
		{"projects <name>", "projects", projects.Projects[0].Project},
		{"sessions <id>", "sessions", sessions.Sessions[0].ID},
		{"memories <id>", "memories", memories.Memories[0].ID},
		{"days <date>", "days", days.Days[0].Day},
	} {
		pq := q
		pq.ID = p.id
		runView(p.name, p.view, pq)
	}
}

// realWork is, per view, a figure only the generated history can give: the
// view read it, not an empty index.
var realWork = map[string]workCheck{
	"overview": {"recall.hit_rate", "the week's recalls (n > 1,000)", 1000},
	"quality":  {"declare.coverage", "the week's turns that showed memory (n > 100)", 100},
	"cost":     {"recall.latency", "the week's recall times (n > 1,000)", 1000},
	"bank":     {"stale.never_used", "the ledger's never-used memories (> 100)", 100},
	"pipeline": {"pipeline.flow", "the week's sent documents (n > 100)", 100},
	"system":   {"jobs.last_run", "the last tick's age (a value)", 0},
}

// workCheck is one view's figure: its n (den), else its value, at least min.
type workCheck struct {
	id, what string
	min      float64
}

func (c workCheck) ok(r Result) bool {
	m := r.Metrics[c.id]
	if m.Status == StatusNoData {
		return false
	}
	if m.Den != nil {
		return float64(*m.Den) >= c.min
	}
	return m.Value != nil && *m.Value >= c.min
}

// TestDailyFitsTheWriteCap computes this PC's daily figures on 13 months
// of a heavy PC's generated history (indexgen.Budget), and holds the
// marshalled document under the write cap without Fit dropping anything.
func TestDailyFitsTheWriteCap(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip(heavyHistory)
	}
	home := testtool.BenchTempDir(t)
	t.Setenv("BACKSIGHT_HOME", home)
	t.Setenv("BACKSIGHT_STATE_DIR", "")
	dirs, err := paths.Resolve(runtime.GOOS, home, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	indexgen.Generate(t, dirs.State, indexgen.Budget)
	ctx := context.Background()
	ix, err := index.Open(ctx, index.Options{Dirs: dirs, Home: home, Getenv: func(string) string { return "" },
		Now: func() time.Time { return indexgen.Now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	fr, err := ix.CatchUp(ctx, time.Hour)
	if err != nil || fr.Partial {
		t.Fatalf("catch-up: %v, partial %v", err, fr.Partial)
	}
	cfg := config.Default(dirs, home)
	cfg.Feedback.Enabled = true
	src := Source{Index: ix, Fresh: fr, Now: func() time.Time { return indexgen.Now }, Config: cfg, Configured: true}

	days, err := Daily(ctx, src, indexgen.Now)
	if err != nil {
		t.Fatal(err)
	}
	doc := machines.Doc{Machine: "3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6", At: indexgen.Now, OS: "linux", FirstSeen: "2025-01-01",
		BinaryVersion: "v0.1.30", Until: indexgen.Now.UTC().AddDate(0, 0, -1).Format("2006-01-02"), Days: days}
	body, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := machines.Parse(machines.DocPrefix+doc.Machine, body, indexgen.Now); err != nil {
		t.Errorf("the published document does not parse: %v", err)
	}
	t.Logf("daily figures: %d days, document %d bytes (write cap %d)", len(days), len(body), machines.WriteMax)
	if len(days) < 300 {
		t.Errorf("%d days, want the generated history (>= 300)", len(days))
	}
	if len(body) >= machines.WriteMax {
		t.Errorf("document is %d bytes, want under %d", len(body), machines.WriteMax)
	}
	if _, dropped, err := machines.Fit(doc, machines.WriteMax); err != nil || dropped != (machines.Dropped{}) {
		t.Errorf("Fit dropped %+v (err %v), want nothing", dropped, err)
	}
}
