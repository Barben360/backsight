package cli

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/index/indextest"
	"github.com/Barben360/backsight/internal/metricsfile"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/telemetry"
)

// mdAt is the memory-docs tests' clock.
var mdAt = time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)

// mdEnv is a state directory with its metrics index open, the HTTP fake
// and a read-only client of it.
type mdEnv struct {
	t  *testing.T
	d  paths.Dirs
	ix *index.Index
	ro *hindsight.ReadOnly
	at time.Time
}

func newMDEnv(t *testing.T) *mdEnv {
	t.Helper()
	port := fakeHindsight(t)
	fakeCreated = []fakeMem{}
	e := &mdEnv{t: t, d: paths.Dirs{State: filepath.Join(t.TempDir(), "state")}, at: mdAt}
	indextest.Seed(t, e.d) // at the head: the migrations are not what these tests read
	ix, err := index.Open(context.Background(), index.Options{Dirs: e.d, Home: t.TempDir(), Getenv: func(string) string { return "" },
		Migrations: indexRegistry(), Now: func() time.Time { return e.at }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	e.ix = ix
	ro, err := hindsight.NewReadOnly("http://127.0.0.1:"+strconv.Itoa(port), "k", "claude-memory", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	e.ro = ro
	return e
}

// run runs the step at at, the writer's clock at at too; the fake's
// query log starts empty.
func (e *mdEnv) run(at time.Time) string {
	e.t.Helper()
	e.at = at
	fakeListLog = nil
	w := metricsfile.New(e.d.MetricsDir(), "test")
	w.Now = func() time.Time { return at }
	return collectMemoryDocs(context.Background(), w, e.ix, e.ro, at)
}

// write appends records as an earlier run at at.
func (e *mdEnv) write(at time.Time, recs ...metricsfile.Record) {
	e.t.Helper()
	w := metricsfile.New(e.d.MetricsDir(), "test")
	w.Now = func() time.Time { return at }
	if err := w.Append(recs...); err != nil {
		e.t.Fatal(err)
	}
}

// listed records the days as listed by an earlier run, the first
// complete when complete is set.
func (e *mdEnv) listed(complete bool, days ...string) {
	e.t.Helper()
	var recs []metricsfile.Record
	for i, d := range days {
		recs = append(recs, metricsfile.Listed{Day: d, N: 0, Complete: complete && i == 0})
	}
	e.write(mdAt.Add(-48*time.Hour), recs...)
}

// listedRange is every day from 2026-10-20 to to listed, 2026-10-19
// listed complete when complete is set.
func (e *mdEnv) listedRange(to string, complete bool) {
	e.t.Helper()
	f := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	l, _ := time.Parse(time.DateOnly, to)
	var days []string
	if complete {
		days = append(days, f.AddDate(0, 0, -1).Format(time.DateOnly))
	}
	for d := f; !d.After(l); d = d.AddDate(0, 0, 1) {
		days = append(days, d.Format(time.DateOnly))
	}
	e.listed(complete, days...)
}

// sent records an ok send of doc at at, as this PC's telemetry does.
func (e *mdEnv) sent(doc string, at time.Time) {
	telemetry.New(e.d.State, telemetry.Common{BinaryVersion: "test"}).WithClock(func() time.Time { return at }).
		Emit(telemetry.Retain{Document: doc, Items: 1, Status: "ok", MS: 5})
}

// raw is every memory-docs file's content, in file name order.
func (e *mdEnv) raw() []byte {
	e.t.Helper()
	files, _ := filepath.Glob(filepath.Join(e.d.MetricsDir(), "memory-docs-*.jsonl"))
	sort.Strings(files)
	var b []byte
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			e.t.Fatal(err)
		}
		b = append(b, raw...)
	}
	return b
}

// lines is every memory-docs line, in file order.
func (e *mdEnv) lines() []map[string]any {
	e.t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(e.raw()))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			e.t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// mdKind is the lines of one kind, in file order.
func mdKind(lines []map[string]any, kind string) (out []map[string]any) {
	for _, l := range lines {
		if l["kind"] == kind {
			out = append(out, l)
		}
	}
	return out
}

// memoriesOf is the memory lines of a created day, with their positions.
func memoriesOf(lines []map[string]any, day string) (out []map[string]any, at []int) {
	for i, l := range lines {
		if l["kind"] == "memory" && l["created_day"] == day {
			out, at = append(out, l), append(at, i)
		}
	}
	return out, at
}

// listedAt is the position of each listed line by day (the last one).
func listedAt(lines []map[string]any) map[string]int {
	out := map[string]int{}
	for i, l := range lines {
		if l["kind"] == "listed" {
			out[l["day"].(string)] = i
		}
	}
	return out
}

// bankDay is n memories created over day, evenly spread, ids
// "<day>-<i>", each a world fact of the day's session document.
func bankDay(day string, n int) []fakeMem {
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		panic(err)
	}
	out := make([]fakeMem, n)
	for i := range out {
		out[i] = fakeMem{ID: fmt.Sprintf("%s-%06d", day, i), FactType: "world", Document: "conversation:s-" + day,
			Created: d.Add(time.Duration(i) * (24 * time.Hour / time.Duration(n)))}
	}
	return out
}

// setBank is the fake's bank: these memories, by creation time.
func setBank(days ...[]fakeMem) {
	var all []fakeMem
	for _, d := range days {
		all = append(all, d...)
	}
	slices.SortStableFunc(all, func(a, b fakeMem) int { return a.Created.Compare(b.Created) })
	fakeCreated = all
}

// mdQuery names a created-day listing: "check 2000" and "check 2099" (the
// filter checks), "probe <end>" (limit 1, no start), "day <D>@<offset>/<limit>"
// (a whole UTC day) or "window <start>/<end>@<offset>" (anything else).
func mdQuery(q url.Values) string {
	s, e, off, lim := q.Get("start_date"), q.Get("end_date"), q.Get("offset"), q.Get("limit")
	if q.Get("time_field") != "created_at" {
		return "not by creation: " + q.Encode()
	}
	switch {
	case s == "2000-01-01T00:00:00Z" && e == "2000-01-02T00:00:00Z" && lim == "1" && off == "0":
		return "check 2000"
	case s == "2099-01-01T00:00:00Z" && e == "" && lim == "1" && off == "0":
		return "check 2099"
	case s == "" && lim == "1" && off == "0":
		return "probe " + e
	}
	st, err1 := time.Parse(time.RFC3339, s)
	en, err2 := time.Parse(time.RFC3339, e)
	if err1 == nil && err2 == nil && st.Equal(utcDate(st)) && en.Equal(st.Add(24*time.Hour)) {
		return "day " + st.Format(time.DateOnly) + "@" + off + "/" + lim
	}
	return "window " + s + "/" + e + "@" + off
}

func mdQueries() []string {
	out := make([]string, 0, len(fakeListLog))
	for _, q := range fakeListLog {
		out = append(out, mdQuery(q))
	}
	return out
}

// dayQueries is the "day …" queries, sorted.
func dayQueries() []string {
	var out []string
	for _, q := range mdQueries() {
		if strings.HasPrefix(q, "day ") {
			out = append(out, q)
		}
	}
	return sorted(out)
}

