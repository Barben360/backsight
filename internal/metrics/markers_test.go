package metrics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/telemetry"
)

func (x *fx) promptAt(at time.Time, version string) {
	x.t.Helper()
	x.binary = version
	x.tel(at, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "s", "alpha")
}

func (x *fx) configAt(at time.Time, sha string, values map[string]float64) {
	x.t.Helper()
	x.tel(at, telemetry.Config{SHA256: strings.Repeat(sha, 64), Values: values}, "", "")
}

var mk = func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, fxParis) }

// TestReleaseMarkers: v16 prompts, v17 at 14:02 on the 28th, a stray v16 at
// 15:00, then v17 again: every switch is a marker, in time order, each with
// the version seen just before; the first prompt ever is none; a switch
// before the window gives no marker but is the previous of the first one
// inside it.
func TestReleaseMarkers(t *testing.T) {
	x := newFx(t, fxNow)
	x.promptAt(mk(20, 9, 0), "0.1.15")
	x.promptAt(mk(26, 9, 0), "0.1.16") // a switch before the window
	x.promptAt(mk(27, 10, 0), "0.1.16")
	x.promptAt(mk(28, 9, 0), "0.1.16")
	x.promptAt(mk(28, 14, 2), "0.1.17")
	x.promptAt(mk(28, 14, 30), "0.1.17")
	x.promptAt(mk(28, 15, 0), "0.1.16")
	x.promptAt(mk(28, 15, 30), "0.1.17")
	r := x.run("releases", x.query(4)) // 26th 12:00 .. 30th 12:00
	got := r.Tables.Markers
	want := []Marker{
		{At: mk(28, 14, 2), Kind: "release", Version: "0.1.17", Previous: "0.1.16"},
		{At: mk(28, 15, 0), Kind: "release", Version: "0.1.16", Previous: "0.1.17"},
		{At: mk(28, 15, 30), Kind: "release", Version: "0.1.17", Previous: "0.1.16"},
	}
	if len(got) != len(want) {
		t.Fatalf("markers = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		if !got[i].At.Equal(w.At) || got[i].Kind != w.Kind || got[i].Version != w.Version || got[i].Previous != w.Previous {
			t.Errorf("marker %d = %+v, want %+v", i, got[i], w)
		}
	}
	// A window that starts after the 26th switch: its first prompt is not a marker.
	q := x.query(3) // 27th 12:00 ..
	if m := x.run("releases", q).Tables.Markers; len(m) != 3 || m[0].Previous != "0.1.16" {
		t.Errorf("mid-version window markers = %+v", m)
	}
	// The window ends before the switches: none, and never null.
	q = x.query(10)
	q.Window = x.window(10)
	all := x.run("releases", q).Tables.Markers
	if len(all) != 4 || all[0].Version != "0.1.16" || all[0].Previous != "0.1.15" {
		t.Errorf("ten-day markers = %+v, want the 26th switch first then three", all)
	}
	empty := newFx(t, fxNow)
	if m := empty.run("releases", empty.query(7)).Tables.Markers; m == nil || len(m) != 0 {
		t.Errorf("no prompts: markers = %v, want []", m)
	}
}

// TestConfigMarkers: three config rows sha a, a, b with recall.min_score
// 0.5, 0.5, 0.55 (and a key that is new in the third, one gone): one marker
// at the third row's time; the first row is none; ties are in file order.
func TestConfigMarkers(t *testing.T) {
	x := newFx(t, fxNow)
	x.configAt(mk(27, 9, 0), "a", map[string]float64{"recall.min_score": 0.5, "recall.old": 1})
	x.configAt(mk(28, 9, 0), "a", map[string]float64{"recall.min_score": 0.5, "recall.old": 1})
	x.configAt(mk(29, 9, 30), "b", map[string]float64{"recall.min_score": 0.55, "feedback.new": 2})
	r := x.run("releases", x.query(5))
	if len(r.Tables.Markers) != 1 {
		t.Fatalf("markers = %+v, want one", r.Tables.Markers)
	}
	m := r.Tables.Markers[0]
	if m.Kind != "config" || !m.At.Equal(mk(29, 9, 30)) || m.Version != "" || m.Previous != "" || len(m.Changes) != 3 {
		t.Fatalf("marker = %+v", m)
	}
	f := func(v float64) *float64 { return &v }
	want := []SettingChange{{"feedback.new", nil, f(2)}, {"recall.min_score", f(0.5), f(0.55)}, {"recall.old", f(1), nil}}
	for i, w := range want {
		c := m.Changes[i]
		if c.Key != w.Key || (c.From == nil) != (w.From == nil) || (c.To == nil) != (w.To == nil) ||
			(w.From != nil && *c.From != *w.From) || (w.To != nil && *c.To != *w.To) {
			t.Errorf("change %d = %+v, want %+v", i, c, w)
		}
	}
	raw, _ := json.Marshal(m)
	for _, s := range []string{`"kind":"config"`, `"from":null`, `"to":0.55`, `"at":`} {
		if !strings.Contains(string(raw), s) {
			t.Errorf("json %s lacks %s", raw, s)
		}
	}
	if strings.Contains(string(raw), `"version"`) {
		t.Errorf("a config marker names a version: %s", raw)
	}
	// A window starting after the second row still compares the third to it.
	q := x.query(2) // 28th 12:00 ..
	if m := x.run("releases", q).Tables.Markers; len(m) != 1 || len(m[0].Changes) != 3 {
		t.Errorf("late window markers = %+v", m)
	}
	// A window that holds only the first rows has none.
	q = x.query(5)
	q.Window = q.Window.span(mk(26, 0, 0), mk(29, 0, 0))
	if m := x.run("releases", q).Tables.Markers; len(m) != 0 {
		t.Errorf("early window markers = %+v, want none", m)
	}
}

