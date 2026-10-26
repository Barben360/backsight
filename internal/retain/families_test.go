package retain

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	f := ComputeFamilies(all, nil)
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
	f := ComputeFamilies(all, nil)
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
			if _, err := e.f.Attribution.keepAgent(x); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := clockAt(12, 0)
	write(turns("s1", 3, now.Add(-24*time.Hour), "backsight", "spec"))
	write(turns("s2", 3, now.Add(-24*time.Hour), "backsight", "spec"))
	write(turns("s3", 3, now.Add(-70*24*time.Hour), "clinic", "site"))
	write(turns("s4", 3, now.Add(-70*24*time.Hour), "clinic", "site"))
	path := filepath.Join(t.TempDir(), "families.json")
	f, err := RefreshFamilies(e.f.Attribution, path, now, nil)
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

// The thresholds hold at their bounds: exactly 5 turns of exactly 2
// sessions making exactly 20 % of the larger project's turns link; one
// turn, one session or one percent less does not.
func TestFamilyThresholdsAtTheirBounds(t *testing.T) {
	at := clockAt(9, 0)
	pair := func(coA, coB, alone int, sessB string) Families {
		all := append(turns("s1", coA, at, "app", "lib"), turns(sessB, coB, at, "lib", "app")...)
		return ComputeFamilies(append(all, turns("s3", alone, at, "app")...), nil)
	}
	for _, c := range []struct {
		name            string
		coA, coB, alone int
		sessB           string
		linked          bool
	}{
		{"5 turns, 2 sessions, 20 %", 3, 2, 20, "s2", true},
		{"4 turns", 2, 2, 16, "s2", false},
		{"one session", 3, 2, 20, "s1", false},
		{"under 20 % (5 of 26)", 3, 2, 21, "s2", false},
	} {
		f := pair(c.coA, c.coB, c.alone, c.sessB)
		if got := len(f.Of["app"]) == 1; got != c.linked {
			t.Errorf("%s: linked %v, want %v (%+v)", c.name, got, c.linked, f.Links)
		}
	}
}

// A project kept out of memory or forgotten since its turns were recorded
// is no member, whatever its co-touch.
func TestFamiliesLeaveOutProjectsExcludedOrForgottenSince(t *testing.T) {
	e := newEnv(t)
	e.attributed()
	secret := e.ownRepo("secret-plans", "")
	now := clockAt(12, 0)
	for _, sid := range []string{"s1", "s2"} {
		for _, x := range turns(sid, 3, now.Add(-time.Hour), "backsight", "secret-plans", "spec") {
			x.Agent = x.Turn
			x.Votes.Roots = map[string]string{"secret-plans": secret}
			if _, err := e.f.Attribution.keepAgent(x); err != nil {
				t.Fatal(err)
			}
		}
	}
	path := filepath.Join(t.TempDir(), "families.json")
	f, _ := RefreshFamilies(e.f.Attribution, path, now, nil)
	if !slices.Contains(f.Of["backsight"], "secret-plans") || f.Roots["secret-plans"] != secret {
		t.Fatalf("before: %+v", f)
	}
	e.f.Attribution.Exclude = []string{secret} // backsight pause --project, later
	f, _ = RefreshFamilies(e.f.Attribution, path, now, nil)
	if slices.Contains(f.Of["backsight"], "secret-plans") || len(f.Of["secret-plans"]) != 0 || !slices.Contains(f.Of["backsight"], "spec") {
		t.Errorf("after the exclusion: %+v", f.Of)
	}
	e.f.Attribution.Exclude = nil
	f, _ = RefreshFamilies(e.f.Attribution, path, now, func(p string) bool { return p != "spec" }) // forget --project spec
	if slices.Contains(f.Of["backsight"], "spec") || !slices.Contains(f.Of["backsight"], "secret-plans") {
		t.Errorf("after forgetting spec: %+v", f.Of)
	}
}
