package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/machines"
	"github.com/Barben360/backsight/internal/telemetry"
)

// A build named by its tag wrote "v0.1.27-m7" where a release writes
// "0.1.27-m7": every figure counts the two as one version, named without
// the "v".

// vOdd spells every odd event of a version with a leading "v".
func vOdd(v string, i int) string {
	if i%2 == 1 {
		return "v" + v
	}
	return v
}

// TestReleasesMergeBothSpellingsOfAVersion: releaseFx with half of each
// version's prompts and turns spelled "v…" gives the very rows and markers
// the plain fixture gives: one row per version, named without the "v",
// with the summed figures.
func TestReleasesMergeBothSpellingsOfAVersion(t *testing.T) {
	p := releaseFx(t)
	plain := p.run("releases", p.query(7))
	x := releaseFxSpelled(t, vOdd)
	r := x.run("releases", x.query(7))
	if len(r.Tables.Releases) != 3 {
		var names []string
		for _, row := range r.Tables.Releases {
			names = append(names, row.Version)
		}
		t.Fatalf("versions %v, want 0.1.16, 0.1.15 and 0.1.17 once each", names)
	}
	by := releaseByVersion(r)
	if v := by["0.1.16"]; v.N != 40 || num(v.Flagship["recall.hit_rate"]) != 10 || den(v.Flagship["recall.hit_rate"]) != 40 ||
		num(v.Flagship["used.turn_rate"]) != 5 || den(v.Flagship["used.turn_rate"]) != 20 {
		t.Errorf("0.1.16 = n %d, hit %d/%d, used %d/%d, want 40, 10/40, 5/20", v.N, num(v.Flagship["recall.hit_rate"]),
			den(v.Flagship["recall.hit_rate"]), num(v.Flagship["used.turn_rate"]), den(v.Flagship["used.turn_rate"]))
	}
	if !reflect.DeepEqual(r.Tables.Releases, plain.Tables.Releases) {
		t.Errorf("rows differ from the plain spelling's:\n got %+v\nwant %+v", r.Tables.Releases, plain.Tables.Releases)
	}
	if !reflect.DeepEqual(r.Tables.Markers, plain.Tables.Markers) {
		t.Errorf("markers = %+v, want the plain spelling's %+v", r.Tables.Markers, plain.Tables.Markers)
	}
}

// TestReleaseMarkersIgnoreTheSpelling: a switch from "v0.1.16" to
// "0.1.16" is no release, inside the window or across its start, and a
// marker names both versions without the "v".
func TestReleaseMarkersIgnoreTheSpelling(t *testing.T) {
	x := newFx(t, fxNow)
	x.promptAt(mk(25, 9, 0), "v0.1.16") // before the window
	x.promptAt(mk(27, 9, 0), "0.1.16")
	x.promptAt(mk(27, 10, 0), "v0.1.16")
	x.promptAt(mk(28, 9, 0), "v0.1.17")
	x.promptAt(mk(28, 10, 0), "0.1.17")
	got := x.run("releases", x.query(4)).Tables.Markers // 26th 12:00 .. 30th 12:00
	want := []Marker{{At: mk(28, 9, 0), Kind: "release", Version: "0.1.17", Previous: "0.1.16"}}
	if len(got) != 1 || !got[0].At.Equal(want[0].At) || got[0].Kind != want[0].Kind || got[0].Version != want[0].Version || got[0].Previous != want[0].Previous {
		t.Errorf("markers = %+v, want %+v", got, want)
	}
	// A window that opens on "v0.1.17" after "0.1.16": a marker, named without the v.
	y := newFx(t, fxNow)
	y.promptAt(mk(25, 9, 0), "0.1.16")
	y.promptAt(mk(28, 9, 0), "v0.1.17")
	got = y.run("releases", y.query(4)).Tables.Markers
	if len(got) != 1 || got[0].Version != "0.1.17" || got[0].Previous != "0.1.16" {
		t.Errorf("markers across the start = %+v, want 0.1.16 to 0.1.17", got)
	}
}

