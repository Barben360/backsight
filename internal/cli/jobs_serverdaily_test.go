package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/metricsfile"
	"github.com/Barben360/backsight/internal/output"
)

// sdFake is a server-daily reader whose answers the test sets. Buckets
// are cut at the fake's clock like a server's: up to today's (UTC), back
// to the bucket holding now − 90 days for the series and now − 30 days for
// the LLM stats (a bucket the window covers in part only).
type sdFake struct {
	mu        sync.Mutex
	now       time.Time
	stats     hindsight.BankStats
	statsErr  error
	tags      []hindsight.TagCount
	tagsErr   error
	series    hindsight.Timeseries
	seriesErr error
	llm       map[string][]hindsight.LLMBucket // by operation, "" = all
	llmErr    map[string]error
	block     bool // every call waits for its context, then longer
	calls     []string
}

func (f *sdFake) record(c string) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func (f *sdFake) wait() {
	if f.block {
		time.Sleep(10 * time.Second) // ignores its context
	}
}

func (f *sdFake) BankStats(context.Context) (hindsight.BankStats, error) {
	f.record("stats")
	f.wait()
	return f.stats, f.statsErr
}

func (f *sdFake) ProjectTags(context.Context) ([]hindsight.TagCount, error) {
	f.record("tags")
	f.wait()
	return f.tags, f.tagsErr
}

func (f *sdFake) MemoriesTimeseries(_ context.Context, period string) (hindsight.Timeseries, error) {
	f.record("series:" + period)
	f.wait()
	if f.seriesErr != nil {
		return hindsight.Timeseries{}, f.seriesErr
	}
	out := f.series
	out.Buckets = nil
	for _, b := range f.series.Buckets {
		if !b.Time.After(f.now) && !b.Time.Before(utcDay(f.now.Add(-90*24*time.Hour))) {
			out.Buckets = append(out.Buckets, b)
		}
	}
	return out, nil
}

func (f *sdFake) LLMStats(_ context.Context, period, op string) ([]hindsight.LLMBucket, error) {
	f.record("llm:" + period + ":" + op)
	f.wait()
	if err := f.llmErr[op]; err != nil {
		return nil, err
	}
	var out []hindsight.LLMBucket
	for _, b := range f.llm[op] {
		if !b.Time.After(f.now) && !b.Time.Before(utcDay(f.now.Add(-30*24*time.Hour))) {
			out = append(out, b)
		}
	}
	return out, nil
}

