package metrics

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/metricsfile"
)

// The fixtures of the developer metrics on the bank. The default window
// starts 2026-10-23T11:00Z, so its UTC date is 2026-10-23: a memory created
// that day counts as new even when made before 11:00Z (day precision).

const dbShown = "bank.active_share"

// dbServer is a bank of total memories whose series adds perDay on each of
// the UTC days from..to (October).
func (x *fx) dbServer(total int, perDay map[int]int64) {
	x.t.Helper()
	x.server.set(func(d *fakeData) {
		d.Stats = hindsight.BankStats{NodesByFactType: map[string]int{"world": total}}
		d.Series = hindsight.Timeseries{Trunc: "day"}
		for _, day := range []int{20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30} {
			d.Series.Buckets = append(d.Series.Buckets, hindsight.SeriesBucket{Time: utcDay(day), World: perDay[day]})
		}
	})
}

// dbOld is n memories (prefix-1 ...) created long before the window, with
// a day-list row each, shown once inside it.
func (x *fx) dbOld(prefix string, n int) []string {
	x.t.Helper()
	var ids []string
	var recs []metricsfile.Record
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		ids = append(ids, id)
		recs = append(recs, metricsfile.MemoryDoc{Memory: id, Outcome: "found", DocKind: "session", Document: "conversation:s-" + id,
			Session: "s-" + id, CreatedDay: "2026-09-01", FactType: "world"})
	}
	x.mrec(rcBase, recs...)
	return ids
}

func (x *fx) dbShow(at time.Time, turn string, ids ...string) {
	x.t.Helper()
	x.shows(at, "dbs", turn, "", "alpha", "prompt", "", ids...)
}

func dbExtra(t *testing.T, m Metric, key string) int64 {
	t.Helper()
	v, ok := m.Extra[key].(int64)
	if !ok {
		t.Fatalf("extra.%s = %#v, want a count", key, m.Extra[key])
	}
	return v
}

// TestActiveShare: the bank held 1 260 now, 46 were created since the
// window began (the series' days from 2026-10-23 on; the day before does
// not count): 1 214. 418 distinct memories were shown, 6 of them created
// from the window's first date on (one that very date, before its time):
// 412 older ones.
func TestActiveShare(t *testing.T) {
	x := newFx(t, fxNow)
	x.need = BankServerNeeds()
	x.dbServer(1260, map[int]int64{22: 90, 23: 10, 24: 6, 25: 6, 26: 6, 27: 6, 28: 6, 29: 6})
	x.mrec(rcBase, metricsfile.Listed{Day: "2026-09-01", N: 3})
	old := x.dbOld("o", 412)
	var fresh []string
	for i, day := range []string{"2026-10-23", "2026-10-24", "2026-10-25", "2026-10-26", "2026-10-27", "2026-10-28"} {
		id := fmt.Sprintf("n-%d", i)
		fresh = append(fresh, id)
		x.mrec(rcBase, metricsfile.MemoryDoc{Memory: id, Outcome: "found", DocKind: "session", Document: "conversation:s-" + id,
			Session: "s-" + id, CreatedDay: day, FactType: "world"})
	}
	x.dbShow(fxNow.Add(-2*time.Hour), "t1", slices.Concat(old, fresh)...)
	x.dbShow(fxNow.Add(-time.Hour), "t2", old[0]) // shown twice: one memory
	// shown before the window only
	earlier := x.dbOld("e", 5)
	x.dbShow(fxNow.Add(-10*24*time.Hour), "t0", earlier...)

	m := x.metrics([]string{dbShown}, x.query(7))[dbShown]
	wantInt(t, "num", m.Num, 412)
	wantInt(t, "den", m.Den, 1214)
	wantFloat(t, "value", m.Value, 412.0/1214)
	lo, hi := Wilson(412, 1214)
	if m.CI == nil || m.CI[0] != lo || m.CI[1] != hi {
		t.Errorf("CI %v, want the Wilson interval %v..%v", m.CI, lo, hi)
	}
	if m.Status != StatusOK || len(m.Tags) != 0 {
		t.Errorf("status %q tags %v, want ok and no tag", m.Status, m.Tags)
	}
	if dbExtra(t, m, "unresolved") != 0 || dbExtra(t, m, "gone") != 0 {
		t.Errorf("unresolved %v gone %v, want 0 and 0", m.Extra["unresolved"], m.Extra["gone"])
	}
}