// TestCompareTakesEitherSpelling: --compare given "v0.1.16" or "0.1.16"
// (either side, or both) compares the same units, those of both
// spellings: on each measure the comparison equals the one over a plainly
// spelled index, and names the versions without the "v".
func TestCompareTakesEitherSpelling(t *testing.T) {
	const a, b = "0.1.16", "0.1.17"
	days := func(i int) time.Time { return time.Date(2026, 10, 12+i%8, 9+i/8, 0, 0, 0, fxParis) }
	hits := map[string]int{a: 12, b: 21}
	build := func(spell func(string, int) string) *fx {
		x := newFx(t, fxNow)
		for _, v := range []string{a, b} {
			for i := range 30 {
				x.binary = spell(v, i)
				hit := i < hits[v]
				inj, verdict := 0, "used"
				if hit {
					inj, verdict = 2, "wrong"
				}
				s := fmt.Sprintf("%s-%d", v, i)
				x.tel(days(i), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, Injected: inj}, s, "alpha")
				x.turnEvent(days(i).Add(-time.Second), s, "alpha")
				x.shows(days(i), s, fxTurn, "", "alpha", "prompt", "", "m1")
				x.credit(days(i).Add(30*time.Second), s, "", "alpha", verdict, "m1")
				x.stop(days(i).Add(time.Minute), s, "", "declared")
			}
		}
		return x
	}
	plain, spelled := build(func(v string, _ int) string { return v }), build(vOdd)
	for _, measure := range CompareMeasures {
		q := plain.query(30)
		q.Compare, q.Measure = [2]string{a, b}, measure
		want := plain.run("releases", q).Tables.Compare
		if want == nil || want.Reading != "differs" || len(want.Strata) != 1 || want.Strata[0].A.Den != 30 || want.Strata[0].B.Den != 30 {
			t.Fatalf("%s: plain comparison %+v, want 30 units a side that differ", measure, want)
		}
		for _, pair := range [][2]string{{a, b}, {"v" + a, b}, {a, "v" + b}, {"v" + a, "v" + b}} {
			q := spelled.query(30)
			q.Compare, q.Measure = pair, measure
			got := spelled.run("releases", q).Tables.Compare
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s, --compare %s,%s:\n got %+v\nwant %+v", measure, pair[0], pair[1], got, want)
			}
		}
	}
}

// TestReleasesMergeSpellingsAcrossPCs: this PC's "0.1.2" and "v0.1.2" and
// another PC's days of either spelling (both on one day) add up in one
// "0.1.2" row with all PCs, and in one row of this PC's alone without.
func TestReleasesMergeSpellingsAcrossPCs(t *testing.T) {
	x := newFx(t, fxNow)
	x.binary = "0.1.2"
	x.prompts("alpha", "a", fxNow.Add(-24*time.Hour), 2, injecting(1))
	x.binary = "v0.1.2"
	x.prompts("alpha", "b", fxNow.Add(-30*time.Hour), 3, injecting(2))
	x.pcDoc(pcOther1, "2026-09-10", fxNow.Add(-time.Hour), "")
	x.pcRows(
		pcRow{machine: pcOther1, day: "2026-10-28", version: "v0.1.2", f: machines.Figures{Prompts: 10, Hit: machines.Pair{3, 10}}},
		pcRow{machine: pcOther1, day: "2026-10-29", version: "0.1.2", f: machines.Figures{Prompts: 5, Hit: machines.Pair{1, 5}}},
		pcRow{machine: pcOther1, day: "2026-10-29", version: "v0.1.2", f: machines.Figures{Prompts: 4, Hit: machines.Pair{4, 4}}},
		pcRow{machine: pcOther1, day: "2026-10-28", f: machines.Figures{Prompts: 10, Hit: machines.Pair{3, 10}}},
		pcRow{machine: pcOther1, day: "2026-10-29", f: machines.Figures{Prompts: 9, Hit: machines.Pair{5, 9}}},
	)
	this := x.runAs("releases", x.query(7)).Tables.Releases
	if len(this) != 1 || this[0].Version != "0.1.2" || this[0].N != 5 ||
		num(this[0].Flagship["recall.hit_rate"]) != 3 || den(this[0].Flagship["recall.hit_rate"]) != 5 {
		t.Fatalf("this PC: %+v, want one 0.1.2 row of 5 prompts, hit 3/5", this)
	}
	all := x.runAs("releases", x.allQuery()).Tables.Releases
	if len(all) != 1 {
		t.Fatalf("all PCs: %d rows, want one 0.1.2", len(all))
	}
	v := all[0]
	if v.Version != "0.1.2" || v.N != 24 || num(v.Flagship["recall.hit_rate"]) != 11 || den(v.Flagship["recall.hit_rate"]) != 24 {
		t.Errorf("all PCs: %s n %d hit %d/%d, want 0.1.2, 24, 11/24", v.Version, v.N, num(v.Flagship["recall.hit_rate"]), den(v.Flagship["recall.hit_rate"]))
	}
	if want := time.Date(2026, 10, 28, 13, 0, 0, 0, fxParis); !v.First.Equal(want) {
		t.Errorf("all PCs: first %v, want the other PC's v0.1.2 day, %v", v.First, want)
	}
}