// statsJSON decodes a /stats body the way the client does (Present set).
func statsJSON(t *testing.T, body string) hindsight.BankStats {
	t.Helper()
	var s hindsight.BankStats
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

const fullStats = `{"total_nodes":60,"total_documents":7,"total_links":30,"total_observations":12,
"nodes_by_fact_type":{"world":40,"experience":15,"observation":5},
"operations_by_status":{"pending":2,"completed":10,"failed":1,"weird":3,"cancelled":4},
"pending_consolidation":3,"failed_consolidation":1,"last_consolidated_at":"2026-10-29T22:00:00+00:00"}`

// seriesDays is n daily buckets ending on last (UTC midnight), bucket i
// holding world i+1, experience 1, observation 2.
func seriesDays(last time.Time, n int) hindsight.Timeseries {
	ts := hindsight.Timeseries{Period: "90d", Trunc: "day"}
	for i := range n {
		ts.Buckets = append(ts.Buckets, hindsight.SeriesBucket{Time: last.AddDate(0, 0, i-n+1), World: int64(i + 1), Experience: 1, Observation: 2})
	}
	return ts
}

// llmDays is n daily buckets ending on last, input 100×(op index+1).
func llmDays(last time.Time, n int, trunc string, statuses map[string]int64) map[string][]hindsight.LLMBucket {
	out := map[string][]hindsight.LLMBucket{}
	for k, op := range []string{"", "retain", "consolidation", "refresh_mental_model"} {
		for i := range n {
			out[op] = append(out[op], hindsight.LLMBucket{Trunc: trunc, Time: last.AddDate(0, 0, i-n+1), Statuses: statuses,
				Total: 4, Input: int64(100 * (k + 1)), Output: 10, Cached: 5, Thoughts: 2})
		}
	}
	return out
}

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// healthyFake answers 90 series days and 30 LLM days, both up to today's
// bucket at the fake's clock, from 100 days before at to 100 days after.
func healthyFake(t *testing.T, at time.Time) *sdFake {
	t.Helper()
	today := utcDay(at)
	return &sdFake{now: at, stats: statsJSON(t, fullStats),
		tags:   []hindsight.TagCount{{Tag: "project:backsight", Count: 41}, {Tag: "project:site", Count: 9}},
		series: seriesDays(today.AddDate(0, 0, 100), 201),
		llm:    llmDays(today.AddDate(0, 0, 100), 201, "day", map[string]int64{"success": 3, "error": 1}),
	}
}

func paris(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// sdRun runs the step at at, the writer's clock at at too.
func sdRun(t *testing.T, dir string, r serverDailyReader, at time.Time, loc *time.Location) string {
	t.Helper()
	if f, ok := r.(*sdFake); ok {
		f.now = at
	}
	w := metricsfile.New(dir, "test")
	w.Now = func() time.Time { return at }
	return collectServerDaily(context.Background(), w, r, at, loc)
}

type sdLines struct {
	snaps []map[string]any
	days  []map[string]any
	files map[string]int // file base name → lines
}

func readSD(t *testing.T, dir string) sdLines {
	t.Helper()
	out := sdLines{files: map[string]int{}}
	files, _ := filepath.Glob(filepath.Join(dir, "server-daily-*.jsonl"))
	slices.Sort(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if l == "" {
				continue
			}
			out.files[filepath.Base(f)]++
			var m map[string]any
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("%s: %v", l, err)
			}
			switch m["kind"] {
			case "snapshot":
				out.snaps = append(out.snaps, m)
			case "day":
				out.days = append(out.days, m)
			default:
				t.Fatalf("unexpected line %s", l)
			}
		}
	}
	return out
}

func (s sdLines) buckets() []string {
	var b []string
	for _, d := range s.days {
		b = append(b, d["bucket"].(string)[:10])
	}
	return b
}

// newest is the last of b; an empty b fails the test.
func newest(t *testing.T, b []string) string {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("no day line")
	}
	return b[len(b)-1]
}

// noDuplicates fails when a bucket has two lines.
func (s sdLines) noDuplicates(t *testing.T) {
	t.Helper()
	seen := map[string]bool{}
	for _, b := range s.buckets() {
		if seen[b] {
			t.Errorf("bucket %s recorded twice", b)
		}
		seen[b] = true
	}
}

