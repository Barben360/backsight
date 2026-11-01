package dashboard

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
	_ "time/tzdata" // parisZone loads Europe/Paris whatever the host has

	"github.com/Barben360/backsight/internal/metrics"
)

// pageGoldens names, per page, the block 2 goldens its declared views are
// executed on, in the order of the page's views header. memory_text runs
// on the memory page's golden with a synthetic text (fixtureData).
var pageGoldens = map[string][]string{
	"overview": {"overview"}, "quality": {"quality"}, "cost": {"cost"}, "bank": {"bank"}, "pipeline": {"pipeline"},
	"system": {"system"}, "releases": {"releases"}, "defs": {"defs"}, "projects": {"projects"}, "project": {"project-page"},
	"sessions": {"sessions"}, "session": {"session-page"}, "memories": {"memories"}, "memory": {"memory-page"},
	"days": {"days"}, "day": {"day-page"}, "memory_text": {"memory-page"},
}

// goldenPath is the file of the stats view golden called name.
func goldenPath(name string) string {
	return filepath.Join("..", "cli", "testdata", "stats-view-"+name+"-json.golden")
}

// goldenResult is the "data" of the stats view golden called name: a
// fresh Result at each call, so no test shares one.
func goldenResult(t *testing.T, name string) *metrics.Result {
	t.Helper()
	raw, err := os.ReadFile(goldenPath(name))
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	var env struct {
		Data *metrics.Result `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Data == nil {
		t.Fatalf("golden %s: data %v, error %v", name, env.Data, err)
	}
	// A field the struct no longer reads (a renamed json tag) would stay
	// zero and the pages would run on less data than the golden holds: the
	// data must survive a round trip through the struct.
	var rawEnv struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &rawEnv); err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	again, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if json.Unmarshal(rawEnv.Data, &want) != nil || json.Unmarshal(again, &got) != nil || !reflect.DeepEqual(want, got) {
		t.Fatalf("golden %s: the data does not survive a round trip through metrics.Result (a renamed json field?)", name)
	}
	return env.Data
}

// parisZone is Europe/Paris, from the embedded zone database: a fixed
// offset would be wrong on one side of each DST change.
func parisZone() *time.Location {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		panic("Europe/Paris: " + err.Error())
	}
	return loc
}

// fixtureData is the page data of spec, with a full frame: the goldens of
// its views and the Paris zone, or with empty an empty Result per view and no zone.
func fixtureData(t *testing.T, spec pageSpec, empty bool) PageData {
	t.Helper()
	d := PageData{V: map[string]*metrics.Result{}, Loc: parisZone()}
	if empty {
		d.Loc = nil
	}
	goldens := pageGoldens[spec.Name]
	if len(goldens) != len(spec.Views) {
		t.Fatalf("page %s: %d goldens for %d views", spec.Name, len(goldens), len(spec.Views))
	}
	for i, v := range spec.Views {
		if empty {
			d.V[v] = &metrics.Result{}
		} else {
			d.V[v] = goldenResult(t, goldens[i])
		}
	}
	// No golden holds a memory's text: the text page gets a synthetic one,
	// and none in the empty run.
	if spec.Name == "memory_text" && !empty {
		d.V["memories"].Text = &metrics.MemoryText{ID: "mem-long-1", Text: "fixture <b>text</b>"}
	}
	head := d.V[spec.Views[0]]
	d.Frame = Frame{
		Title: "Title", Path: "/", Version: "test", HelpURL: helpURL,
		Nav: navFor("/", true), Windows: windowsFor("/", url.Values{}), PrefMenu: prefMenu("/", Prefs{Theme: "auto"}),
		Prefs: Prefs{Theme: "auto", Dev: true}, AllProjects: "/", Scope: &head.Scope, Generated: head.GeneratedAt,
		Keep: [][2]string{{"days", "7"}},
	}
	d.Frame.SessionHead = spec.Name == "session" // as the handler sets it
	return d
}