// TestDailyPublishesOneSpellingPerVersion: this PC's daily figures name a
// version once, without the "v", with both spellings' counts; "v" alone and
// "dev" are versions of their own.
func TestDailyPublishesOneSpellingPerVersion(t *testing.T) {
	x := newFx(t, dailyAt)
	x.binary = "v0.1.2"
	dailyPrompt(x, utc(1, 8), 1, "", map[string]any{"decision": "recall", "injected": 1})
	x.declaredTurn(utc(1, 10), "T1", "declared", "used", "m1")
	x.binary = "0.1.2"
	dailyPrompt(x, utc(1, 9), 2, "", map[string]any{"decision": "recall", "injected": 0, "empty": "no_candidates"})
	x.declaredTurn(utc(1, 11), "T2", "declared", "", "m2")
	x.binary = "v"
	dailyPrompt(x, utc(1, 12), 3, "", map[string]any{"decision": "recall", "injected": 1})
	x.binary = "dev"
	dailyPrompt(x, utc(1, 13), 4, "", map[string]any{"decision": "recall", "injected": 1})
	days := x.daily()
	if len(days) != 1 {
		t.Fatalf("days = %+v, want one", days)
	}
	vs := days[0].Versions
	if _, ok := vs["v0.1.2"]; ok || len(vs) != 3 {
		t.Fatalf("versions = %+v, want 0.1.2, v and dev", vs)
	}
	if f := vs["0.1.2"]; f.Prompts != 2 || f.Hit != (machines.Pair{1, 2}) || f.Used != (machines.Pair{1, 2}) || f.Coverage != (machines.Pair{2, 2}) {
		t.Errorf("0.1.2 = %+v, want 2 prompts, hit [1 2], used [1 2], coverage [2 2]", f)
	}
	for _, k := range []string{"v", "dev"} {
		if f := vs[k]; f.Prompts != 1 || f.Hit != (machines.Pair{1, 1}) {
			t.Errorf("%s = %+v, want its own prompt", k, f)
		}
	}
}

// TestCanonVersionSQLIsCanonVersion: the SQL that names a stored version
// gives what machines.CanonVersion gives.
func TestCanonVersionSQLIsCanonVersion(t *testing.T) {
	x := newFx(t, fxNow)
	x.source()
	for _, in := range []string{"v0.1.27-m7", "0.1.27-m7", "v1", "vv1.2", "v", "dev", "vdev", "V1.2", "", "v0.0.2-m0-93-gfd77ae6"} {
		var got string
		if err := x.ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(context.Background(), `WITH t(binary_version) AS (SELECT ?) SELECT `+canonVersionSQL("binary_version")+` FROM t`, in).Scan(&got)
		}); err != nil {
			t.Fatal(err)
		}
		if want := machines.CanonVersion(in); got != want {
			t.Errorf("SQL of %q = %q, want %q", in, got, want)
		}
	}
}