func TestMemoryDocsListsDays(t *testing.T) {
	setup := func(t *testing.T) (*mdEnv, []fakeMem) {
		e := newMDEnv(t)
		e.listedRange("2026-10-26", true)
		d28 := bankDay("2026-10-28", 1203)
		d28[0].Document = "conversation:sess-1@backsight"
		d28[1].Document = "note:abc123"
		d28[2].Document = "import:docs/x.md"
		d28[3].Document, d28[3].FactType = "", "observation"
		setBank(bankDay("2026-10-25", 4), bankDay("2026-10-27", 2), d28, bankDay("2026-10-29", 3), bankDay("2026-10-30", 2))
		return e, d28
	}
	t.Run("past days", func(t *testing.T) {
		e, d28 := setup(t)
		if got, want := e.run(mdAt), "memory docs: 3 day(s) listed (1208 memories), every day listed, 0 operation(s), 0 unit count(s)"; got != want {
			t.Errorf("result %q, want %q", got, want)
		}
		want := []string{"day 2026-10-27@0/500", "day 2026-10-28@0/500", "day 2026-10-28@1000/500", "day 2026-10-28@500/500", "day 2026-10-29@0/500"}
		if got := dayQueries(); !slices.Equal(got, want) {
			t.Errorf("day queries %v, want %v", got, want)
		}
		if q := mdQueries(); len(q) != 2+len(want) {
			t.Errorf("queries %v, want the 2 checks and the days only (no probe: the record is complete)", q)
		}
		lines := e.lines()
		var order []string
		for _, l := range lines {
			if l["kind"] == "listed" && l["day"].(string) > "2026-10-26" {
				order = append(order, l["day"].(string))
				if l["day"] == "2026-10-28" && l["n"] != float64(1203) {
					t.Errorf("listed %v, want n 1203", l)
				}
			}
		}
		if !slices.Equal(order, []string{"2026-10-27", "2026-10-28", "2026-10-29"}) {
			t.Errorf("listed days in file order %v, want 27, 28, 29", order)
		}
		mem, at := memoriesOf(lines, "2026-10-28")
		ids := map[string]map[string]any{}
		for _, m := range mem {
			ids[m["memory"].(string)] = m
		}
		if len(mem) != 1203 || len(ids) != 1203 {
			t.Fatalf("%d memory lines of 2026-10-28 (%d ids), want 1203", len(mem), len(ids))
		}
		if pos := listedAt(lines)["2026-10-28"]; pos < at[len(at)-1] {
			t.Errorf("the day's listed line (line %d) comes before its last memory line (%d)", pos, at[len(at)-1])
		}
		check := func(id string, want map[string]any, absent ...string) {
			t.Helper()
			m := ids[id]
			for k, v := range want {
				if m[k] != v {
					t.Errorf("memory %s: %s = %v, want %v (%v)", id, k, m[k], v, m)
				}
			}
			for _, k := range absent {
				if _, ok := m[k]; ok {
					t.Errorf("memory %s has %s: %v", id, k, m)
				}
			}
		}
		check(d28[0].ID, map[string]any{"doc_kind": "session", "document": "conversation:sess-1@backsight", "session": "sess-1", "outcome": "found", "fact_type": "world", "created_day": "2026-10-28"})
		check(d28[1].ID, map[string]any{"doc_kind": "note"}, "document", "session")
		check(d28[2].ID, map[string]any{"doc_kind": "import"}, "document", "session")
		check(d28[3].ID, map[string]any{"doc_kind": "none", "fact_type": "observation"}, "document", "session")
		raw := e.raw()
		for _, s := range []string{"docs/x.md", "x.md", "canary"} {
			if bytes.Contains(raw, []byte(s)) {
				t.Errorf("the record holds %q", s)
			}
		}
		if _, at := memoriesOf(lines, "2026-10-30"); len(at) != 0 {
			t.Errorf("today's memories were listed")
		}

		// The same day again: the two checks only.
		if got, want := e.run(mdAt.Add(time.Hour)), "memory docs: 0 day(s) listed (0 memories), every day listed, 0 operation(s), 0 unit count(s)"; got != want {
			t.Errorf("second run %q, want %q", got, want)
		}
		if q := sorted(mdQueries()); !slices.Equal(q, []string{"check 2000", "check 2099"}) {
			t.Errorf("second run's queries %v, want the two checks only", q)
		}
	})
	t.Run("before 01:00 UTC", func(t *testing.T) {
		e, _ := setup(t)
		e.run(time.Date(2026, 10, 30, 0, 30, 0, 0, time.UTC))
		want := []string{"day 2026-10-27@0/500", "day 2026-10-28@0/500", "day 2026-10-28@1000/500", "day 2026-10-28@500/500"}
		if got := dayQueries(); !slices.Equal(got, want) {
			t.Errorf("day queries %v, want 27 and 28 only (29 waits for 01:00)", got)
		}
	})
}

func TestMemoryDocsBackfillStops(t *testing.T) {
	e := newMDEnv(t)
	setBank(bankDay("2026-10-25", 2), bankDay("2026-10-26", 1), bankDay("2026-10-28", 3), bankDay("2026-10-29", 2))
	got := e.run(mdAt)
	if want := "memory docs: 5 day(s) listed (8 memories), every day listed, 0 operation(s), 0 unit count(s)"; got != want {
		t.Errorf("result %q, want %q", got, want)
	}
	q := mdQueries()
	if len(q) != 9 {
		t.Fatalf("%d list calls %v, want 9", len(q), q)
	}
	if c := sorted(q[:2]); !slices.Equal(c, []string{"check 2000", "check 2099"}) {
		t.Errorf("first calls %v, want the checks", c)
	}
	if q[2] != "day 2026-10-29@0/500" || q[3] != "probe 2026-10-29T00:00:00Z" || q[8] != "probe 2026-10-25T00:00:00Z" {
		t.Errorf("calls %v: want day 29, the probe before 29, the batch, the probe before 25", q)
	}
	if b := sorted(q[4:8]); !slices.Equal(b, []string{"day 2026-10-25@0/500", "day 2026-10-26@0/500", "day 2026-10-27@0/500", "day 2026-10-28@0/500"}) {
		t.Errorf("batch %v, want 28, 27, 26, 25", b)
	}
	listed := mdKind(e.lines(), "listed")
	byDay := map[string]map[string]any{}
	for _, l := range listed {
		byDay[l["day"].(string)] = l
	}
	if len(listed) != 6 || byDay["2026-10-27"]["n"] != float64(0) || byDay["2026-10-28"]["n"] != float64(3) {
		t.Errorf("listed %v", listed)
	}
	if c := byDay["2026-10-24"]; c == nil || c["complete"] != true || c["n"] != float64(0) {
		t.Errorf("complete line %v, want {2026-10-24, n 0, complete true}", c)
	}
	for d, l := range byDay {
		if _, ok := l["complete"]; ok && d != "2026-10-24" {
			t.Errorf("%s written complete: %v", d, l)
		}
	}

	e.run(mdAt.Add(time.Hour))
	if q := sorted(mdQueries()); !slices.Equal(q, []string{"check 2000", "check 2099"}) {
		t.Errorf("second run's queries %v, want the checks only", q)
	}
	setBank(bankDay("2026-10-25", 2), bankDay("2026-10-26", 1), bankDay("2026-10-28", 3), bankDay("2026-10-29", 2), bankDay("2026-10-30", 2))
	next := time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC)
	if got, want := e.run(next), "memory docs: 1 day(s) listed (2 memories), every day listed, 0 operation(s), 0 unit count(s)"; got != want {
		t.Errorf("next day %q, want %q", got, want)
	}
	if q := sorted(mdQueries()); !slices.Equal(q, []string{"check 2000", "check 2099", "day 2026-10-30@0/500"}) {
		t.Errorf("next day's queries %v, want the checks and day 30", q)
	}
}

