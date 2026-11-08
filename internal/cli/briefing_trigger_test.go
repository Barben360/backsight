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
	if !strings.Contains(out, `"briefing_triggers": "1 briefing page(s) given the 6h refresh floor"`) {
		t.Errorf("tick result:\n%s", out)
	}
	if tel := telemetryText(t, home); !strings.Contains(tel, `"event":"briefing_trigger"`) || !strings.Contains(tel, `"checked":1`) || !strings.Contains(tel, `"patched":1`) || !strings.Contains(tel, `"failed":0`) {
		t.Errorf("telemetry:\n%s", tel)
	}
	// Drifted again (an older client recreated it): this binary's ticks
	// leave it to jobs maintain.
	mentalModels[page] = oldPage(page)
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 1 {
		t.Errorf("a second tick of the same binary and floor patched: %q", p)
	}
	// Another floor: the next tick reconciles to it.
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[recall]\nbriefing_refresh_interval = \"0\"\n")...), 0o600)
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 2 || !strings.Contains(p[1], `"min_refresh_interval_seconds":0}`) {
		t.Errorf("a changed floor was not applied: %q", p)
	}
}

// A page whose update failed leaves the marker unwritten: the next tick
// tries again.
func TestTickRetriesAFailedBriefingTrigger(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recentHasEventTime.Store(true)
	recentByTag["project:billing"] = `[{"id":"a","text":"t","mentioned_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}]`
	page := briefingModelID("billing")
	mentalModels[page] = oldPage(page)
	mentalPatchStatus.Store(500)
	_, out, _ := run(t, "jobs", "tick", "--json")
	if !strings.Contains(out, "1 briefing page(s) not updated ("+page+": ") {
		t.Errorf("the failure is not reported:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(pathsOf(home).State, "briefing-trigger")); err == nil {
		t.Error("marker written although a page failed")
	}
	mentalPatchStatus.Store(0)
	run(t, "jobs", "tick")
	if p := triggerPatches(); len(p) != 2 || !strings.Contains(mentalModels[page], `"min_refresh_interval_seconds":21600`) {
		t.Errorf("the next tick did not retry: %q", p)
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
