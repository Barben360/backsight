package recall

import (
	"reflect"
	"testing"
	"time"
)

var indexNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func days(n int) time.Time { return indexNow.Add(-time.Duration(n) * 24 * time.Hour) }

func knownIndex() Index {
	return Index{BuiltAt: indexNow, Known: []ProjectInfo{
		{Name: "backsight", Memories: 8000, Last: days(0)},
		{Name: ".claude", Memories: 2000, Last: days(1)},
		{Name: "hindsight-stack", Memories: 2400, Last: days(15)},
		{Name: "my-homelab", Memories: 3100, Last: days(1)},
		{Name: "infirlib", Memories: 468, Last: days(0)},
		{Name: "Disks", Memories: 9, Last: days(13)},                  // a stray directory: too few
		{Name: "mvgroup-learning", Memories: 25, Last: days(188)},     // idle
		{Name: "unknown-age", Memories: 40},                           // its activity read failed
		{Name: "jobouchon", Memories: ListMinMemories, Last: days(2)}, // exactly the floor
	}}
}

func TestListedLeavesOutSkippedSmallAndIdleProjectsMostRecentFirst(t *testing.T) {
	got := knownIndex().Listed([]string{"backsight", ".claude", "hindsight-stack"}, indexNow)
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	// infirlib (today) before my-homelab (1 day), then jobouchon; the
	// project of unknown activity last.
	want := []string{"infirlib", "my-homelab", "jobouchon", "unknown-age"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("listed %q, want %q", names, want)
	}
	if got := (Index{}).Listed(nil, indexNow); len(got) != 0 {
		t.Errorf("an index without projects listed %v", got)
	}
}

func TestNamedMatchesWholeProjectNamesOnly(t *testing.T) {
	idx := knownIndex()
	idx.Known = append(idx.Known, ProjectInfo{Name: "infra", Memories: 50, Last: days(1)}, ProjectInfo{Name: "web", Memories: 500, Last: days(1)})
	skip := []string{"backsight", ".claude"}
	for _, c := range []struct {
		text string
		want []string
	}{
		{"Deploy it on My-Homelab like infirlib does", []string{"my-homelab", "infirlib"}},
		{"see my-homelabs and infirlib2", nil},                              // not whole words
		{"the web front talks to Disks", nil},                               // too short, too few memories
		{"what did mvgroup-learning decide?", []string{"mvgroup-learning"}}, // idle is still named
		{"backsight and .claude only", nil},                                 // skipped
		{"INFRA", []string{"infra"}},
	} {
		if got := idx.Named(c.text, skip); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Named(%q) = %q, want %q", c.text, got, c.want)
		}
	}
	long := make([]byte, maxMatchChars)
	for i := range long {
		long[i] = ' '
	}
	if got := idx.Named(string(long)+" infirlib", nil); got != nil {
		t.Errorf("a name past the first %d bytes was matched: %q", maxMatchChars, got)
	}
}

func TestHasAndClosest(t *testing.T) {
	idx := knownIndex()
	if has, ok := idx.Has("my-homelab"); !has || !ok {
		t.Errorf("known: %v %v", has, ok)
	}
	if has, ok := idx.Has("my-homelb"); has || !ok {
		t.Errorf("unknown: %v %v", has, ok)
	}
	if has, ok := (Index{}).Has("x"); has || ok {
		t.Errorf("no projects: %v %v (an index without projects cannot tell)", has, ok)
	}
	if got := idx.Closest("My-Homelb", 2); len(got) != 1 || got[0] != "my-homelab" {
		t.Errorf("closest to a typo: %q", got)
	}
	if got := idx.Closest("hindsight", 3); len(got) != 1 || got[0] != "hindsight-stack" {
		t.Errorf("closest to a part of a name: %q", got)
	}
	if got := idx.Closest("photos", 3); len(got) != 0 {
		t.Errorf("a name far from every project got %q", got)
	}
}

func TestStaleAfterADayOrWithoutProjects(t *testing.T) {
	names := map[string][]ProjectCount{"Grafana": {{"garage", 3}}}
	for _, c := range []struct {
		name string
		idx  Index
		want bool
	}{
		{"fresh with projects", Index{BuiltAt: indexNow, Names: names, Known: []ProjectInfo{{Name: "garage", Memories: 3}}}, false},
		{"a day old", Index{BuiltAt: indexNow.Add(-IndexMaxAge), Names: names, Known: []ProjectInfo{{Name: "garage"}}}, true},
		{"written before the projects", Index{BuiltAt: indexNow, Names: names}, true},
		{"an empty bank", Index{BuiltAt: indexNow}, false},
	} {
		if got := c.idx.Stale(indexNow); got != c.want {
			t.Errorf("%s: stale %v", c.name, got)
		}
	}
}