func TestMemoryDocsRedoesAPartDay(t *testing.T) {
	t.Run("a page fails", func(t *testing.T) {
		e := newMDEnv(t)
		e.listedRange("2026-10-26", true)
		setBank(bankDay("2026-10-27", 1203), bankDay("2026-10-28", 5))
		fail := true
		fakeListPage = func(q url.Values, page []fakeMem, total int) ([]fakeMem, int, int) {
			if fail && q.Get("start_date") == "2026-10-27T00:00:00Z" && q.Get("offset") == "1000" {
				return nil, 0, http.StatusInternalServerError
			}
			return page, total, 0
		}
		if got, want := e.run(mdAt), "memory docs: 2 day(s) listed (5 memories), 1 day(s) left for the next run, 0 operation(s), 0 unit count(s)"; got != want {
			t.Errorf("result %q, want %q", got, want)
		}
		lines := e.lines()
		if mem, _ := memoriesOf(lines, "2026-10-27"); len(mem) != 0 {
			t.Errorf("%d memory lines of the cut day", len(mem))
		}
		la := listedAt(lines)
		if _, ok := la["2026-10-27"]; ok {
			t.Error("the cut day is listed")
		}
		if _, ok := la["2026-10-28"]; !ok {
			t.Error("the other day is not listed")
		}

		fail = false
		if got, want := e.run(mdAt.Add(time.Hour)), "memory docs: 1 day(s) listed (1203 memories), every day listed, 0 operation(s), 0 unit count(s)"; got != want {
			t.Errorf("next run %q, want %q", got, want)
		}
		if q := dayQueries(); !slices.Equal(q, []string{"day 2026-10-27@0/500", "day 2026-10-27@1000/500", "day 2026-10-27@500/500"}) {
			t.Errorf("next run's day queries %v, want day 27 again only", q)
		}
		lines = e.lines()
		mem, _ := memoriesOf(lines, "2026-10-27")
		ids := map[any]bool{}
		for _, m := range mem {
			ids[m["memory"]] = true
		}
		if len(mem) != 1203 || len(ids) != 1203 {
			t.Errorf("%d memory lines of 27 (%d ids), want each of the 1203 once", len(mem), len(ids))
		}
		n := 0
		for _, l := range lines {
			if l["kind"] == "listed" && l["day"] == "2026-10-27" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d listed lines of 27, want 1", n)
		}
	})
	t.Run("repeated ids", func(t *testing.T) {
		e := newMDEnv(t)
		e.listedRange("2026-10-26", true)
		d27 := bankDay("2026-10-27", 1000)
		setBank(d27, bankDay("2026-10-28", 5))
		fakeListPage = func(q url.Values, page []fakeMem, total int) ([]fakeMem, int, int) {
			if q.Get("start_date") == "2026-10-27T00:00:00Z" && q.Get("offset") == "500" && len(page) > 0 {
				page = append([]fakeMem{d27[0]}, page[1:]...) // 999 distinct of 1 000
			}
			return page, total, 0
		}
		if got, want := e.run(mdAt), "memory docs: 2 day(s) listed (5 memories), 1 day(s) left for the next run, 0 operation(s), 0 unit count(s)"; got != want {
			t.Errorf("result %q, want %q", got, want)
		}
		lines := e.lines()
		if mem, _ := memoriesOf(lines, "2026-10-27"); len(mem) != 0 {
			t.Errorf("%d memory lines of a day read with repeats", len(mem))
		}
		if _, ok := listedAt(lines)["2026-10-27"]; ok {
			t.Error("a day read with repeats is listed")
		}
		e.run(mdAt.Add(time.Hour))
		if q := dayQueries(); !slices.Equal(q, []string{"day 2026-10-27@0/500", "day 2026-10-27@500/500"}) {
			t.Errorf("next run's day queries %v, want day 27 again", q)
		}
	})
}

// windowHour is the hour of the window a memory of bankDay(day, n) falls in.
func windowHour(t *testing.T, id string, n int) int {
	t.Helper()
	i, err := strconv.Atoi(id[strings.LastIndex(id, "-")+1:])
	if err != nil {
		t.Fatal(err)
	}
	return i * 24 / n
}