// The first run takes the snapshot and records every past full UTC day the
// server holds: series days with new counts, LLM days with each operation.
func TestServerDailyFirstRunBackfills(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)
	f := healthyFake(t, at)
	got := sdRun(t, dir, f, at, paris(t))
	if got != "server daily: snapshot, 89 day(s)" {
		t.Errorf("result %q", got)
	}
	rec := readSD(t, dir)
	if len(rec.snaps) != 1 || len(rec.days) != 89 {
		t.Fatalf("%d snapshot(s), %d day line(s), want 1 and 89", len(rec.snaps), len(rec.days))
	}
	s := rec.snaps[0]
	want := map[string]any{"day": "2026-10-30", "ok": true, "world": 40.0, "experience": 15.0, "observation": 5.0,
		"documents": 7.0, "links": 30.0, "observations": 12.0,
		"ops":                   map[string]any{"pending": 2.0, "processing": 0.0, "completed": 10.0, "failed": 1.0, "other": 7.0},
		"consolidation_pending": 3.0, "consolidation_failed": 1.0, "last_consolidated_at": "2026-10-29T22:00:00Z",
		"projects": map[string]any{"backsight": 41.0, "site": 9.0}}
	for k, v := range want {
		if !reflect.DeepEqual(s[k], v) {
			t.Errorf("snapshot %s = %v, want %v", k, s[k], v)
		}
	}
	if _, ok := s["error"]; ok {
		t.Errorf("an ok snapshot carries an error: %v", s)
	}
	b := rec.buckets()
	if newest(t, b) != "2026-10-29" || b[0] != "2026-08-02" || b[len(b)-1] != "2026-10-29" {
		t.Errorf("buckets %s … %s, want 2026-08-02 … 2026-10-29", b[0], b[len(b)-1])
	}
	if slices.Contains(b, "2026-10-30") {
		t.Error("today's bucket recorded")
	}
	rec.noDuplicates(t)
	for i, d := range rec.days {
		_, hasNew := d["new"]
		_, hasLLM := d["llm"]
		if !hasNew || hasLLM != (i >= 60) {
			t.Errorf("day %d (%v): new %v llm %v, want new and llm only for the 29 newest", i, d["bucket"], hasNew, hasLLM)
		}
	}
	if got := rec.days[0]["new"]; !reflect.DeepEqual(got, map[string]any{"world": 12.0, "experience": 1.0, "observation": 2.0}) { // the 12th bucket of the fake
		t.Errorf("oldest day's new %v", got)
	}
	llm := rec.days[88]["llm"].(map[string]any)
	if len(llm) != 4 {
		t.Errorf("llm operations %v, want all, retain, consolidation, refresh_mental_model", llm)
	}
	wantRetain := map[string]any{"input": 200.0, "output": 10.0, "cached": 5.0, "thoughts": 2.0, "requests": 4.0,
		"statuses": map[string]any{"success": 3.0, "error": 1.0}}
	if !reflect.DeepEqual(llm["retain"], wantRetain) || llm["all"].(map[string]any)["input"] != 100.0 || llm["refresh_mental_model"].(map[string]any)["input"] != 400.0 {
		t.Errorf("llm %v", llm)
	}
	if rec.days[88]["bucket"] != "2026-10-29T00:00:00Z" {
		t.Errorf("bucket %v, want the UTC day start", rec.days[88]["bucket"])
	}
	f.mu.Lock()
	calls := slices.Sorted(slices.Values(f.calls))
	f.mu.Unlock()
	if want := []string{"llm:30d:", "llm:30d:consolidation", "llm:30d:refresh_mental_model", "llm:30d:retain", "series:90d", "stats", "tags"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("calls %v, want %v", calls, want)
	}
}

// A second run the same day writes nothing: the snapshot is taken and every
// past day is recorded.
func TestServerDailySecondRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)
	f := healthyFake(t, at)
	sdRun(t, dir, f, at, paris(t))
	before := readSD(t, dir)
	// The server now says more about a recorded day: it is not rewritten.
	f.series.Buckets[50].World = 999 // a recorded day
	got := sdRun(t, dir, f, at.Add(6*time.Hour), paris(t))
	if got != "server daily: snapshot already taken, 0 day(s)" {
		t.Errorf("result %q", got)
	}
	after := readSD(t, dir)
	if len(after.snaps) != len(before.snaps) || len(after.days) != len(before.days) {
		t.Errorf("second run wrote %d snapshot(s), %d day(s)", len(after.snaps)-len(before.snaps), len(after.days)-len(before.days))
	}
}

