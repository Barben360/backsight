package metrics

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/machines"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/testtool"
)

// baseViewLimit is the dashboard's page budget (300 ms p95, spec §7) with
// a margin of 3 for a slower runner, as block 1's index budgets.
const baseViewLimit = 900 * time.Millisecond

// viewLimit is baseViewLimit, doubled on GitHub's Windows runner, as the
// dashboard's pageLimit: there the slowest view (projects <name>) took
// 901 ms and the overview 674 ms, where the Windows test VM's slowest
// takes 494 ms and its overview 423 ms, so the runner, not Windows, is
// slow; the VM and every other host keep the base limit.
var viewLimit = func() time.Duration {
	if runtime.GOOS == "windows" && os.Getenv("GITHUB_ACTIONS") == "true" {
		return 2 * baseViewLimit
	}
	return baseViewLimit
}()

// TestViewBudgets times the page views on block 1's generated 13 months
// (indexgen.Budget), the default 7-day window, a server that answers at
// once and an instant doctor: each view, best of 3 runs, within viewLimit.
// The overview and the quality view are the budget's; the others, the
// browse views (their lists and a page of each) included, are timed and held
// to it too.
func TestViewBudgets(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("time budgets are measured without -race and -short")
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

	// timed runs view for query and returns its best of 3 and its result.
	timed := func(name, view string, q Query) Result {
		best := time.Duration(1<<63 - 1)
		var r Result
		for range 3 { // best of 3: a slow shared runner's one-off stall is not the view's cost
			start := time.Now()
			r, err = Run(ctx, view, src, q)
			best = min(best, time.Since(start))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		t.Logf("%s: best of 3 %v (limit %v)", name, best, viewLimit)
		if best > viewLimit {
			t.Errorf("%s took %v, limit %v", name, best, viewLimit)
		}
		return r
	}
	for _, view := range Views[:6] {
		r := timed(view, view, q)
		if view == "overview" { // the block's reads: time to recall (the sessions' way) and Helped most
			f, _ := r.Metrics["pipeline.time_to_recall"].Extra["funnel"].(map[string]int64)
			if f["ended"] < 100 {
				t.Errorf("overview: pipeline.time_to_recall funnel %v, want the week's ended sessions (>= 100)", f)
			}
			if len(r.Tables.Helped) != 3 {
				t.Errorf("overview: %d memories that helped most, want 3", len(r.Tables.Helped))
			}
		}
		// the measure is of real work: each view read the generated week
		if c := realWork[view]; !c.ok(r) {
			t.Errorf("%s: %s = %+v, want %s", view, c.id, r.Metrics[c.id], c.what)
		}
	}

	// The browse views: the lists, then a page of the first row of each.
	projects := timed("projects", "projects", q).Tables
	sessions := timed("sessions", "sessions", q).Tables
	memories := timed("memories", "memories", q).Tables
	days := timed("days", "days", q).Tables
	releases := timed("releases", "releases", q).Tables
	defs := timed("defs", "defs", q).Tables
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
	if c := timed("releases --compare", "releases", cq).Tables.Compare; c == nil {
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
		timed(p.name, p.view, pq)
	}
}

// realWork is, per view, a figure only the generated history can give: the
// view's time is that of real reads, not of an empty index.
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

// TestDailyBudget computes this PC's daily figures on block 1's generated 13 months (indexgen.Budget), best
// of 3 within 15 s, and holds the marshalled document under the write cap without Fit dropping anything.
func TestDailyBudget(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("time budgets are measured without -race and -short")
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

	const limit = 15 * time.Second
	best := time.Duration(1<<63 - 1)
	var days []machines.Day
	for range 3 {
		start := time.Now()
		days, err = Daily(ctx, src, indexgen.Now)
		best = min(best, time.Since(start))
		if err != nil {
			t.Fatal(err)
		}
	}
	doc := machines.Doc{Machine: "3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6", At: indexgen.Now, OS: "linux", FirstSeen: "2025-01-01",
		BinaryVersion: "v0.1.30", Until: indexgen.Now.UTC().AddDate(0, 0, -1).Format("2006-01-02"), Days: days}
	body, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("daily figures: best of 3 %v (limit %v), %d days, document %d bytes (write cap %d)", best, limit, len(days), len(body), machines.WriteMax)
	if best > limit {
		t.Errorf("Daily took %v, limit %v", best, limit)
	}
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