func TestMemoryDocsSplitsAHugeDay(t *testing.T) {
	t.Run("read last in hour windows", func(t *testing.T) {
		e := newMDEnv(t)
		e.listedRange("2026-10-26", true)
		e.listed(false, "2026-10-29")
		setBank(bankDay("2026-10-27", 60000), bankDay("2026-10-28", 10))
		if got, want := e.run(mdAt), "memory docs: 2 day(s) listed (60010 memories), every day listed, 0 operation(s), 0 unit count(s)"; got != want {
			t.Errorf("result %q, want %q", got, want)
		}
		q := mdQueries()
		windows := map[string]int{}
		for _, s := range q {
			if strings.HasPrefix(s, "window ") {
				w, off, _ := strings.Cut(strings.TrimPrefix(s, "window "), "@")
				windows[w]++
				if off != "0" && off != "500" && off != "1000" && off != "1500" && off != "2000" {
					t.Errorf("window query %s", s)
				}
			}
		}
		if len(windows) != 24 {
			t.Errorf("%d windows read, want 24: %v", len(windows), windows)
		}
		for h := range 24 {
			st := time.Date(2026, 10, 27, h, 0, 0, 0, time.UTC)
			w := st.Format(time.RFC3339) + "/" + st.Add(time.Hour).Format(time.RFC3339)
			if windows[w] != 5 {
				t.Errorf("window %s read in %d pages, want 5", w, windows[w])
			}
		}
		if dq := dayQueries(); !slices.Equal(dq, []string{"day 2026-10-27@0/500", "day 2026-10-28@0/500"}) {
			t.Errorf("day queries %v, want page 0 of 27 only, and 28", dq)
		}
		lines := e.lines()
		mem, at := memoriesOf(lines, "2026-10-27")
		if len(mem) != 60000 {
			t.Fatalf("%d memory lines of 27, want 60000", len(mem))
		}
		la := listedAt(lines)
		if la["2026-10-28"] > at[0] {
			t.Errorf("28 listed at line %d, after 27's first memory line (%d): 27 must go last", la["2026-10-28"], at[0])
		}
		if la["2026-10-27"] < at[len(at)-1] {
			t.Errorf("27 listed at line %d, before its last memory line %d", la["2026-10-27"], at[len(at)-1])
		}
		if l := lines[la["2026-10-27"]]; l["n"] != float64(60000) {
			t.Errorf("listed %v, want n 60000", l)
		}
		// Each window's lines are written at once: 24 runs of one hour.
		runs, last := 0, -1
		for _, m := range mem {
			if h := windowHour(t, m["memory"].(string), 60000); h != last {
				runs, last = runs+1, h
			}
		}
		if runs != 24 {
			t.Errorf("the day's lines come in %d runs of one window, want 24", runs)
		}
	})
	t.Run("too large for one run", func(t *testing.T) {
		e := newMDEnv(t)
		e.listedRange("2026-10-26", false)
		// 150 000 memories on 27: 10 in each hour up to 23:00, the rest in
		// its last hour, more pages than the run has calls left.
		d27 := make([]fakeMem, 0, 150000)
		for i := range 150000 {
			at := time.Date(2026, 10, 27, i/10, i%10, 0, 0, time.UTC)
			if i >= 230 {
				at = time.Date(2026, 10, 27, 23, 0, 0, 0, time.UTC).Add(time.Duration(i-230) * 20 * time.Millisecond)
			}
			d27 = append(d27, fakeMem{ID: fmt.Sprintf("2026-10-27-%06d", i), FactType: "world", Document: "conversation:s-27", Created: at})
		}
		setBank(bankDay("2026-10-19", 3), d27, bankDay("2026-10-28", 10), bankDay("2026-10-29", 5))
		// 28, 29 and the backfill's 19 to 16 are listed (15 complete); not
		// every day, while 27 is not.
		if got, want := e.run(mdAt), "memory docs: 6 day(s) listed (18 memories), day 2026-10-27 too large, 150000 memories, 0 operation(s), 0 unit count(s)"; got != want {
			t.Errorf("result %q, want %q", got, want)
		}
		// The last hour's 300 pages do not fit what is left: its page 0
		// only, no call spent on a window that cannot finish.
		var last []string
		for _, q := range mdQueries() {
			if strings.HasPrefix(q, "window 2026-10-27T23:00:00Z/") {
				last = append(last, q)
			}
		}
		if !slices.Equal(last, []string{"window 2026-10-27T23:00:00Z/2026-10-28T00:00:00Z@0"}) {
			t.Errorf("last hour's queries %v, want its page 0 only", last)
		}
		if n := fakeCalls["GET /memories/list"]; n != 35 {
			t.Errorf("%d list calls, want 35 (2 checks, 3 days, 2 probes, 4 older days, 24 windows)", n)
		}
		lines := e.lines()
		mem, at := memoriesOf(lines, "2026-10-27")
		la := listedAt(lines)
		if _, ok := la["2026-10-27"]; ok {
			t.Error("the day too large is listed")
		}
		if len(mem) != 230 {
			t.Errorf("%d memory lines of 27, want the 230 of its 23 windows read whole", len(mem))
		}
		for _, d := range []string{"2026-10-28", "2026-10-29", "2026-10-19", "2026-10-18", "2026-10-17", "2026-10-16", "2026-10-15"} {
			pos, ok := la[d]
			if !ok || len(at) > 0 && pos > at[0] {
				t.Errorf("%s listed %v at line %d, want before the day too large (%v)", d, ok, pos, at[:min(1, len(at))])
			}
		}
		if c := lines[la["2026-10-15"]]; c["complete"] != true {
			t.Errorf("backfill's end %v, want complete", c)
		}

		e.run(mdAt.Add(time.Hour))
		if dq := dayQueries(); !slices.Contains(dq, "day 2026-10-27@0/500") {
			t.Errorf("next run's day queries %v, want 27 again", dq)
		}
		mem, _ = memoriesOf(e.lines(), "2026-10-27")
		ids := map[string]bool{}
		for _, m := range mem {
			ids[m["memory"].(string)] = true
		}
		if _, err := e.ix.CatchUp(context.Background(), 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		var rows, distinct int
		if err := e.ix.ReadTx(context.Background(), func(tx *sql.Tx) error {
			return tx.QueryRow(`SELECT count(*), count(DISTINCT memory_id) FROM memory_doc WHERE created_day = '2026-10-27'`).Scan(&rows, &distinct)
		}); err != nil {
			t.Fatal(err)
		}
		if rows != 230 || distinct != 230 || len(ids) != 230 || len(mem) != 460 {
			t.Errorf("memory_doc %d rows (%d distinct) for %d ids in %d lines, want one row per memory", rows, distinct, len(ids), len(mem))
		}
	})
	t.Run("more pages than the run's budget", func(t *testing.T) {
		// 25 000 memories, under 50 000, but in pages of 100: 250 pages.
		e := newMDEnv(t)
		e.listedRange("2026-10-26", true)
		fakeListCap = 100
		setBank(bankDay("2026-10-27", 25000), bankDay("2026-10-28", 10))
		got := e.run(mdAt)
		if !strings.Contains(got, ", day 2026-10-27 too large, 25000 memories, ") {
			t.Errorf("result %q, want 27 too large", got)
		}
		lines := e.lines()
		_, at := memoriesOf(lines, "2026-10-27")
		la := listedAt(lines)
		if pos, ok := la["2026-10-28"]; !ok || len(at) > 0 && pos > at[0] {
			t.Errorf("28 listed %v at %d, want before 27's lines", ok, pos)
		}
		if _, ok := la["2026-10-27"]; ok {
			t.Error("27 listed")
		}
	})
}

func TestMemoryDocsFollowsTheServersPageSize(t *testing.T) {
	t.Run("pages of 100", func(t *testing.T) {
		e := newMDEnv(t)
		e.listedRange("2026-10-26", true)
		e.listed(false, "2026-10-28", "2026-10-29")
		fakeListCap = 100
		setBank(bankDay("2026-10-27", 1203))
		if got, want := e.run(mdAt), "memory docs: 1 day(s) listed (1203 memories), every day listed, 0 operation(s), 0 unit count(s)"; got != want {
			t.Errorf("result %q, want %q", got, want)
		}
		var want []string
		for off := 0; off <= 1200; off += 100 {
			want = append(want, "day 2026-10-27@"+strconv.Itoa(off)+"/500")
		}
		if got := dayQueries(); !slices.Equal(got, sorted(want)) {
			t.Errorf("day queries %v, want %v", got, want)
		}
		lines := e.lines()
		if mem, _ := memoriesOf(lines, "2026-10-27"); len(mem) != 1203 {
			t.Errorf("%d memory lines, want 1203", len(mem))
		}
		if pos, ok := listedAt(lines)["2026-10-27"]; !ok || lines[pos]["n"] != float64(1203) {
			t.Errorf("listed %v", ok)
		}
	})
	t.Run("an empty page before the total", func(t *testing.T) {
		e := newMDEnv(t)
		e.listedRange("2026-10-26", true)
		e.listed(false, "2026-10-28", "2026-10-29")
		fakeListCap = 100
		setBank(bankDay("2026-10-27", 1203))
		fakeListPage = func(q url.Values, page []fakeMem, total int) ([]fakeMem, int, int) {
			if q.Get("offset") == "500" {
				return nil, total, 0
			}
			return page, total, 0
		}
		if got, want := e.run(mdAt), "memory docs: 0 day(s) listed (0 memories), 1 day(s) left for the next run, 0 operation(s), 0 unit count(s)"; got != want {
			t.Errorf("result %q, want %q", got, want)
		}
		lines := e.lines()
		if mem, _ := memoriesOf(lines, "2026-10-27"); len(mem) != 0 {
			t.Errorf("%d memory lines of a day read in part", len(mem))
		}
		if _, ok := listedAt(lines)["2026-10-27"]; ok {
			t.Error("a day read in part is listed")
		}
		if q := dayQueries(); len(q) != 6 {
			t.Errorf("day queries %v, want 6 (up to the empty page)", q)
		}
	})
}

// fakeOp is a retain operation of doc, as /operations lists it.
func fakeOp(id, status, doc string, retries int) string {
	return fmt.Sprintf(`{"id":%q,"operation_id":%q,"operation_type":"retain","task_type":"retain","status":%q,"document_id":%q,"created_at":"2026-10-29T10:00:00Z","updated_at":"2026-10-29T10:02:00Z","retry_count":%d,"items_count":1,"error_message":"canary error text"}`, id, id, status, doc, retries)
}

func TestMemoryDocsIsBounded(t *testing.T) {
	e := newMDEnv(t)
	var bank [][]fakeMem
	for i := 1; i <= 300; i++ {
		bank = append(bank, bankDay(mdAt.AddDate(0, 0, -i).Format(time.DateOnly), 600))
	}
	setBank(bank...)
	for i := range 300 {
		doc := fmt.Sprintf("conversation:b-%03d", i)
		e.sent(doc, mdAt.Add(-24*time.Hour))
		fakeOperations = append(fakeOperations, fakeOp(fmt.Sprintf("op-%03d", i), "completed", doc, 0))
		documentUnits[doc] = 2
	}
	fakeOverlap.Store(true)
	got := e.run(mdAt)
	if n := fakeCalls["GET /memories/list"]; n != 200 {
		t.Errorf("%d list calls, want 200", n)
	}
	if n := fakeCalls["GET /documents/{id}"]; n != 200 {
		t.Errorf("%d document reads, want 200", n)
	}
	if m := fakeMaxInFlight.Load(); m != 4 {
		t.Errorf("at most %d requests in flight, want 4", m)
	}
	if !strings.Contains(got, ", older days on the next run, ") || !strings.HasSuffix(got, ", 300 operation(s), 200 unit count(s), 100 unit count(s) left for the next run") {
		t.Errorf("result %q", got)
	}
	lines := e.lines()
	per := map[string]int{}
	for _, l := range lines {
		if l["kind"] == "memory" {
			per[l["created_day"].(string)]++
		}
	}
	la := listedAt(lines)
	if len(la) < 80 {
		t.Errorf("%d days listed, want the budget's worth (about 86)", len(la))
	}
	for d := range la {
		if per[d] != 600 {
			t.Errorf("%s listed with %d memory lines, want 600", d, per[d])
		}
		if c, ok := lines[la[d]]["complete"]; ok {
			t.Errorf("%s complete (%v) though older days remain", d, c)
		}
	}
	for d, n := range per {
		if _, ok := la[d]; !ok {
			t.Errorf("%s has %d memory lines but is not listed: a day is written whole or not at all", d, n)
		}
	}
	ops := mdKind(lines, "op")
	units := 0
	for _, o := range ops {
		if _, ok := o["units"]; ok {
			units++
		}
	}
	if len(ops) != 300 || units != 200 {
		t.Errorf("%d op lines, %d with units; want 300 and 200", len(ops), units)
	}
}

func TestOpsOnlyForThisPCsRecentSends(t *testing.T) {
	e := newMDEnv(t)
	e.listedRange("2026-10-29", true)
	e.sent("conversation:recent", mdAt.Add(-3*24*time.Hour))
	e.sent("conversation:old", mdAt.Add(-10*24*time.Hour))
	e.sent("conversation:moved", mdAt.Add(-24*time.Hour))
	e.sent("conversation:broke", mdAt.Add(-2*24*time.Hour))
	e.write(mdAt.Add(-24*time.Hour), metricsfile.DocOp{Operation: "op-moved", Document: "conversation:moved", Session: "moved", Status: "pending",
		CreatedAt: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)})
	fakeOperations = []string{
		fakeOp("op-recent", "completed", "conversation:recent", 0),
		fakeOp("op-old", "completed", "conversation:old", 0),
		fakeOp("op-other", "completed", "conversation:otherpc", 0),
		fakeOp("op-moved", "completed", "conversation:moved", 0),
		fakeOp("op-broke", "failed", "conversation:broke", 2),
	}
	documentUnits["conversation:recent"], documentUnits["conversation:old"], documentUnits["conversation:otherpc"], documentUnits["conversation:moved"] = 4, 9, 7, 0
	if got, want := e.run(mdAt), "memory docs: 0 day(s) listed (0 memories), every day listed, 3 operation(s), 2 unit count(s)"; got != want {
		t.Errorf("result %q, want %q", got, want)
	}
	if n := fakeCalls["GET /documents/{id}"]; n != 2 {
		t.Errorf("%d document reads, want 2 (the completed operations of this PC's recent sends)", n)
	}
	ops := mdKind(e.lines(), "op")
	byOp := map[string][]map[string]any{}
	for _, o := range ops {
		byOp[o["operation"].(string)] = append(byOp[o["operation"].(string)], o)
	}
	if len(byOp) != 3 || byOp["op-old"] != nil || byOp["op-other"] != nil {
		t.Errorf("operations %v, want op-recent, op-moved, op-broke", byOp)
	}
	if r := byOp["op-recent"]; len(r) != 1 || r[0]["status"] != "completed" || r[0]["units"] != float64(4) || r[0]["session"] != "recent" ||
		r[0]["document"] != "conversation:recent" || r[0]["created_at"] != "2026-10-29T10:00:00Z" || r[0]["updated_at"] != "2026-10-29T10:02:00Z" || r[0]["retries"] != float64(0) {
		t.Errorf("op-recent %v", r)
	}
	if m := byOp["op-moved"]; len(m) != 2 || m[0]["status"] != "pending" || m[1]["status"] != "completed" || m[1]["units"] != float64(0) {
		t.Errorf("op-moved %v, want its pending line and a completed one with 0 units", m)
	}
	if b := byOp["op-broke"]; len(b) != 1 || b[0]["status"] != "failed" || b[0]["retries"] != float64(2) {
		t.Errorf("op-broke %v", b)
	} else if _, ok := b[0]["units"]; ok {
		t.Errorf("a failed operation has units: %v", b)
	}
	if bytes.Contains(e.raw(), []byte("canary")) {
		t.Error("the record holds an error text")
	}

	// The same statuses again: no line, no read.
	if got, want := e.run(mdAt.Add(time.Hour)), "memory docs: 0 day(s) listed (0 memories), every day listed, 0 operation(s), 0 unit count(s)"; got != want {
		t.Errorf("second run %q, want %q", got, want)
	}
	if again := mdKind(e.lines(), "op"); len(again) != len(ops) {
		t.Errorf("%d op lines after the second run, want %d", len(again), len(ops))
	}
	if n := fakeCalls["GET /documents/{id}"]; n != 2 {
		t.Errorf("%d document reads after the second run, want still 2", n)
	}
}