// The server down: a gap snapshot with the error enum, the job done; the
// next day with the server back, an ok snapshot and the day it missed.
func TestServerDailyGapIsVisible(t *testing.T) {
	home := maintainWithoutBackup(t)
	fixedMaintainDoctor(t, doctor.Check{ID: "a", Status: doctor.OK})
	dir := pathsOf(home).MetricsDir()
	day0 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	f := healthyFake(t, day0)
	sdRun(t, dir, f, day0, time.Local) // the record up to 2026-10-19

	down := &sdFake{statsErr: hindsight.ErrUnreachable, tagsErr: hindsight.ErrUnreachable, seriesErr: hindsight.ErrUnreachable,
		llmErr: map[string]error{"": hindsight.ErrUnreachable, "retain": hindsight.ErrUnreachable, "consolidation": hindsight.ErrUnreachable, "refresh_mental_model": hindsight.ErrUnreachable}}
	var reader serverDailyReader = down
	old := serverDailyReaderFor
	serverDailyReaderFor = func() (serverDailyReader, error) { return reader, nil }
	t.Cleanup(func() { serverDailyReaderFor = old })

	day1 := day0.AddDate(0, 0, 1)
	withClock(t, day1)
	down.now = day1
	code, out, _ := run(t, "jobs", "maintain", "--json")
	var env struct {
		Data maintainResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if code != output.OK || env.Data.Status != "done" || env.Data.ServerDaily != "server daily: not reached (unreachable), 0 day(s)" {
		t.Errorf("exit %d, status %q, server daily %q", code, env.Data.Status, env.Data.ServerDaily)
	}
	// A second run that day keeps a single gap line.
	if _, text, _ := run(t, "jobs", "maintain"); !strings.Contains(text, "server daily: not reached (unreachable), 0 day(s)") {
		t.Errorf("text:\n%s", text)
	}
	rec := readSD(t, dir)
	if len(rec.snaps) != 2 {
		t.Fatalf("%d snapshots, want the first day's and one gap", len(rec.snaps))
	}
	gap := rec.snaps[1]
	if gap["ok"] != false || gap["error"] != "unreachable" || gap["day"] != day1.In(time.Local).Format("2006-01-02") {
		t.Errorf("gap %v", gap)
	}
	for k := range gap {
		if !slices.Contains([]string{"day", "ok", "error", "schema", "ts", "kind", "v", "binary_version"}, k) {
			t.Errorf("gap line carries %s", k)
		}
	}

	day2 := day1.AddDate(0, 0, 1)
	f.now = day2
	reader = f
	withClock(t, day2)
	if code, out, _ := run(t, "jobs", "maintain", "--json"); code != output.OK || !strings.Contains(out, `"server_daily": "server daily: snapshot, 2 day(s)"`) {
		t.Errorf("exit %d\n%s", code, out)
	}
	rec = readSD(t, dir)
	if len(rec.snaps) != 3 || rec.snaps[2]["ok"] != true {
		t.Errorf("snapshots %v", rec.snaps)
	}
	b := rec.buckets()
	if len(b) < 2 {
		t.Fatalf("buckets %v", b)
	}
	if got := b[len(b)-2:]; !reflect.DeepEqual(got, []string{"2026-10-20", "2026-10-21"}) {
		t.Errorf("newest buckets %v, want the missed day and yesterday", got)
	}
	rec.noDuplicates(t)
}

// One ok snapshot per local day across the DST change and midnight, day
// lines read across a month file boundary, and a hole after days off filled
// once.
func TestServerDailyCalendarEdges(t *testing.T) {
	loc := paris(t)
	t.Run("dst", func(t *testing.T) {
		dir := t.TempDir()
		first := time.Date(2026, 10, 25, 0, 30, 0, 0, loc) // +02:00, the previous UTC day
		f := healthyFake(t, first)
		sdRun(t, dir, f, first, loc)
		last := time.Date(2026, 10, 25, 23, 30, 0, 0, loc) // +01:00
		if _, off := last.Zone(); off != 3600 {
			t.Fatalf("offset %d, want winter time", off)
		}
		if got := sdRun(t, dir, f, last, loc); got != "server daily: snapshot already taken, 1 day(s)" {
			t.Errorf("result %q", got)
		}
		rec := readSD(t, dir)
		if len(rec.snaps) != 1 || rec.snaps[0]["day"] != "2026-10-25" {
			t.Errorf("snapshots %v", rec.snaps)
		}
		if b := newest(t, rec.buckets()); b != "2026-10-24" {
			t.Errorf("newest bucket %s, want 2026-10-24 (the UTC day of the second run is the 25th)", b)
		}
		rec.noDuplicates(t)
	})
	t.Run("month files", func(t *testing.T) {
		dir := t.TempDir()
		oct := time.Date(2026, 10, 31, 10, 0, 0, 0, loc)
		f := healthyFake(t, oct)
		sdRun(t, dir, f, oct, loc)
		// 00:30 local on the 1st is 23:30 UTC on the 31st: a new local day,
		// no new UTC day.
		midnight := time.Date(2026, 11, 1, 0, 30, 0, 0, loc)
		if got := sdRun(t, dir, f, midnight, loc); got != "server daily: snapshot, 0 day(s)" {
			t.Errorf("result %q", got)
		}
		noon := time.Date(2026, 11, 1, 12, 0, 0, 0, loc)
		if got := sdRun(t, dir, f, noon, loc); got != "server daily: snapshot already taken, 1 day(s)" {
			t.Errorf("result %q", got)
		}
		rec := readSD(t, dir)
		if rec.files["server-daily-2026-10.jsonl"] == 0 || rec.files["server-daily-2026-11.jsonl"] != 1 {
			t.Errorf("files %v, want October's lines and one November line", rec.files)
		}
		var days []string
		for _, s := range rec.snaps {
			days = append(days, s["day"].(string))
		}
		if !reflect.DeepEqual(days, []string{"2026-10-31", "2026-11-01"}) {
			t.Errorf("snapshot days %v", days)
		}
		if b := newest(t, rec.buckets()); b != "2026-10-31" {
			t.Errorf("newest bucket %s", b)
		}
		rec.noDuplicates(t)
	})
	t.Run("forty days off", func(t *testing.T) {
		dir := t.TempDir()
		first := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
		f := healthyFake(t, first)
		sdRun(t, dir, f, first, loc)
		back := first.AddDate(0, 0, 40)
		if got := sdRun(t, dir, f, back, loc); got != "server daily: snapshot, 40 day(s)" {
			t.Errorf("result %q", got)
		}
		rec := readSD(t, dir)
		rec.noDuplicates(t)
		b := rec.buckets()
		// Every day from the first run's series reach to the day before
		// the second run, each once.
		want := []string{}
		for d := utcDay(first).AddDate(0, 0, -89); d.Before(utcDay(back)); d = d.AddDate(0, 0, 1) {
			want = append(want, d.Format("2006-01-02"))
		}
		slices.Sort(b)
		newest(t, b)
		if !reflect.DeepEqual(b, want) {
			t.Errorf("%d buckets %s … %s, want %d %s … %s", len(b), b[0], b[len(b)-1], len(want), want[0], want[len(want)-1])
		}
	})
}

// Odd answers give lines with what was said, never invented zeros, and a
// reader that ignores its context cannot hold the job.
func TestServerDailySurvivesOddServers(t *testing.T) {
	at := time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)
	loc := paris(t)
	t.Run("fields missing", func(t *testing.T) {
		dir := t.TempDir()
		f := healthyFake(t, at)
		f.stats = statsJSON(t, `{"total_nodes":60,"nodes_by_fact_type":null,"pending_consolidation":null}`)
		f.tags = nil
		f.llm = llmDays(utcDay(at), 30, "day", nil)
		sdRun(t, dir, f, at, loc)
		rec := readSD(t, dir)
		if len(rec.snaps) != 1 || len(rec.days) == 0 {
			t.Fatalf("%d snapshot(s), %d day(s)", len(rec.snaps), len(rec.days))
		}
		s := rec.snaps[0]
		for _, k := range []string{"world", "experience", "observation", "documents", "links", "observations", "ops", "consolidation_pending", "consolidation_failed", "last_consolidated_at", "projects", "error"} {
			if _, ok := s[k]; ok {
				t.Errorf("snapshot has %s though the server did not say it: %v", k, s)
			}
		}
		if s["ok"] != true {
			t.Errorf("snapshot %v", s)
		}
		llm := rec.days[len(rec.days)-1]["llm"].(map[string]any)
		if _, ok := llm["all"].(map[string]any)["statuses"]; ok {
			t.Errorf("null statuses recorded: %v", llm)
		}
	})
	t.Run("llm 401", func(t *testing.T) {
		// The days the LLM stats cover (from 2026-09-30) wait for them; the
		// older ones need the series only.
		dir := t.TempDir()
		f := healthyFake(t, at)
		unauthorized := &hindsight.APIError{Kind: hindsight.Unauthorized, Status: http.StatusUnauthorized}
		f.llmErr = map[string]error{"retain": unauthorized}
		if got := sdRun(t, dir, f, at, loc); got != "server daily: snapshot, 59 day(s)" {
			t.Errorf("result %q", got)
		}
		rec := readSD(t, dir)
		if b := newest(t, rec.buckets()); b != "2026-09-29" {
			t.Errorf("newest bucket %s, want 2026-09-29", b)
		}
		for _, d := range rec.days {
			if _, ok := d["llm"]; ok || d["new"] == nil {
				t.Fatalf("day %v, want new only", d)
			}
		}
	})
	t.Run("hourly llm", func(t *testing.T) {
		dir := t.TempDir()
		f := healthyFake(t, at)
		f.llm = llmDays(utcDay(at), 30, "hour", nil)
		f.series = hindsight.Timeseries{Trunc: "day"} // nothing from the series either
		if got := sdRun(t, dir, f, at, loc); got != "server daily: snapshot, 0 day(s)" {
			t.Errorf("result %q", got)
		}
		if rec := readSD(t, dir); len(rec.days) != 0 {
			t.Errorf("%d day lines from hourly buckets", len(rec.days))
		}
	})
	t.Run("blocking reader", func(t *testing.T) {
		dir := t.TempDir()
		f := healthyFake(t, at)
		f.block = true
		start := time.Now()
		got := sdRun(t, dir, f, at, loc)
		// The bound is the calls' 3 s context deadline, not the fake
		// clock: the step abandons them.
		if el := time.Since(start); el > 3500*time.Millisecond {
			t.Errorf("step took %v, want within 3.5 s", el)
		}
		if got != "server daily: not reached (timeout), 0 day(s)" {
			t.Errorf("result %q", got)
		}
		if rec := readSD(t, dir); len(rec.snaps) != 1 || rec.snaps[0]["error"] != "timeout" {
			t.Errorf("snapshots %v", rec.snaps)
		}
	})
	t.Run("blocking reader in the job", func(t *testing.T) {
		maintainWithoutBackup(t)
		fixedMaintainDoctor(t, doctor.Check{ID: "a", Status: doctor.OK})
		f := healthyFake(t, at)
		f.block = true
		old := serverDailyReaderFor
		serverDailyReaderFor = func() (serverDailyReader, error) { return f, nil }
		t.Cleanup(func() { serverDailyReaderFor = old })
		withClock(t, at)
		code, out, _ := run(t, "jobs", "maintain", "--json")
		if code != output.OK || !strings.Contains(out, `"status": "done"`) || !strings.Contains(out, `"server_daily": "server daily: not reached (timeout), 0 day(s)"`) {
			t.Errorf("exit %d\n%s", code, out)
		}
	})
}