func TestActiveShareServerDown(t *testing.T) {
	x := newFx(t, fxNow)
	x.need = BankServerNeeds()
	x.server.failAll(hindsight.ErrUnreachable)
	x.dbShow(fxNow.Add(-time.Hour), "t1", x.dbOld("o", 3)...)
	m := x.metrics([]string{dbShown}, x.query(7))[dbShown]
	wantNoData(t, dbShown, m, ReasonServerDown)
}

// TestActiveShareUnresolved: shown memories without a day-list row. The
// record is complete up to 2026-10-28 (two days before the window's end):
// one first shown after that day may simply not be listed yet (counted,
// unresolved, tag partial); one first shown on or before it was forgotten
// before it was listed (gone: left out, no tag). Without a complete line
// every one is unresolved.
func TestActiveShareUnresolved(t *testing.T) {
	build := func(covered bool) *fx {
		x := newFx(t, fxNow)
		x.need = BankServerNeeds()
		x.dbServer(1000, nil)
		old := x.dbOld("o", 20)
		x.dbShow(fxNow.Add(-5*24*time.Hour), "t1", old...)
		if covered {
			coverage(x, "2026-09-01", "2026-10-28")
		}
		x.dbShow(time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC), "t2", "late-1", "late-2", "late-3") // after the days listed
		x.dbShow(time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC), "t3", "lost-1", "lost-2")           // within them
		return x
	}
	x := build(true)
	m := x.metrics([]string{dbShown}, x.query(7))[dbShown]
	wantInt(t, "num", m.Num, 23)
	wantInt(t, "den", m.Den, 1000)
	if dbExtra(t, m, "unresolved") != 3 || dbExtra(t, m, "gone") != 2 {
		t.Errorf("unresolved %v gone %v, want 3 and 2", m.Extra["unresolved"], m.Extra["gone"])
	}
	if !slices.Contains(m.Tags, tagPartial) {
		t.Errorf("tags %v, want partial", m.Tags)
	}

	y := build(false)
	m = y.metrics([]string{dbShown}, y.query(7))[dbShown]
	wantInt(t, "num without a complete line", m.Num, 25)
	if dbExtra(t, m, "unresolved") != 5 || dbExtra(t, m, "gone") != 0 {
		t.Errorf("unresolved %v gone %v, want 5 and 0", m.Extra["unresolved"], m.Extra["gone"])
	}

	// gone alone: no partial tag
	z := newFx(t, fxNow)
	z.need = BankServerNeeds()
	z.dbServer(1000, nil)
	z.dbShow(fxNow.Add(-5*24*time.Hour), "t1", z.dbOld("o", 20)...)
	coverage(z, "2026-09-01", "2026-10-28")
	z.dbShow(time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC), "t3", "lost-1")
	m = z.metrics([]string{dbShown}, z.query(7))[dbShown]
	if slices.Contains(m.Tags, tagPartial) || dbExtra(t, m, "gone") != 1 {
		t.Errorf("tags %v gone %v, want no partial and 1", m.Tags, m.Extra["gone"])
	}
}

// A memory listed twice is read from its later line: listed new first, then
// older (or the other way round), the later line decides.
func TestActiveShareReadsTheLaterLine(t *testing.T) {
	x := newFx(t, fxNow)
	x.need = BankServerNeeds()
	x.dbServer(1000, nil)
	x.mrec(rcBase, metricsfile.MemoryDoc{Memory: "twice", Outcome: "found", DocKind: "session", Document: "conversation:s-a",
		Session: "s-a", CreatedDay: "2026-10-26", FactType: "world"})
	x.mrec(rcBase.Add(time.Hour), metricsfile.MemoryDoc{Memory: "twice", Outcome: "found", DocKind: "session", Document: "conversation:s-a",
		Session: "s-a", CreatedDay: "2026-09-01", FactType: "world"})
	x.mrec(rcBase, metricsfile.MemoryDoc{Memory: "other", Outcome: "found", DocKind: "session", Document: "conversation:s-b",
		Session: "s-b", CreatedDay: "2026-09-01", FactType: "world"})
	x.mrec(rcBase.Add(time.Hour), metricsfile.MemoryDoc{Memory: "other", Outcome: "found", DocKind: "session", Document: "conversation:s-b",
		Session: "s-b", CreatedDay: "2026-10-26", FactType: "world"})
	x.dbShow(fxNow.Add(-time.Hour), "t1", "twice", "other")
	m := x.metrics([]string{dbShown}, x.query(7))[dbShown]
	wantInt(t, "num: only the memory whose later line is old", m.Num, 1)
}

// dbShowsOf shows ids[i] counts[i] times in the window, one line each.
func (x *fx) dbShowsOf(counts []int) {
	x.t.Helper()
	n := 0
	for i, c := range counts {
		for range c {
			n++
			x.dbShow(fxNow.Add(-time.Duration(n)*time.Minute), fmt.Sprintf("c%d", n), fmt.Sprintf("m%02d", i+1))
		}
	}
}