// blockingMD is a reader whose every call waits for its context.
type blockingMD struct{}

func (blockingMD) ListCreatedMemories(ctx context.Context, _, _ time.Time, _, _ int) ([]hindsight.ListedMemory, int, error) {
	<-ctx.Done()
	return nil, 0, ctx.Err()
}

func (blockingMD) RetainOperations(ctx context.Context, _ string, _ int) ([]hindsight.Operation, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingMD) DocumentUnits(ctx context.Context, _ string) (int, bool, error) {
	<-ctx.Done()
	return 0, false, ctx.Err()
}

func TestMemoryDocsSurvivesOddServers(t *testing.T) {
	withOp := func(e *mdEnv, units bool) {
		e.sent("conversation:s1", mdAt.Add(-24*time.Hour))
		fakeOperations = []string{fakeOp("op-1", "completed", "conversation:s1", 0)}
		if units {
			documentUnits["conversation:s1"] = 3
		} else {
			documentNoUnits["conversation:s1"] = true
		}
	}
	for _, c := range []struct{ name, filter string }{{"filter ignored", "none"}, {"start_date ignored", "start"}} {
		t.Run(c.name, func(t *testing.T) {
			e := newMDEnv(t)
			withOp(e, true)
			setBank(bankDay("2026-10-21", 11358))
			fakeListFilter = c.filter
			if got, want := e.run(mdAt), "memory docs: the server cannot list memories by creation day, 1 operation(s), 1 unit count(s)"; got != want {
				t.Errorf("result %q, want %q", got, want)
			}
			if q := sorted(mdQueries()); !slices.Equal(q, []string{"check 2000", "check 2099"}) {
				t.Errorf("queries %v, want the checks only", q)
			}
			lines := e.lines()
			for _, k := range []string{"memory", "listed"} {
				if l := mdKind(lines, k); len(l) != 0 {
					t.Errorf("%d %s line(s) from a server that ignores the filter", len(l), k)
				}
			}
			if ops := mdKind(lines, "op"); len(ops) != 1 {
				t.Errorf("%d op lines, want 1", len(ops))
			}
		})
	}
	t.Run("no created-day listing", func(t *testing.T) {
		e := newMDEnv(t)
		withOp(e, true)
		fakeListStatus = http.StatusNotFound
		if got, want := e.run(mdAt), "memory docs: days not listed (not_found), 1 operation(s), 1 unit count(s)"; got != want {
			t.Errorf("result %q, want %q", got, want)
		}
		if l := mdKind(e.lines(), "listed"); len(l) != 0 {
			t.Errorf("listed lines %v", l)
		}
	})
	t.Run("odd fields", func(t *testing.T) {
		e := newMDEnv(t)
		e.listedRange("2026-10-26", true)
		withOp(e, false)
		d := bankDay("2026-10-27", 3)
		d[0].FactType = ""
		d[1].FactType = "opinion"
		d[2].FactType, d[2].Document = "<null>", "<null>"
		setBank(d)
		if got, want := e.run(mdAt), "memory docs: 3 day(s) listed (3 memories), every day listed, 1 operation(s), 0 unit count(s)"; got != want {
			t.Errorf("result %q, want %q", got, want)
		}
		lines := e.lines()
		mem, _ := memoriesOf(lines, "2026-10-27")
		if len(mem) != 3 {
			t.Fatalf("memory lines %v", mem)
		}
		for _, m := range mem {
			if m["fact_type"] != "other" {
				t.Errorf("memory %v, want fact_type other", m)
			}
		}
		if mem[2]["doc_kind"] != "none" {
			t.Errorf("memory with a null document %v, want doc_kind none", mem[2])
		}
		ops := mdKind(lines, "op")
		if len(ops) != 1 {
			t.Fatalf("op lines %v", ops)
		}
		if _, ok := ops[0]["units"]; ok || ops[0]["no_units"] != true {
			t.Errorf("op %v, want no units and no_units true (the server gave no count)", ops[0])
		}
		if bytes.Contains(e.raw(), []byte("canary")) {
			t.Error("the record holds a text")
		}
		// A count the server does not give is not asked again.
		if got, want := e.run(mdAt.Add(time.Hour)), "memory docs: 0 day(s) listed (0 memories), every day listed, 0 operation(s), 0 unit count(s)"; got != want {
			t.Errorf("second run %q, want %q", got, want)
		}
		if n := fakeCalls["GET /documents/{id}"]; n != 1 {
			t.Errorf("%d document reads over two runs, want 1", n)
		}
		if ops := mdKind(e.lines(), "op"); len(ops) != 1 {
			t.Errorf("op lines after the second run %v, want 1", ops)
		}
	})
	t.Run("every call times out", func(t *testing.T) {
		old := memoryDocsStepTimeout
		memoryDocsStepTimeout = 300 * time.Millisecond
		t.Cleanup(func() { memoryDocsStepTimeout = old })
		e := newMDEnv(t)
		e.sent("conversation:s1", mdAt.Add(-24*time.Hour))
		w := metricsfile.New(e.d.MetricsDir(), "test")
		w.Now = func() time.Time { return mdAt }
		start := time.Now()
		got := collectMemoryDocs(context.Background(), w, e.ix, blockingMD{}, mdAt)
		if el := time.Since(start); el > 2*time.Second {
			t.Errorf("step took %v, want about its 300 ms bound", el)
		}
		if want := "memory docs: days not listed (timeout), operations not read (timeout)"; got != want {
			t.Errorf("result %q, want %q", got, want)
		}
	})
	t.Run("every call times out in the job", func(t *testing.T) {
		old, oldSD, oldReader := memoryDocsStepTimeout, serverDailyReaderFor, memoryDocsReaderFor
		memoryDocsStepTimeout = 300 * time.Millisecond
		serverDailyReaderFor = func() (serverDailyReader, error) { return nil, nil }
		memoryDocsReaderFor = func() (memoryDocsReader, error) { return blockingMD{}, nil }
		t.Cleanup(func() { memoryDocsStepTimeout, serverDailyReaderFor, memoryDocsReaderFor = old, oldSD, oldReader })
		home := maintainWithoutBackup(t)
		fixedMaintainDoctor(t, doctor.Check{ID: "a", Status: doctor.OK})
		withClock(t, mdAt)
		ix, err := index.Open(context.Background(), indexOptions(pathsOf(home), home, nil)) // as a tick builds it
		if err != nil {
			t.Fatal(err)
		}
		_ = ix.Close()
		code, out, _ := run(t, "jobs", "maintain", "--json")
		if code != output.OK || !strings.Contains(out, `"status": "done"`) || !strings.Contains(out, `"memory_docs": "memory docs: days not listed (timeout), operations not read (timeout)"`) {
			t.Errorf("exit %d\n%s", code, out)
		}
	})
}

