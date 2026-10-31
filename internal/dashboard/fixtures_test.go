package dashboard

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
)

// pageGoldens names, per page, the block 2 goldens its declared views are
// executed on, in the order of the page's views header. memory_text is
// added with its page.
var pageGoldens = map[string][]string{
	"overview": {"overview"}, "quality": {"quality"}, "cost": {"cost"}, "bank": {"bank"}, "pipeline": {"pipeline"},
	"system": {"system"}, "releases": {"releases"}, "defs": {"defs"}, "projects": {"projects"}, "project": {"project-page"},
	"sessions": {"sessions"}, "session": {"session-page"}, "memories": {"memories"}, "memory": {"memory-page"},
	"days": {"days"}, "day": {"day-page"},
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
	return env.Data
}

// parisZone is Europe/Paris, or a fixed +02:00 zone of that name on a
// machine without the zone database.
func parisZone() *time.Location {
	if loc, err := time.LoadLocation("Europe/Paris"); err == nil {
		return loc
	}
	return time.FixedZone("Europe/Paris", 2*3600)
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
	head := d.V[spec.Views[0]]
	d.Frame = Frame{
		Title: "Title", Path: "/", Version: "test", HelpURL: helpURL,
		Nav: navFor("/", true), Windows: windowsFor("/", url.Values{}), PrefMenu: prefMenu("/", Prefs{Theme: "auto"}),
		Prefs: Prefs{Theme: "auto", Dev: true}, AllProjects: "/", Scope: &head.Scope, Generated: head.GeneratedAt,
		Keep: [][2]string{{"days", "7"}},
	}
	return d
}
