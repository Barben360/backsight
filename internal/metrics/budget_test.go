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
	"github.com/Barben360/backsight/internal/paths"
)

// viewLimit is the dashboard's page budget (300 ms p95, spec §7) with a
// margin of 3 for a slower runner, as block 1's index budgets.
const viewLimit = 900 * time.Millisecond

// TestViewBudgets times the page views on block 1's generated 13 months
// (indexgen.Budget), the default 7-day window, a server that answers at
// once and an instant doctor: each view, best of 3 runs, within viewLimit.
// The overview and the quality view are the budget's; the others are
// timed and held to it too.
func TestViewBudgets(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("time budgets are measured without -race and -short")
	}
	home := t.TempDir()
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

	for _, view := range Views[:6] {
		best := time.Duration(1<<63 - 1)
		var r Result
		for range 3 { // best of 3: a slow shared runner's one-off stall is not the view's cost
			start := time.Now()
			r, err = Run(ctx, view, src, q)
			best = min(best, time.Since(start))
			if err != nil {
				t.Fatalf("%s: %v", view, err)
			}
		}
		// the measure is of real work: the generated activity was read
		if hit := r.Metrics["recall.hit_rate"]; view == "overview" && (hit.Den == nil || *hit.Den < 1000) {
			t.Errorf("overview: recall.hit_rate den %v, want the generated week's recalls (> 1,000)", hit.Den)
		}
		t.Logf("%s: best of 3 %v (limit %v)", view, best, viewLimit)
		if best > viewLimit {
			t.Errorf("%s took %v, limit %v", view, best, viewLimit)
		}
	}
}
