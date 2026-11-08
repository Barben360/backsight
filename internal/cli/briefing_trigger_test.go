package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	floorPATCH = `{"trigger":{"budget":"low","mode":"full","refresh_after_consolidation":true,"min_refresh_interval_seconds":21600}}`
	// noFloor: a page made before decision 0083 C2.
	noFloor = `{"budget":"low","mode":"full","refresh_after_consolidation":true,"min_refresh_interval_seconds":null}`
)

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
	if p := triggerPatches(); len(p) != 1 || p[0] != "PATCH "+page+" "+floorPATCH {
		t.Fatalf("want one PATCH of the backsight page with the whole trigger, got %q", p)
	}
	if d := pathsOf(home); !ownEntry(d, d.State, "briefing-trigger") {
		t.Error("uninstall would leave the marker behind")
	}
	if !strings.Contains(out, `"briefing_triggers": "1 briefing page(s) given the 6h refresh floor"`) {
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
	if p := triggerPatches(); len(p) != 1 || p[0] != "PATCH "+page+" "+floorPATCH || !strings.Contains(out, "1 briefing page(s) given the 6h refresh floor") {
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
