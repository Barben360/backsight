package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// noFloor: a page made before decision 0083 C2.
const noFloor = `{"budget":"low","mode":"full","refresh_after_consolidation":true,"min_refresh_interval_seconds":null}`

// patchBody is the update a page gets: the current request and the whole
// trigger, with the 6 h floor.
func patchBody(query string) string {
	q, _ := json.Marshal(query)
	return `{"source_query":` + string(q) + `,"trigger":{"budget":"low","mode":"full","refresh_after_consolidation":true,"min_refresh_interval_seconds":21600}}`
}

// oldPage is a briefing page of billing made before decision 0083 C2.
func oldPage(id string) string {
	return `{"id":"` + id + `","content":"x","tags":["project:billing"],"last_refreshed_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","trigger":` + noFloor + `}`
}

func triggerPatches() []string {
	var out []string
	for _, c := range mentalCalls {
		if strings.HasPrefix(c, "PATCH ") {
			out = append(out, c)
		}
	}
	return out
}

// The first tick of a binary gives every briefing page the floor, once:
// the next ticks look no more until the version or the floor changes.
func TestTickGivesBriefingPagesTheFloorOncePerVersion(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recentHasEventTime.Store(true)
	recentByTag["project:billing"] = `[{"id":"a","text":"t","mentioned_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}]`
	page, other := briefingModelID("billing"), "team-notes"
	mentalModels[page], mentalModels[other] = oldPage(page), oldPage(other)
	_, out, _ := run(t, "jobs", "tick", "--json")
	if p := triggerPatches(); len(p) != 1 || p[0] != "PATCH "+page+" "+patchBody(briefingQuery) {
		t.Fatalf("want one PATCH of the backsight page with the whole trigger, got %q", p)
	}
	if d := pathsOf(home); !ownEntry(d, d.State, "briefing-trigger") {
		t.Error("uninstall would leave the marker behind")
	}
	if !strings.Contains(out, `"briefing_triggers": "1 briefing page(s) given the 6h refresh floor and the current briefing request"`) {
		t.Errorf("tick result:\n%s", out)
	}
	if tel := telemetryText(t, home); !strings.Contains(tel, `"event":"briefing_trigger"`) || !strings.Contains(tel, `"checked":1`) || !strings.Contains(tel, `"patched":1`) || !strings.Contains(tel, `"failed":0`) {
		t.Errorf("telemetry:\n%s", tel)
	}
	// Drifted again (an older client recreated it): this binary's ticks
	// leave it to jobs maintain, and list the pages without their content.
	mentalModels[page] = oldPage(page)
	mentalLists = nil
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 1 {
		t.Errorf("a second tick of the same binary and floor patched: %q", p)
	}
	if len(mentalLists) != 1 || strings.Contains(mentalLists[0], "detail=content") {
		t.Errorf("the tick's pruning must list without content: %q", mentalLists)
	}
	// Another floor: the next tick reconciles to it (a null floor would
	// already be no floor).
	mentalModels[page] = strings.Replace(oldPage(page), `"min_refresh_interval_seconds":null`, `"min_refresh_interval_seconds":21600`, 1)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[recall]\nbriefing_refresh_interval = \"0\"\n")...), 0o600)
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 2 || !strings.Contains(p[1], `"min_refresh_interval_seconds":0}`) {
		t.Errorf("a changed floor was not applied: %q", p)
	}
}