func TestConcentration(t *testing.T) {
	const id = "shows.concentration"
	// {1, 1, 2, 4}: 2·(1+2+6+16)/(4·8) − 5/4
	x := newFx(t, fxNow)
	x.dbShowsOf([]int{1, 1, 2, 4})
	m := x.metrics([]string{id}, x.query(7))[id]
	if m.Status != StatusTooFew {
		t.Errorf("status %q, want too_few (4 memories)", m.Status)
	}
	if g, _ := m.Extra["gini"].(float64); !close4(g, 0.3125) {
		t.Errorf("gini %v, want 0.3125", m.Extra["gini"])
	}

	// 25 memories shown 1, 2, ... 25 times: 2·5525/(25·325) − 26/25
	y := newFx(t, fxNow)
	var counts []int
	for i := 1; i <= 25; i++ {
		counts = append(counts, i)
	}
	y.dbShowsOf(counts)
	m = y.metrics([]string{id}, y.query(7))[id]
	if m.Status != StatusOK {
		t.Errorf("status %q, want ok (shown, not judged)", m.Status)
	}
	wantFloat(t, "value", m.Value, 0.32)
	if g, _ := m.Extra["gini"].(float64); !close4(g, 0.32) {
		t.Errorf("gini %v, want 0.32", m.Extra["gini"])
	}
	// the 10 most shown: 16..25 = 205 of 325
	if s, _ := m.Extra["top10_share"].(float64); !close4(s, 205.0/325) {
		t.Errorf("top10_share %v, want %v", m.Extra["top10_share"], 205.0/325)
	}
	if n := dbExtra(t, m, "shows"); n != 325 {
		t.Errorf("shows %d, want 325", n)
	}
}

func TestConcentrationEqualShows(t *testing.T) {
	const id = "shows.concentration"
	x := newFx(t, fxNow)
	x.dbShowsOf(slices.Repeat([]int{3}, 22))
	m := x.metrics([]string{id}, x.query(7))[id]
	wantFloat(t, "value when all are shown alike", m.Value, 0)
}

func TestConcentrationWithoutShows(t *testing.T) {
	const id = "shows.concentration"
	x := newFx(t, fxNow)
	m := x.metrics([]string{id}, x.query(7))[id]
	wantNoData(t, id, m, ReasonNoEvents)
}

func TestTopShown(t *testing.T) {
	x := newFx(t, fxNow)
	x.need = BankServerNeeds()
	var counts []int
	for i := 1; i <= 12; i++ { // m01 shown once ... m12 twelve times
		counts = append(counts, i)
	}
	x.dbShowsOf(counts)
	x.credit(fxNow.Add(-time.Hour), "cs", "", "alpha", "used", "m12")
	x.credit(fxNow.Add(-2*time.Hour), "cs", "", "alpha", "used", "m12", "m11")
	x.credit(fxNow.Add(-3*time.Hour), "cs", "", "alpha", "wrong", "m12")                  // not used
	x.credit(fxNow.Add(-30*24*time.Hour), "cs", "", "alpha", "used", "m12", "m11", "m10") // outside the window
	rows := x.run("bank", x.query(7)).Tables.TopShown
	if len(rows) != 10 {
		t.Fatalf("%d rows, want 10", len(rows))
	}
	if rows[0].Memory != "m12" || rows[0].Shown != 12 || rows[0].Used != 2 || rows[0].Project != "alpha" {
		t.Errorf("first row %+v, want m12 shown 12 used 2 in alpha", rows[0])
	}
	if rows[1].Memory != "m11" || rows[1].Used != 1 || rows[9].Memory != "m03" || rows[9].Shown != 3 {
		t.Errorf("rows %+v ... %+v, want m11 used 1 second and m03 shown 3 last", rows[1], rows[9])
	}
	// all shows: 1 + ... + 12 = 78
	if want := 12.0 / 78; !close4(rows[0].Share, want) {
		t.Errorf("share %v, want %v", rows[0].Share, want)
	}

	y := newFx(t, fxNow)
	y.need = BankServerNeeds()
	y.dbShowsOf([]int{2, 1, 1})
	rows = y.run("bank", y.query(7)).Tables.TopShown
	if len(rows) != 3 || rows[0].Memory != "m01" || rows[1].Memory != "m02" || rows[2].Memory != "m03" {
		t.Errorf("rows %+v, want the 3 memories, the most shown first and ties by id", rows)
	}
}
