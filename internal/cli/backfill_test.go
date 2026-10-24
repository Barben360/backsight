package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
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
	oldPoll, oldFinal, oldTick := backfillPoll, finalQueueWait, tickQueueWait
	backfillPoll, finalQueueWait, tickQueueWait = time.Millisecond, 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { backfillPoll, finalQueueWait, tickQueueWait = oldPoll, oldFinal, oldTick; llmInput.Store(0) })
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
	if code != output.OK || p.Send != 1 || p.Notes != 1 || p.Estimate.Cost <= 0 || p.MaxCost < 1.49*p.Estimate.Cost || p.MaxCost > 1.51*p.Estimate.Cost ||
		p.Estimate.Chars == 0 || p.Currency != "USD" || retained.Load() != before {
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
	if code != output.OK || !env.Data.Finished || env.Data.Done != 2 || env.Data.Total != 2 || len(env.Data.Results) != 2 || env.Data.Results[0].Session != "older" {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := retained.Load() - before; got != 3 { // two sessions + one replayed note
		t.Errorf("retained %d items, want 3", got)
	}
	note := false
	for _, tags := range retainedTags {
		if strings.Contains(tags, "source:note") {
			note = strings.Contains(tags, "kind:decision") && strings.Contains(tags, "project:billing")
		}
	}
	if !note {
		t.Errorf("the replayed note lost its kind or project: %v", retainedTags)
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
	// A plain rerun does not grant a new cap.
	if code, out, _ := run(t, "retain", "backfill"); code != output.Partial || !strings.Contains(out+"", "") || retained.Load() != before+1 {
		t.Errorf("a plain rerun after a budget pause must stay paused: exit %d", code)
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
	queue := []string{filepath.Join(home, ".claude", "projects", "-home-u-billing", "x1.jsonl"), filepath.Join(home, ".claude", "projects", "-home-u-billing", "x2.jsonl")}
	_ = state.Update(d, "test", func(s *state.State) error {
		s.Backfill = &state.Backfill{Total: 2, MaxCost: 10, Currency: "USD", StartDay: time.Now().UTC().Format("2006-01-02"), Paused: "budget", Sessions: queue}
		return nil
	})
	if _, out, _ := run(t, "jobs", "tick"); strings.Contains(out, ", backfill ") || retained.Load() != 0 {
		t.Fatalf("a tick continued a paused backfill: %s", out)
	}
	_ = state.Update(d, "test", func(s *state.State) error { s.Backfill.Paused = ""; return nil })
	for i, want := range []string{"backfill 1/2", "backfill 2/2"} {
		_, out, _ := run(t, "jobs", "tick")
		if !strings.Contains(out, want) || retained.Load() != int64(i+1) {
			t.Errorf("tick %d: retained %d\n%s", i+1, retained.Load(), out)
		}
	}
}

// Backfill waits for the server's queue before each session.
func TestBackfillWaitsForTheQueue(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	oldSession(t, home, "q1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	queued.Store(int64(backfillQueueLimit) + 5) // pending drains by one per poll
	processing.Store(1)                         // counted too: the wait ends at limit-1 pending
	before := retained.Load()
	run(t, "retain", "backfill")
	if retained.Load() != before+1 || queued.Load()+processing.Load() > int64(backfillQueueLimit) {
		t.Errorf("sent %d, queue %d", retained.Load()-before, queued.Load())
	}
}

// Spend the bank had before the backfill started is not billed to it.
func TestBackfillBaselineExcludesEarlierSpend(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	llmInput.Store(50_000_000) // $7 of earlier work today
	oldSession(t, home, "a", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	code, out, _ := run(t, "retain", "backfill", "--max-cost", "0.5", "--json")
	var env struct {
		Data backfillResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || !env.Data.Finished || env.Data.Spent != 0 {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestBackfillRefusesToStartWithoutUsage(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	statsDown.Store(true)
	oldSession(t, home, "a", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	before := retained.Load()
	if code, _, errOut := run(t, "retain", "backfill"); code != output.Blocked || !strings.Contains(errOut, "spending cap") || retained.Load() != before {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

// A measurement failure mid-run pauses instead of sending uncapped.
func TestBackfillPausesWhenMeasurementFails(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	oldSession(t, home, "a", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	oldSession(t, home, "b", time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC), false)
	statsFailAfter.Store(2) // the baseline and the check before "a" work, the check before "b" fails
	code, out, _ := run(t, "retain", "backfill", "--json")
	st, _ := state.Load(pathsOf(home).StateFile())
	if code != output.Partial || st.Backfill == nil || st.Backfill.Paused != "measure" || retained.Load() != 1 {
		t.Errorf("exit %d, retained %d\n%s", code, retained.Load(), out)
	}
}

// A server failure mid-run pauses and keeps the session queued.
func TestBackfillServerFailureKeepsTheSession(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	oldSession(t, home, "a", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	oldSession(t, home, "b", time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC), false)
	failRetains.Store(1) // the second retain gets HTTP 500
	code, _, _ := run(t, "retain", "backfill")
	st, _ := state.Load(pathsOf(home).StateFile())
	if code != output.Partial || st.Backfill.Paused != "server" || len(st.Backfill.Sessions) != 1 || st.Backfill.Done != 1 ||
		!strings.HasSuffix(st.Backfill.Sessions[0], "b.jsonl") {
		t.Errorf("exit %d, state %+v", code, st.Backfill)
	}
	failRetains.Store(0)
	if code, out, _ := run(t, "retain", "backfill"); code != output.OK || !strings.Contains(out, "finished") {
		t.Errorf("resume after the server is back: exit %d\n%s", code, out)
	}
}

func TestBackfillTokenCapForUnpricedModels(t *testing.T) {
	home := devInstall(t, true)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	pointAt(t, home, fakeHindsight(t))
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("[llm]\nprovider = \"ollama\"\nmodel = \"qwen3:8b\"\n")...), 0o600)
	fastBackfill(t)
	oldSession(t, home, "a", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	oldSession(t, home, "b", time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC), false)
	if code, _, errOut := run(t, "retain", "backfill", "--max-cost", "1"); code != output.Usage || !strings.Contains(errOut, "--max-tokens") {
		t.Errorf("--max-cost without a price: exit %d: %s", code, errOut)
	}
	llmPerRetain.Store(5000)
	code, out, _ := run(t, "retain", "backfill", "--max-tokens", "1000")
	if code != output.Partial || !strings.Contains(out, "paused: budget") || retained.Load() != 1 {
		t.Errorf("exit %d, retained %d\n%s", code, retained.Load(), out)
	}
}

// Explicit saves of an opted-out session are never replayed.
func TestBackfillOptedOutSessionSendsNoNotes(t *testing.T) {
	home := devInstall(t, true)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	pointAt(t, home, fakeHindsight(t))
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("[privacy]\nexclude_paths = [\""+filepath.Join(home, "billing")+"\"]\n")...), 0o600)
	fastBackfill(t)
	oldSession(t, home, "secret", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), true)
	_, out, _ := run(t, "retain", "backfill", "--plan", "--json")
	var env struct {
		Data backfillResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if env.Data.Plan.OptedOut != 1 || env.Data.Plan.Notes != 0 || env.Data.Plan.Send != 0 {
		t.Errorf("plan %+v", env.Data.Plan)
	}
	run(t, "retain", "backfill")
	if retained.Load() != 0 {
		t.Errorf("an opted-out session leaked %d items", retained.Load())
	}
}

// A session already sent that holds explicit saves is queued for them.
func TestBackfillReplaysNotesOfSentSessions(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	oldSession(t, home, "live", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), true)
	run(t, "retain", "flush", "--session", "live")
	_, out, _ := run(t, "retain", "backfill", "--plan", "--json")
	var env struct {
		Data backfillResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if env.Data.Plan.NotesOnly != 1 || env.Data.Plan.Notes != 1 || env.Data.Plan.Send != 0 {
		t.Fatalf("plan %+v", env.Data.Plan)
	}
	before := retained.Load()
	run(t, "retain", "backfill")
	if retained.Load() != before+1 {
		t.Errorf("want the note only, got %d items", retained.Load()-before)
	}
}

// Other filters build a new queue; the spend so far still counts.
func TestBackfillFilterChangeKeepsTheSpend(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	oldSession(t, home, "sep", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	oldSession(t, home, "oct", time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), false)
	llmPerRetain.Store(10_000_000)
	run(t, "retain", "backfill", "--max-cost", "1") // pauses after "sep" ($1.40)
	code, _, _ := run(t, "retain", "backfill", "--since", "2026-10-01", "--max-cost", "1")
	st, _ := state.Load(pathsOf(home).StateFile())
	if code != output.Partial || st.Backfill.Paused != "budget" || st.Backfill.SpentCost < 1.39 {
		t.Errorf("exit %d, state %+v", code, st.Backfill)
	}
}

func TestBackfillProjectsDir(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	other := filepath.Join(home, "other-profile", "projects")
	p := filepath.Join(other, "-x", "o1.jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte(`{"type":"user","uuid":"o1-u","sessionId":"o1","cwd":"`+filepath.Join(home, "x")+`","timestamp":"2026-09-20T09:00:00Z","message":{"role":"user","content":"from another profile"}}`+"\n"), 0o600)
	_, out, _ := run(t, "retain", "backfill", "--plan", "--projects-dir", other)
	if !strings.Contains(out, "1 session(s) to send") {
		t.Errorf("%s", out)
	}
}

// Ctrl-C pauses the backfill so that ticks do not continue it.
func TestBackfillInterruptPauses(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	fastBackfill(t)
	oldSession(t, home, "a", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	d := pathsOf(home)
	_ = state.Update(d, "test", func(s *state.State) error {
		s.Backfill = &state.Backfill{Total: 1, MaxCost: 10, Currency: "USD", StartDay: "2026-10-24",
			Sessions: []string{filepath.Join(home, ".claude", "projects", "-home-u-billing", "a.jsonl")}}
		return nil
	})
	cfg, sec, _ := loadServerConfig()
	f, _ := newFlusher(d, home, cfg, sec, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runBackfill(ctx, f, d, cfg, retain.PricesFor(cfg.LLM.Model, 0, 0, 0, "USD"), false)
	st, _ := state.Load(d.StateFile())
	if err == nil || st.Backfill.Paused != "interrupted" || retained.Load() != 0 {
		t.Errorf("%v %+v", err, st.Backfill)
	}
	if _, out, _ := run(t, "jobs", "tick"); strings.Contains(out, ", backfill ") || retained.Load() != 0 {
		t.Errorf("a tick continued an interrupted backfill: %s", out)
	}
}
