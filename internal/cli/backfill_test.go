package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

// oldSession writes a transcript with one turn and, optionally, an
// explicit save made through the reference plugin.
func oldSession(t *testing.T, home, sid string, at time.Time, withSave bool) {
	t.Helper()
	p := filepath.Join(home, ".claude", "projects", "-home-u-billing", sid+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	ts := at.UTC().Format(time.RFC3339)
	lines := `{"type":"user","uuid":"` + sid + `-u1","sessionId":"` + sid + `","cwd":"` + filepath.Join(home, "billing") + `","timestamp":"` + ts + `","message":{"role":"user","content":"We keep PostgreSQL 16 for billing."}}` + "\n"
	if withSave {
		lines += `{"type":"assistant","uuid":"` + sid + `-a1","sessionId":"` + sid + `","cwd":"` + filepath.Join(home, "billing") + `","timestamp":"` + ts + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__hindsight__hindsight_ingest_document","input":{"title":"Décision : PostgreSQL 16","content":"Index partiels requis."}}]}}` + "\n"
	}
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fastBackfill(t *testing.T) {
	t.Helper()
	old := backfillPoll
	backfillPoll = time.Millisecond
	t.Cleanup(func() { backfillPoll = old; llmInput.Store(0) })
}

func TestBackfillPlanSendsNothing(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), true)
	before := retained.Load()
	code, out, _ := run(t, "retain", "backfill", "--plan", "--json")
	var env struct {
		Data backfillResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	p := env.Data.Plan
	if code != output.OK || p.Send != 1 || p.Notes != 1 || p.Estimate.Cost <= 0 || p.MaxCost < 1.49*p.Estimate.Cost || p.Currency != "USD" || retained.Load() != before {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestBackfillSendsOldestFirstWithNotesThenNothing(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	oldSession(t, home, "newer", time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), false)
	oldSession(t, home, "older", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), true)
	before := retained.Load()
	code, out, errOut := run(t, "retain", "backfill", "--json")
	var env struct {
		Data backfillResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || !env.Data.Finished || env.Data.Done != 2 || len(env.Data.Results) != 2 || env.Data.Results[0].Session != "older" {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := retained.Load() - before; got != 3 { // two sessions + one replayed note
		t.Errorf("retained %d items, want 3", got)
	}
	before = retained.Load()
	_, out, _ = run(t, "retain", "backfill", "--plan")
	if !strings.Contains(out, "0 session(s) to send (2 already sent") || retained.Load() != before {
		t.Errorf("a second run must find nothing to send:\n%s", out)
	}
}

func TestBackfillStopsAtTheBudgetAndResumes(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	oldSession(t, home, "a", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	oldSession(t, home, "b", time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC), false)
	// Each accepted item costs 10M input tokens ($1.40) on the fake bank.
	llmPerRetain.Store(10_000_000)
	t.Cleanup(func() { llmPerRetain.Store(0) })
	before := retained.Load()
	code, out, _ := run(t, "retain", "backfill", "--max-cost", "0.5")
	if code != output.Partial || !strings.Contains(out, "paused: budget") || retained.Load() != before+1 {
		t.Fatalf("exit %d, retained %d\n%s", code, retained.Load()-before, out)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if st.Backfill == nil || st.Backfill.Paused != "budget" || len(st.Backfill.Sessions) != 1 || st.Backfill.SpentCost < 1.39 {
		t.Errorf("state %+v", st.Backfill)
	}
	code, out, _ = run(t, "retain", "backfill", "--max-cost", "50")
	if code != output.OK || !strings.Contains(out, "finished") || retained.Load() != before+2 {
		t.Errorf("resume: exit %d\n%s", code, out)
	}
}

func TestBackfillRefusesTheSubscriptionLane(t *testing.T) {
	home := devInstall(t, true)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("[llm]\nprovider = \"claude-code\"\nmodel = \"claude-haiku-5-5\"\n")...), 0o600)
	if code, _, errOut := run(t, "retain", "backfill"); code != output.Usage || !strings.Contains(errOut, "--allow-subscription") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestBackfillFilters(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	oldSession(t, home, "sep", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	oldSession(t, home, "oct", time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), false)
	_, out, _ := run(t, "retain", "backfill", "--plan", "--since", "2026-10-01")
	if !strings.Contains(out, "1 session(s) to send") {
		t.Errorf("--since: %s", out)
	}
	_, out, _ = run(t, "retain", "backfill", "--plan", "--project", "other")
	if !strings.Contains(out, "0 session(s) to send") {
		t.Errorf("--project: %s", out)
	}
	if code, _, _ := run(t, "retain", "backfill", "--plan", "--since", "01/10/2026"); code != output.Usage {
		t.Errorf("bad date: exit %d", code)
	}
}

func TestJobsTickContinuesABackfill(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	old := tickBackfillLimit
	tickBackfillLimit = 1
	t.Cleanup(func() { tickBackfillLimit = old })
	for i, sid := range []string{"x1", "x2"} {
		oldSession(t, home, sid, time.Date(2026, 9, 20+i, 9, 0, 0, 0, time.UTC), false)
	}
	d := pathsOf(home)
	_ = state.Update(d, "test", func(s *state.State) error {
		s.Backfill = &state.Backfill{Total: 2, MaxCost: 10, Currency: "USD",
			Sessions: []string{filepath.Join(home, ".claude", "projects", "-home-u-billing", "x1.jsonl"), filepath.Join(home, ".claude", "projects", "-home-u-billing", "x2.jsonl")}}
		return nil
	})
	_, out, _ := run(t, "jobs", "tick")
	if !strings.Contains(out, "backfill 1/2") {
		t.Errorf("%s", out)
	}
	_, out, _ = run(t, "jobs", "tick")
	if !strings.Contains(out, "backfill 2/2") {
		t.Errorf("%s", out)
	}
}