// A page whose update failed is tried again by the ticks, at most once an
// hour (not at every tick); a page deleted meanwhile (404) is done.
func TestTickRetriesAFailedBriefingTriggerHourly(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recentHasEventTime.Store(true)
	recentByTag["project:billing"] = `[{"id":"a","text":"t","mentioned_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}]`
	prev := now
	t.Cleanup(func() { now = prev })
	at := time.Now()
	now = func() time.Time { return at }
	page := briefingModelID("billing")
	mentalModels[page] = oldPage(page)
	mentalPatchStatus.Store(422) // an older server refusing the field
	_, out, _ := run(t, "jobs", "tick", "--json")
	if !strings.Contains(out, "1 briefing page(s) not updated ("+page+": ") {
		t.Errorf("the failure is not reported:\n%s", out)
	}
	at = at.Add(59 * time.Minute)
	if _, out, _ = run(t, "jobs", "tick", "--json"); len(triggerPatches()) != 1 || strings.Contains(out, "briefing page") {
		t.Errorf("a tick within the hour tried again: %q\n%s", triggerPatches(), out)
	}
	at = at.Add(2 * time.Minute)
	mentalPatchStatus.Store(0)
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 2 || !strings.Contains(mentalModels[page], `"min_refresh_interval_seconds":21600`) {
		t.Errorf("the tick after the hour did not retry: %q", p)
	}

	// Deleted between the listing and the update: done, never retried.
	other := briefingModelID("ops")
	mentalModels[other] = oldPage(other)
	_ = os.Remove(pathsOf(home).BriefingTriggerFile())
	mentalPatchStatus.Store(404)
	run(t, "jobs", "tick")
	n := len(triggerPatches())
	at = at.Add(2 * time.Hour)
	run(t, "jobs", "tick")
	if n != 3 || len(triggerPatches()) != n {
		t.Errorf("a deleted page was retried (or never tried): %q", triggerPatches())
	}
}

// jobs maintain reconciles every day, marker or not; with briefing =
// "recent" no page is touched.
func TestMaintainReconcilesBriefingTriggers(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[backup]\nenabled = false\n")...), 0o600)
	page := briefingModelID("billing")
	mentalModels[page] = oldPage(page)
	_ = os.WriteFile(filepath.Join(pathsOf(home).State, "briefing-trigger"), []byte("whatever\n"), 0o600)
	_, out, _ := run(t, "jobs", "maintain")
	if p := triggerPatches(); len(p) != 1 || p[0] != "PATCH "+page+" "+patchBody(briefingQuery) || !strings.Contains(out, "1 briefing page(s) given the 6h refresh floor") {
		t.Fatalf("maintain: %q\n%s", p, out)
	}
	_, out, _ = run(t, "jobs", "maintain")
	if p := triggerPatches(); len(p) != 1 || strings.Contains(out, "briefing page") {
		t.Errorf("a second maintain must patch nothing and say nothing: %q\n%s", p, out)
	}
	mentalModels[page] = oldPage(page)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[backup]\nenabled = false\n[recall]\nbriefing = \"recent\"\n")...), 0o600)
	run(t, "jobs", "maintain")
	if p := triggerPatches(); len(p) != 1 {
		t.Errorf("briefing = recent: %q", p)
	}
}

// jobs maintain gives the pages their floor before the subagent sweep
// (whose purges make the server consolidate, which refreshes the pages)
// and before the update check (an update applied must not leave this
// binary's request on the pages) (decision 0083 C2, C3).
func TestMaintainReconcilesBriefingTriggersFirst(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[backup]\nenabled = false\n")...), 0o600)
	page := briefingModelID("billing")
	mentalModels[page] = oldPage(page)
	var mu sync.Mutex
	var log []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		log = append(log, s)
	}
	fakeHook = func(r *http.Request) { note(r.Method + " " + r.URL.Path) }
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		note("update check")
		http.NotFound(w, r)
	}))
	t.Cleanup(gh.Close)
	old := githubAPI
	githubAPI = gh.URL
	t.Cleanup(func() { githubAPI = old })
	run(t, "jobs", "maintain")
	mu.Lock()
	defer mu.Unlock()
	first := func(match func(string) bool) int { return slices.IndexFunc(log, match) }
	patch := first(func(s string) bool {
		return strings.HasPrefix(s, "PATCH ") && strings.Contains(s, "/mental-models/"+page)
	})
	sweep := first(func(s string) bool { return strings.HasPrefix(s, "GET ") && strings.HasSuffix(s, "/documents") })
	update := first(func(s string) bool { return s == "update check" })
	if patch < 0 || sweep < 0 || update < 0 {
		t.Fatalf("patch %d, sweep listing %d, update check %d: a step did not run\n%s", patch, sweep, update, strings.Join(log, "\n"))
	}
	if patch > sweep || patch > update {
		t.Errorf("the pages were reconciled after the sweep or the update check\n%s", strings.Join(log, "\n"))
	}
}