// Through the real read-only client, only GET requests reach the server.
func TestMemoryDocsUsesGETOnly(t *testing.T) {
	e := newMDEnv(t)
	var mu sync.Mutex
	var seen []string
	fakeHook = func(r *http.Request) {
		mu.Lock()
		seen = append(seen, fakeEndpoint(r))
		mu.Unlock()
	}
	e.sent("conversation:s1", mdAt.Add(-24*time.Hour))
	fakeOperations = []string{fakeOp("op-1", "completed", "conversation:s1", 0)}
	documentUnits["conversation:s1"] = 3
	setBank(bankDay("2026-10-28", 2), bankDay("2026-10-29", 3))
	if got, want := e.run(mdAt), "memory docs: 5 day(s) listed (5 memories), every day listed, 1 operation(s), 1 unit count(s)"; got != want {
		t.Errorf("result %q, want %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	endpoints := map[string]bool{}
	for _, s := range seen {
		if !strings.HasPrefix(s, "GET ") {
			t.Errorf("%s reached the server", s)
		}
		endpoints[s] = true
	}
	for _, p := range []string{"GET /memories/list", "GET /operations", "GET /documents/{id}"} {
		if !endpoints[p] {
			t.Errorf("%s not read (read: %v)", p, endpoints)
		}
	}
}

// The daily job runs the step and reports its line; it never creates the
// metrics index (ticks build it): without one the step waits.
func TestMaintainRunsMemoryDocs(t *testing.T) {
	home := maintainWithoutBackup(t)
	fixedMaintainDoctor(t, doctor.Check{ID: "a", Status: doctor.OK})
	oldSD, oldReader := serverDailyReaderFor, memoryDocsReaderFor
	serverDailyReaderFor = func() (serverDailyReader, error) { return nil, nil }
	var reader memoryDocsReader
	memoryDocsReaderFor = func() (memoryDocsReader, error) { return reader, nil }
	t.Cleanup(func() { serverDailyReaderFor, memoryDocsReaderFor = oldSD, oldReader })
	withClock(t, mdAt)
	code, out, _ := run(t, "jobs", "maintain")
	if code != output.OK || !strings.Contains(out, ", memory docs: no server set up") {
		t.Errorf("exit %d\n%s", code, out)
	}
	reader = blockingMD{}
	code, out, _ = run(t, "jobs", "maintain")
	if code != output.OK || !strings.Contains(out, ", memory docs: not run, no metrics index yet") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(pathsOf(home).IndexDir()); !os.IsNotExist(err) {
		t.Errorf("maintain created the index: %v", err)
	}
}

// Days read at once share the run's calls: each reserves its pages after
// page 0, oldest first, so days that do not all fit are not cut in step
// (none would be whole): the oldest that fit are written, the rest wait.
func TestMemoryDocsDoesNotStarveDays(t *testing.T) {
	e := newMDEnv(t)
	e.listedRange("2026-10-25", true)
	fakeListCap = 100 // 60 pages a day
	setBank(bankDay("2026-10-26", 6000), bankDay("2026-10-27", 6000), bankDay("2026-10-28", 6000), bankDay("2026-10-29", 6000))
	if got, want := e.run(mdAt), "memory docs: 3 day(s) listed (18000 memories), 1 day(s) left for the next run, 0 operation(s), 0 unit count(s)"; got != want {
		t.Errorf("result %q, want %q", got, want)
	}
	lines := e.lines()
	la := listedAt(lines)
	for _, d := range []string{"2026-10-26", "2026-10-27", "2026-10-28"} {
		if mem, _ := memoriesOf(lines, d); len(mem) != 6000 {
			t.Errorf("%s: %d memory lines, want 6000", d, len(mem))
		}
		if _, ok := la[d]; !ok {
			t.Errorf("%s not listed", d)
		}
	}
	if mem, _ := memoriesOf(lines, "2026-10-29"); len(mem) != 0 {
		t.Errorf("%d memory lines of the day left", len(mem))
	}
	if q := dayQueries(); slices.Contains(q, "day 2026-10-29@100/500") {
		t.Errorf("the day left was read past page 0: %v", q)
	}
	if got, want := e.run(mdAt.Add(time.Hour)), "memory docs: 1 day(s) listed (6000 memories), every day listed, 0 operation(s), 0 unit count(s)"; got != want {
		t.Errorf("next run %q, want %q", got, want)
	}
	if mem, _ := memoriesOf(e.lines(), "2026-10-29"); len(mem) != 6000 {
		t.Errorf("next run: %d memory lines of 29, want 6000", len(mem))
	}
}

// mdIndex is the metrics index with its catch-up, or its reads of the
// sends and operations, answering what the test sets.
type mdIndex struct {
	*index.Index
	fresh            *index.Freshness // the catch-up's answer, when set
	catchErr         error
	sentErr, recsErr error
}

func (x mdIndex) CatchUp(ctx context.Context, b time.Duration) (index.Freshness, error) {
	if x.fresh != nil || x.catchErr != nil {
		var f index.Freshness
		if x.fresh != nil {
			f = *x.fresh
		}
		return f, x.catchErr
	}
	return x.Index.CatchUp(ctx, b)
}

func (x mdIndex) SentDocuments(ctx context.Context, since time.Time) ([]index.SentDocument, error) {
	if x.sentErr != nil {
		return nil, x.sentErr
	}
	return x.Index.SentDocuments(ctx, since)
}

func (x mdIndex) RecordedOperations(ctx context.Context) (map[string]index.RecordedOp, error) {
	if x.recsErr != nil {
		return nil, x.recsErr
	}
	return x.Index.RecordedOperations(ctx)
}

// An index behind its files would have the run list again the days it
// holds: no day is read, the operations still run. An index that cannot
// say which sends or operations it holds says so, never "0 operation(s)".
func TestMemoryDocsWaitsForTheIndex(t *testing.T) {
	setup := func(t *testing.T) *mdEnv {
		e := newMDEnv(t)
		e.listedRange("2026-10-28", true)
		setBank(bankDay("2026-10-29", 3))
		e.sent("conversation:s1", mdAt.Add(-24*time.Hour))
		fakeOperations = []string{fakeOp("op-1", "completed", "conversation:s1", 0)}
		documentUnits["conversation:s1"] = 3
		if _, err := e.ix.CatchUp(context.Background(), 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		return e
	}
	runWith := func(e *mdEnv, x mdIndex) string {
		fakeListLog = nil
		w := metricsfile.New(e.d.MetricsDir(), "test")
		w.Now = func() time.Time { return mdAt }
		return collectMemoryDocs(context.Background(), w, x, e.ro, mdAt)
	}
	for _, c := range []struct {
		name string
		x    mdIndex
	}{
		{"catch-up partial", mdIndex{fresh: &index.Freshness{Partial: true}}},
		{"catch-up failed", mdIndex{catchErr: errors.New("index failure")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			c.x.Index = e.ix
			before := len(mdKind(e.lines(), "listed"))
			if got, want := runWith(e, c.x), "memory docs: days not read (index behind), 1 operation(s), 1 unit count(s)"; got != want {
				t.Errorf("result %q, want %q", got, want)
			}
			if q := mdQueries(); len(q) != 0 {
				t.Errorf("list calls %v, want none", q)
			}
			lines := e.lines()
			if l := mdKind(lines, "memory"); len(l) != 0 {
				t.Errorf("%d memory line(s) written behind the index", len(l))
			}
			if l := mdKind(lines, "listed"); len(l) != before {
				t.Errorf("%d listed line(s), want the earlier run's %d", len(l), before)
			}
			if ops := mdKind(lines, "op"); len(ops) != 1 {
				t.Errorf("%d op lines, want 1", len(ops))
			}
		})
	}
	for _, c := range []struct {
		name string
		x    mdIndex
	}{
		{"sends not read", mdIndex{sentErr: errors.New("index failure")}},
		{"operations not read", mdIndex{recsErr: errors.New("index failure")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			c.x.Index = e.ix
			if got, want := runWith(e, c.x), "memory docs: 1 day(s) listed (3 memories), every day listed, operations not read (index)"; got != want {
				t.Errorf("result %q, want %q", got, want)
			}
			if ops := mdKind(e.lines(), "op"); len(ops) != 0 {
				t.Errorf("%d op lines, want none", len(ops))
			}
		})
	}
}

// The step's deadline in the middle of a run: the days read whole are
// written, the day cut writes nothing and is left for the next run.
func TestMemoryDocsDeadlineKeepsWholeDays(t *testing.T) {
	old := memoryDocsStepTimeout
	memoryDocsStepTimeout = time.Second
	t.Cleanup(func() { memoryDocsStepTimeout = old })
	e := newMDEnv(t)
	e.listedRange("2026-10-26", true)
	setBank(bankDay("2026-10-27", 3), bankDay("2026-10-28", 4), bankDay("2026-10-29", 600))
	fakeHook = func(r *http.Request) {
		if q := r.URL.Query(); q.Get("start_date") == "2026-10-29T00:00:00Z" && q.Get("offset") == "500" {
			<-r.Context().Done() // held until the client gives up: the step's deadline, before the call's 3 s
		}
	}
	start := time.Now()
	got := e.run(mdAt)
	// Three times the deadline, still below the call's own 3 s bound.
	if el := time.Since(start); el >= 3*time.Second {
		t.Errorf("step took %v, want about its 1 s deadline", el)
	}
	if want := "memory docs: 2 day(s) listed (7 memories), 1 day(s) left for the next run, operations not read (timeout)"; got != want {
		t.Errorf("result %q, want %q", got, want)
	}
	lines := e.lines()
	la := listedAt(lines)
	for d, n := range map[string]int{"2026-10-27": 3, "2026-10-28": 4} {
		if mem, _ := memoriesOf(lines, d); len(mem) != n {
			t.Errorf("%s: %d memory lines, want %d", d, len(mem), n)
		}
		if _, ok := la[d]; !ok {
			t.Errorf("%s not listed", d)
		}
	}
	if mem, _ := memoriesOf(lines, "2026-10-29"); len(mem) != 0 {
		t.Errorf("the cut day wrote %d memory lines", len(mem))
	}
	if _, ok := la["2026-10-29"]; ok {
		t.Error("the cut day is listed")
	}
}

// A day of more pages than half the run's list calls goes the hourly way
// (an ordinary day then always fits a run); under that it is read whole.
func TestMemoryDocsHalfTheBudgetIsHuge(t *testing.T) {
	for _, c := range []struct {
		name    string
		n       int
		windows bool
	}{{"150 pages", 15000, true}, {"90 pages", 9000, false}} {
		t.Run(c.name, func(t *testing.T) {
			e := newMDEnv(t)
			e.listedRange("2026-10-26", true)
			e.listed(false, "2026-10-28", "2026-10-29")
			fakeListCap = 100
			setBank(bankDay("2026-10-27", c.n))
			want := fmt.Sprintf("memory docs: 1 day(s) listed (%d memories), every day listed, 0 operation(s), 0 unit count(s)", c.n)
			if got := e.run(mdAt); got != want {
				t.Errorf("result %q, want %q", got, want)
			}
			var windows, days int
			for _, q := range mdQueries() {
				switch {
				case strings.HasPrefix(q, "window 2026-10-27T"):
					windows++
				case strings.HasPrefix(q, "day 2026-10-27@"):
					days++
				}
			}
			if c.windows && (days != 1 || windows != 24*7) {
				t.Errorf("%d day and %d window queries, want page 0 of the day then 24 windows of 7 pages", days, windows)
			}
			if !c.windows && (days != 90 || windows != 0) {
				t.Errorf("%d day and %d window queries, want the day's 90 pages", days, windows)
			}
			lines := e.lines()
			if mem, _ := memoriesOf(lines, "2026-10-27"); len(mem) != c.n {
				t.Errorf("%d memory lines, want %d", len(mem), c.n)
			}
			if pos, ok := listedAt(lines)["2026-10-27"]; !ok || lines[pos]["n"] != float64(c.n) {
				t.Errorf("listed %v", ok)
			}
		})
	}
}

// Later pages smaller than page 0 spend the reservation before the total:
// the day is cut there (no call beyond what it reserved), the other days
// are written.
func TestMemoryDocsSmallerPagesCutTheDay(t *testing.T) {
	e := newMDEnv(t)
	e.listedRange("2026-10-26", true)
	e.listed(false, "2026-10-29")
	setBank(bankDay("2026-10-27", 1000), bankDay("2026-10-28", 5))
	fakeListPage = func(q url.Values, page []fakeMem, total int) ([]fakeMem, int, int) {
		if q.Get("start_date") == "2026-10-27T00:00:00Z" && q.Get("offset") != "0" && len(page) > 250 {
			page = page[:250]
		}
		return page, total, 0
	}
	if got, want := e.run(mdAt), "memory docs: 1 day(s) listed (5 memories), 1 day(s) left for the next run, 0 operation(s), 0 unit count(s)"; got != want {
		t.Errorf("result %q, want %q", got, want)
	}
	if q := dayQueries(); !slices.Equal(q, []string{"day 2026-10-27@0/500", "day 2026-10-27@500/500", "day 2026-10-28@0/500"}) {
		t.Errorf("day queries %v, want 27's page 0 and its one reserved page, then 28", q)
	}
	lines := e.lines()
	if mem, _ := memoriesOf(lines, "2026-10-27"); len(mem) != 0 {
		t.Errorf("%d memory lines of the cut day", len(mem))
	}
	if _, ok := listedAt(lines)["2026-10-28"]; !ok {
		t.Error("28 not listed")
	}
}

// mdPages is a reader answering one span of total memories with the
// page sizes given by offset (absent: empty), counting its calls.
type mdPages struct {
	total int
	sizes map[int]int
	calls []int
}

func (p *mdPages) ListCreatedMemories(_ context.Context, _, _ time.Time, offset, _ int) ([]hindsight.ListedMemory, int, error) {
	p.calls = append(p.calls, offset)
	items := make([]hindsight.ListedMemory, p.sizes[offset])
	for i := range items {
		items[i] = hindsight.ListedMemory{ID: fmt.Sprintf("m%06d", offset+i), FactType: "world"}
	}
	return items, p.total, nil
}

func (*mdPages) RetainOperations(context.Context, string, int) ([]hindsight.Operation, error) {
	return nil, nil
}

func (*mdPages) DocumentUnits(context.Context, string) (int, bool, error) { return 0, false, nil }

// A span cut before its reservation is spent gives the rest back; one
// whose reservation runs out stops there.
func TestReadSpanReturnsItsReservation(t *testing.T) {
	for _, c := range []struct {
		name  string
		sizes map[int]int
		calls []int
	}{
		{"empty page", map[int]int{0: 500}, []int{0, 500}},                             // reserves 2, spends 1
		{"smaller pages", map[int]int{0: 500, 500: 250, 750: 250}, []int{0, 500, 750}}, // reserves 2, spends 2, 1 000 of 1 500
	} {
		t.Run(c.name, func(t *testing.T) {
			p := &mdPages{total: 1500, sizes: c.sizes}
			m := &mdRun{r: p}
			m.lists.Store(10)
			s := m.readSpan(context.Background(), mdAt, mdAt.Add(time.Hour), true, nil)
			if s.whole {
				t.Errorf("span read whole: %d items", len(s.items))
			}
			if !slices.Equal(p.calls, c.calls) {
				t.Errorf("calls at offsets %v, want %v", p.calls, c.calls)
			}
			if left := m.lists.Load(); left != int64(10-len(c.calls)) {
				t.Errorf("%d calls left, want %d (the reservation not spent goes back)", left, 10-len(c.calls))
			}
		})
	}
}