// Through the real read-only client, only GET requests reach the server.
func TestServerDailyUsesGETOnly(t *testing.T) {
	port := fakeHindsight(t)
	var mu sync.Mutex
	var seen []string
	fakeHook = func(r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
	}
	ro, err := hindsight.NewReadOnly("http://127.0.0.1:"+strconv.Itoa(port), "k", "b", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	at := time.Now()
	w := metricsfile.New(dir, "test")
	// The fake's answers hold today's bucket and, for the LLM stats, one
	// ten days back: each its answer's earliest, so no day line.
	if got := collectServerDaily(context.Background(), w, ro, at, time.Local); got != "server daily: snapshot, 0 day(s)" {
		t.Errorf("result %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	paths := map[string]bool{}
	for _, s := range seen {
		method, path, _ := strings.Cut(s, " ")
		if method != http.MethodGet {
			t.Errorf("%s reached the server", s)
		}
		paths[path[strings.LastIndex(path, "/b/")+2:]] = true
	}
	for _, p := range []string{"/stats", "/tags", "/stats/memories-timeseries", "/llm-requests/stats"} {
		if !paths[p] {
			t.Errorf("%s not read (read: %v)", p, paths)
		}
	}
	if rec := readSD(t, dir); len(rec.snaps) != 1 {
		t.Errorf("snapshots %v", rec.snaps)
	}
}

// Without a configured server the step says so and writes nothing.
func TestServerDailyWithoutServer(t *testing.T) {
	isolate(t)
	if r, err := serverDailyReaderFor(); r != nil || err != nil {
		t.Fatalf("without a config: reader %T, error %v", r, err)
	}
	dir := t.TempDir()
	if got := sdRun(t, dir, nil, time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC), time.UTC); got != "server daily: no server set up" {
		t.Errorf("result %q", got)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*")); len(files) != 0 {
		t.Errorf("files written: %v", files)
	}
}

// A configuration that cannot be read is a server not reached, not a
// server never set up.
func TestServerDailyConfigErrorIsAFailure(t *testing.T) {
	home := devInstall(t, true)
	if err := os.WriteFile(pathsOf(home).ConfigFile(), []byte("[server\nurl = "), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)
	got := maintainServerDaily(context.Background(), pathsOf(home), func() time.Time { return at })
	if got != "server daily: not reached (not_configured), 0 day(s)" {
		t.Errorf("result %q", got)
	}
}

// The first bucket of each answer may hold part of its day (the server's
// window starts at now − period): it is never recorded. The day comes from
// an earlier run that saw it whole, or stays a hole.
func TestServerDailySkipsPartialEarliestBucket(t *testing.T) {
	at := time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)
	loc := paris(t)
	t.Run("one run", func(t *testing.T) {
		dir := t.TempDir()
		f := healthyFake(t, at)
		sdRun(t, dir, f, at, loc)
		rec := readSD(t, dir)
		byBucket := map[string]map[string]any{}
		for _, d := range rec.days {
			byBucket[d["bucket"].(string)[:10]] = d
		}
		if _, ok := byBucket["2026-08-01"]; ok {
			t.Error("the series' partial first day (2026-08-01) recorded")
		}
		if d := byBucket["2026-09-30"]; d == nil || d["llm"] != nil || d["new"] == nil {
			t.Errorf("2026-09-30 %v, want new only (the LLM stats' partial first day)", d)
		}
	})
	t.Run("an earlier run saw it whole", func(t *testing.T) {
		dir := t.TempDir()
		f := healthyFake(t, at)
		sdRun(t, dir, f, at.AddDate(0, 0, -1), loc)
		sdRun(t, dir, f, at, loc)
		rec := readSD(t, dir)
		rec.noDuplicates(t)
		byBucket := map[string]map[string]any{}
		for _, d := range rec.days {
			byBucket[d["bucket"].(string)[:10]] = d
		}
		if _, ok := byBucket["2026-07-31"]; ok {
			t.Error("the first run's partial first day (2026-07-31) recorded")
		}
		if d := byBucket["2026-08-01"]; d == nil || d["new"] == nil {
			t.Errorf("2026-08-01 %v, want the first run's whole day", d)
		}
		if d := byBucket["2026-09-30"]; d == nil || d["llm"] == nil {
			t.Errorf("2026-09-30 %v, want the first run's LLM day", d)
		}
	})
}

// A day is written only when every source that covers it answered: the
// series for 90 days, the LLM stats (each operation) for 30. A day that
// waits is written by a later run, once.
func TestServerDailyDayWaitsForEverySource(t *testing.T) {
	at := time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)
	loc := paris(t)
	dir := t.TempDir()
	f := healthyFake(t, at)
	f.seriesErr = hindsight.ErrTimeout
	if got := sdRun(t, dir, f, at, loc); got != "server daily: snapshot, 0 day(s)" {
		t.Errorf("series down: result %q", got)
	}
	f.seriesErr = nil
	f.llmErr = map[string]error{"consolidation": hindsight.ErrTimeout}
	if got := sdRun(t, dir, f, at.Add(time.Hour), loc); got != "server daily: snapshot already taken, 59 day(s)" {
		t.Errorf("llm down: result %q", got)
	}
	rec := readSD(t, dir)
	if b := newest(t, rec.buckets()); b != "2026-09-29" {
		t.Errorf("newest bucket %s with the LLM stats down, want 2026-09-29 (older than their reach)", b)
	}
	for _, d := range rec.days {
		if d["llm"] != nil || d["new"] == nil {
			t.Fatalf("day %v, want new only", d)
		}
	}
	f.llmErr = nil
	if got := sdRun(t, dir, f, at.Add(2*time.Hour), loc); got != "server daily: snapshot already taken, 30 day(s)" {
		t.Errorf("all back: result %q", got)
	}
	rec = readSD(t, dir)
	rec.noDuplicates(t)
	if len(rec.days) != 89 {
		t.Errorf("%d day lines, want 89", len(rec.days))
	}
	for _, d := range rec.days[59:] {
		b := d["bucket"].(string)[:10]
		llm, _ := d["llm"].(map[string]any)
		if d["new"] == nil || (b != "2026-09-30" && len(llm) != 4) {
			t.Errorf("day %s: new %v, llm %v, want both", b, d["new"], llm)
		}
	}
}