// A restore (a new restore epoch) or another bank makes the next tick
// reconcile again, without waiting for jobs maintain: the pages may be
// older than the marker says.
func TestTickReconcilesBriefingTriggersAfterARestoreOrABankChange(t *testing.T) {
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, port)
	recentHasEventTime.Store(true)
	recentByTag["project:billing"] = `[{"id":"a","text":"t","mentioned_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}]`
	page := briefingModelID("billing")
	mentalModels[page] = oldPage(page)
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 1 {
		t.Fatalf("first tick: %q", p)
	}
	mentalModels[page] = oldPage(page) // the restored server holds the page as it was
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 1 {
		t.Fatalf("no restore, same marker: %q", p)
	}
	cfg, _, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := retain.WriteEpoch(context.Background(), clientOn(port), cfg.Server.Bank, retain.Epoch{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	run(t, "jobs", "tick") // its epoch step reads the restore, then the pages are reconciled
	if st, _ := state.Load(pathsOf(home).StateFile()); st.EpochOf(cfg.Server.Bank) != 1 {
		t.Fatalf("the tick did not take the restore: %+v", st.RestoreEpoch)
	}
	if p := triggerPatches(); len(p) != 2 || p[1] != "PATCH "+page+" "+patchBody(briefingQuery) {
		t.Fatalf("after a restore the tick did not reconcile: %q", p)
	}
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 2 {
		t.Fatalf("the restore was reconciled twice: %q", p)
	}
	mentalModels[page] = oldPage(page)
	path := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(path)
	_ = os.WriteFile(path, []byte(strings.Replace(string(raw), "[server]", "[server]\nbank = \"team-memory\"", 1)), 0o600)
	if c, _, _ := loadServerConfig(); c.Server.Bank != "team-memory" {
		t.Fatalf("the bank did not change: %q", c.Server.Bank)
	}
	// The same epoch number on the other bank: the bank alone tells them apart.
	if err := retain.WriteEpoch(context.Background(), clientOn(port), "team-memory", retain.Epoch{Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	run(t, "jobs", "tick")
	if st, _ := state.Load(pathsOf(home).StateFile()); st.EpochOf("team-memory") != 1 {
		t.Fatalf("the tick did not take the other bank's epoch: %+v", st.RestoreEpoch)
	}
	if p := triggerPatches(); len(p) != 3 {
		t.Errorf("another bank: the tick did not reconcile: %q", p)
	}
}

// 0083 C7: a new briefing request reaches the pages made with an earlier
// one at the first tick (the marker names the request), and only once.
func TestTickGivesBriefingPagesANewQueryOnce(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recentHasEventTime.Store(true)
	recentByTag["project:billing"] = `[{"id":"a","text":"t","mentioned_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}]`
	prev := briefingQuery
	t.Cleanup(func() { briefingQuery = prev })
	page := briefingModelID("billing")
	mentalModels[page] = oldPage(page)
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 1 || p[0] != "PATCH "+page+" "+patchBody(prev) {
		t.Fatalf("first tick: %q", p)
	}
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 1 {
		t.Fatalf("same binary, floor and request: %q", p)
	}
	briefingQuery = "Briefing, asked another way."
	_, out, _ := run(t, "jobs", "tick", "--json")
	if p := triggerPatches(); len(p) != 2 || p[1] != "PATCH "+page+" "+patchBody(briefingQuery) || !strings.Contains(out, "1 briefing page(s) given") {
		t.Fatalf("a changed request was not applied at the next tick: %q\n%s", p, out)
	}
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 2 {
		t.Errorf("the new request was patched again: %q", p)
	}
}
