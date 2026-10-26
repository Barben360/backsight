package retain

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/attribution"
)

// turns builds n turns of session sid touching projects.
func turns(sid string, n int, at time.Time, projects ...string) []TurnEntry {
	var out []TurnEntry
	for i := range n {
		var v attribution.Votes
		for _, p := range projects {
			v.Explicit = append(v.Explicit, attribution.Count{Project: p, N: 1})
		}
		out = append(out, TurnEntry{Session: sid, Turn: fmt.Sprintf("%s-%s-%d", sid, projects[0], i), At: at, Votes: v})
	}
	return out
}

func TestFamiliesLinkProjectsWorkedOnTogether(t *testing.T) {
	at := clockAt(9, 0)
	var all []TurnEntry
	add := func(ts []TurnEntry) { all = append(all, ts...) }
	// backsight and spec: 6 turns together over 2 sessions, backsight has
	// 20 turns: 30 % of the larger → linked.
	add(turns("s1", 3, at, "backsight", "spec"))
	add(turns("s2", 3, at, "spec", "backsight"))
	add(turns("s1", 14, at, "backsight"))
	// clinic and its site: 6 turns, one session → not linked.
	add(turns("s3", 6, at, "clinic", "clinic-site"))
	// garage and trip: 4 turns in 2 sessions → under 5 turns.
	add(turns("s4", 2, at, "garage", "trip"))
	add(turns("s5", 2, at, "trip", "garage"))
	// dotfiles, a hub: 100 turns, 5 of 3 sessions with tiny (6 turns):
	// 5 % of the larger one → not linked (it would be 83 % of the smaller).
	add(turns("s6", 95, at, "dotfiles"))
	add(turns("s6", 2, at, "dotfiles", "tiny"))
	add(turns("s7", 2, at, "tiny", "dotfiles"))
	add(turns("s8", 1, at, "dotfiles", "tiny"))
	add(turns("s8", 1, at, "tiny"))
	// spec and old-stack linked too: pairwise, backsight does not get old-stack.
	add(turns("s9", 3, at, "spec", "old-stack"))
	add(turns("s10", 3, at, "old-stack", "spec"))
	f := ComputeFamilies(all)
	want := map[string][]string{"backsight": {"spec"}, "spec": {"backsight", "old-stack"}, "old-stack": {"spec"}}
	if !reflect.DeepEqual(f.Of, want) {
		t.Errorf("families %v, want %v; links %+v", f.Of, want, f.Links)
	}
	for _, l := range f.Links {
		if l.A == "backsight" && l.B == "spec" && (l.Turns != 6 || l.Sessions != 2 || l.Share != 0.3) {
			t.Errorf("link %+v", l)
		}
	}
}

func TestAFamilyKeepsItsTenMostCoTouchedProjects(t *testing.T) {
	at := clockAt(9, 0)
	libs := []string{"app"}
	for i := range 12 {
		libs = append(libs, fmt.Sprintf("lib%02d", i))
	}
	// 20 turns of two sessions touch app and every lib; lib<i> has i more
	// turns with app, so lib11 is the most co-touched.
	all := append(turns("a", 10, at, libs...), turns("b", 10, at, libs...)...)
	for i := range 12 {
		all = append(all, turns("c", i, at, "app", fmt.Sprintf("lib%02d", i))...)
	}
	f := ComputeFamilies(all)
	got := f.Of["app"]
	if len(got) != MaxFamily || got[0] != "lib11" || got[9] != "lib02" {
		t.Errorf("app's family %v", got)
	}
}

// Only the window's turns count: a link of a past phase decays.
func TestFamiliesComeFromTheLastSixtyDays(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	write := func(ts []TurnEntry) {
		for _, x := range ts {
			x.Agent = x.Turn
			e.f.Attribution.saveAgent(x)
		}
	}
	now := clockAt(12, 0)
	write(turns("s1", 3, now.Add(-24*time.Hour), "backsight", "spec"))
	write(turns("s2", 3, now.Add(-24*time.Hour), "backsight", "spec"))
	write(turns("s3", 3, now.Add(-70*24*time.Hour), "clinic", "site"))
	write(turns("s4", 3, now.Add(-70*24*time.Hour), "clinic", "site"))
	path := filepath.Join(t.TempDir(), "families.json")
	f, err := RefreshFamilies(e.f.Attribution, path, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Of, map[string][]string{"backsight": {"spec"}, "spec": {"backsight"}}) || f.Turns != 6 {
		t.Errorf("%+v", f)
	}
	back, err := LoadFamilies(path)
	if err != nil || !reflect.DeepEqual(back.Of, f.Of) || !back.BuiltAt.Equal(now) {
		t.Errorf("read back %+v %v", back, err)
	}
	if none, err := LoadFamilies(filepath.Join(t.TempDir(), "missing.json")); err != nil || len(none.Of) != 0 {
		t.Errorf("a missing file: %+v %v", none, err)
	}
	_ = os.WriteFile(path, []byte("{"), 0o600)
	if _, err := LoadFamilies(path); err == nil {
		t.Error("a broken file read as empty")
	}
}