// TestDayPageMarkers: the day page lists only that local day's markers,
// both kinds in time order; the day before the switch has none.
func TestDayPageMarkers(t *testing.T) {
	x := newFx(t, fxNow)
	x.promptAt(mk(27, 9, 0), "0.1.16")
	x.promptAt(mk(28, 9, 0), "0.1.17")
	x.promptAt(mk(29, 9, 0), "0.1.18")
	x.configAt(mk(27, 8, 0), "a", map[string]float64{"recall.k": 1})
	x.configAt(mk(28, 7, 0), "b", map[string]float64{"recall.k": 2})
	x.configAt(mk(29, 7, 0), "b", map[string]float64{"recall.k": 2})
	q := x.query(7)
	q.ID = "2026-10-28"
	d := x.run("days", q).Tables.Day
	if len(d.Markers) != 2 || d.Markers[0].Kind != "config" || d.Markers[1].Kind != "release" || d.Markers[1].Version != "0.1.17" || d.Markers[1].Previous != "0.1.16" {
		t.Fatalf("28th markers = %+v, want the config at 07:00 then 0.1.17", d.Markers)
	}
	q.ID = "2026-10-29"
	if d := x.run("days", q).Tables.Day; len(d.Markers) != 1 || d.Markers[0].Version != "0.1.18" {
		t.Errorf("29th markers = %+v, want only 0.1.18 (the unchanged config is none)", d.Markers)
	}
	q.ID = "2026-10-27"
	if d := x.run("days", q).Tables.Day; d.Markers == nil || len(d.Markers) != 0 {
		t.Errorf("27th markers = %v, want []", d.Markers)
	}
}

// TestReleasesCarryTheUsedSeries: the releases view returns used.turn_rate
// with one point per day of the window.
func TestReleasesCarryTheUsedSeries(t *testing.T) {
	x := newFx(t, fxNow)
	x.declaredTurn(mk(28, 10, 0), "a1", "declared", "used", "m1")
	x.declaredTurn(mk(28, 11, 0), "a2", "declared", "", "m2")
	x.declaredTurn(mk(29, 10, 0), "a3", "declared", "used", "m3")
	m, ok := x.run("releases", x.query(3)).Metrics["used.turn_rate"]
	if !ok {
		t.Fatal("no used.turn_rate in the releases view")
	}
	days := map[string]Point{}
	for _, p := range m.Series {
		days[p.Day] = p
	}
	if len(m.Series) != 4 || days["2026-10-28"].Den == nil || *days["2026-10-28"].Den != 2 || *days["2026-10-28"].Num != 1 ||
		days["2026-10-29"].Num == nil || *days["2026-10-29"].Num != 1 {
		t.Errorf("series = %+v, want 28th 1/2 and 29th 1/1 over 4 days", m.Series)
	}
}

// TestReleaseMarkerPreviousComesFromBeforeTheWindow: the first prompt of the
// window is itself the switch, its previous prompt is before since.
func TestReleaseMarkerPreviousComesFromBeforeTheWindow(t *testing.T) {
	x := newFx(t, fxNow)
	x.promptAt(mk(27, 10, 0), "0.1.16")
	x.promptAt(mk(28, 14, 2), "0.1.17")
	m := x.run("releases", x.query(2)).Tables.Markers // 28th 12:00 ..
	if len(m) != 1 || m[0].Version != "0.1.17" || m[0].Previous != "0.1.16" || !m[0].At.Equal(mk(28, 14, 2)) {
		t.Errorf("markers = %+v, want 0.1.17 after 0.1.16", m)
	}
}

// TestMarkersAtTheSameInstant: a release and a config marker recorded at the
// same instant list the release first.
func TestMarkersAtTheSameInstant(t *testing.T) {
	x := newFx(t, fxNow)
	x.promptAt(mk(27, 10, 0), "0.1.16")
	x.configAt(mk(27, 10, 0), "a", map[string]float64{"recall.k": 1})
	x.promptAt(mk(28, 9, 0), "0.1.17")
	x.configAt(mk(28, 9, 0), "b", map[string]float64{"recall.k": 2})
	m := x.run("releases", x.query(3)).Tables.Markers
	if len(m) != 2 || m[0].Kind != "release" || m[1].Kind != "config" || !m[0].At.Equal(m[1].At) {
		t.Errorf("markers = %+v, want release then config at one instant", m)
	}
}

// TestReleaseMarkersIgnoreProjectAndMachine: a switch made by a machine
// prompt of another project is a marker under a project filter.
func TestReleaseMarkersIgnoreProjectAndMachine(t *testing.T) {
	x := newFx(t, fxNow)
	x.binary = "0.1.16"
	x.tel(mk(27, 10, 0), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "s", "alpha")
	x.binary = "0.1.17"
	x.tel(mk(28, 14, 2), telemetry.Prompt{N: 1, Decision: "recall", Reason: "machine", Plen: 80}, "s2", "beta")
	q := x.query(3)
	q.Project = "alpha"
	m := x.run("releases", q).Tables.Markers
	if len(m) != 1 || m[0].Version != "0.1.17" || m[0].Previous != "0.1.16" {
		t.Errorf("markers = %+v, want the machine prompt of beta to count", m)
	}
}
