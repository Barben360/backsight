package index

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/metricsfile"
	"github.com/Barben360/backsight/internal/paths"
)

// mline is one line of a collection family, newline included: the common
// fields (schema 1 unless fields sets it) plus fields.
func mline(kind, ts string, fields map[string]any) string {
	m := map[string]any{"schema": 1, "v": "v10", "binary_version": "1.2.3"}
	if kind != "" {
		m["kind"] = kind
	}
	if ts != "" {
		m["ts"] = ts
	}
	for k, v := range fields {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// mpath is a family's month file in the metrics directory.
func (e *ingestEnv) mpath(family, month string) string {
	return filepath.Join(e.d.MetricsDir(), family+"-"+month+".jsonl")
}

// mrel is the source_file of a family's month file.
func mrel(family, month string) string { return "metrics/" + family + "-" + month + ".jsonl" }

func (e *ingestEnv) appendM(family, month, content string) {
	e.t.Helper()
	if err := os.MkdirAll(e.d.MetricsDir(), 0o700); err != nil {
		e.t.Fatal(err)
	}
	f, err := os.OpenFile(e.mpath(family, month), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		e.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		e.t.Fatal(err)
	}
}

// tsAt is ts0 moved by d, in the writer's layout.
func tsAt(d time.Duration) string {
	return time.UnixMilli(ms0).UTC().Add(d).Format("2006-01-02T15:04:05.000Z")
}

func llmDay(input, output int64, statuses map[string]int64) map[string]any {
	return map[string]any{"input": input, "output": output, "cached": input / 10, "thoughts": output / 10, "requests": input / 100, "statuses": statuses}
}

func TestCollectionLinesBecomeRows(t *testing.T) {
	e := newEnv(t)
	sd, dh, md, sh := metricsfile.FamilyServerDaily, metricsfile.FamilyDoctorHistory, metricsfile.FamilyMemoryDocs, metricsfile.FamilyScanHistory
	e.appendM(sd, "2026-10", mline("snapshot", ts0, map[string]any{
		"day": "2026-10-30", "ok": true, "world": 10, "experience": 5, "observation": 3, "documents": 7, "links": 40, "observations": 3,
		"ops":                   map[string]any{"pending": 1, "processing": 2, "completed": 30, "failed": 4, "other": 0},
		"consolidation_pending": 2, "consolidation_failed": 1, "last_consolidated_at": "2026-10-30T11:00:00Z",
		"projects": map[string]any{"backsight": 12, "proj-a": 6},
	})+mline("day", ts0, map[string]any{
		"bucket": "2026-10-29T00:00:00Z", "new": map[string]any{"world": 4, "experience": 2, "observation": 1},
		"llm": map[string]any{
			"all":                  llmDay(1000, 500, map[string]int64{"success": 9, "error": 1}),
			"retain":               llmDay(600, 300, map[string]int64{"success": 5, "error": 1}),
			"consolidation":        llmDay(300, 150, map[string]int64{"success": 3, "error": 0}),
			"refresh_mental_model": llmDay(100, 50, map[string]int64{"success": 1, "error": 0}),
		},
	}))
	checks := map[string]any{}
	for i, level := range []string{"ok", "ok", "info", "warn", "error", "ok", "ok", "info", "ok"} {
		checks[fmt.Sprintf("check_%d", i)] = level
	}
	e.appendM(dh, "2026-10", mline("run", ts0, map[string]any{"checks": checks}))
	e.appendM(md, "2026-10", mline("memory", ts0, map[string]any{
		"memory": "0b4c0001-0000-4000-8000-000000000001", "outcome": "found", "doc_kind": "session",
		"document": "conversation:sess-1", "session": "sess-1", "created_day": "2026-10-28", "fact_type": "world",
	})+mline("listed", ts0, map[string]any{"day": "2026-10-28", "n": 2717})+
		mline("listed", ts0, map[string]any{"day": "2026-10-27", "n": 0, "complete": true})+
		mline("op", ts0, map[string]any{
			"operation": "op-1", "document": "conversation:sess-1", "session": "sess-1", "status": "completed",
			"created_at": "2026-10-30T10:00:00Z", "updated_at": "2026-10-30T10:02:00.5Z", "retries": 0, "units": 12,
		}))
	e.appendM(sh, "2026-10", mline("scan", ts0, map[string]any{"count": 2, "live": 1, "forgotten": 1, "pages": 12, "ms": 3400}))
	if st := e.ingest(); st.Lines != 8 || st.Errors != 0 {
		t.Fatalf("ingest %+v, want 8 lines and no error", st)
	}

	e.checkRows("server_snapshots", `"offset"`, []map[string]any{{
		"ts": ms0, "day": "2026-10-30", "ok": 1, "error": nil, "world": 10, "experience": 5, "observation": 3,
		"documents": 7, "links": 40, "observations": 3, "ops_pending": 1, "ops_processing": 2, "ops_completed": 30,
		"ops_failed": 4, "ops_other": 0, "consolidation_pending": 2, "consolidation_failed": 1,
		"last_consolidated_at": mustMS("2026-10-30T11:00:00Z"),
	}})
	e.checkRows("server_snapshot_projects", "project", []map[string]any{
		{"ts": ms0, "day": "2026-10-30", "project": "backsight", "memories": 12},
		{"ts": ms0, "day": "2026-10-30", "project": "proj-a", "memories": 6},
	})
	bucket := mustMS("2026-10-29T00:00:00Z")
	e.checkRows("server_days", `"offset"`, []map[string]any{{"ts": ms0, "bucket": bucket, "world": 4, "experience": 2, "observation": 1}})
	e.checkRows("server_day_llm", "operation", []map[string]any{
		{"ts": ms0, "bucket": bucket, "operation": "all", "input": 1000, "output": 500, "cached": 100, "thoughts": 50, "requests": 10},
		{"ts": ms0, "bucket": bucket, "operation": "consolidation", "input": 300, "output": 150, "cached": 30, "thoughts": 15, "requests": 3},
		{"ts": ms0, "bucket": bucket, "operation": "refresh_mental_model", "input": 100, "output": 50, "cached": 10, "thoughts": 5, "requests": 1},
		{"ts": ms0, "bucket": bucket, "operation": "retain", "input": 600, "output": 300, "cached": 60, "thoughts": 30, "requests": 6},
	})
	var st []map[string]any
	for _, op := range []struct {
		name       string
		succ, errs int
	}{{"all", 9, 1}, {"consolidation", 3, 0}, {"refresh_mental_model", 1, 0}, {"retain", 5, 1}} {
		st = append(st,
			map[string]any{"ts": ms0, "bucket": bucket, "operation": op.name, "status": "error", "n": op.errs},
			map[string]any{"ts": ms0, "bucket": bucket, "operation": op.name, "status": "success", "n": op.succ})
	}
	e.checkRows("server_day_llm_status", "operation, status", st)
	var runs []map[string]any
	for i, level := range []string{"ok", "ok", "info", "warn", "error", "ok", "ok", "info", "ok"} {
		runs = append(runs, map[string]any{"ts": ms0, "check_id": fmt.Sprintf("check_%d", i), "status": level})
	}
	e.checkRows("doctor_runs", "check_id", runs)
	e.checkRows("memory_docs", `"offset"`, []map[string]any{{
		"ts": ms0, "memory_id": "0b4c0001-0000-4000-8000-000000000001", "outcome": "found", "doc_kind": "session",
		"document": "conversation:sess-1", "session": "sess-1", "created_day": "2026-10-28", "fact_type": "world",
	}})
	e.checkRows("memory_days", `"offset"`, []map[string]any{
		{"ts": ms0, "day": "2026-10-28", "n": 2717, "complete": 0},
		{"ts": ms0, "day": "2026-10-27", "n": 0, "complete": 1},
	})
	e.checkRows("doc_ops", `"offset"`, []map[string]any{{
		"ts": ms0, "operation_id": "op-1", "document": "conversation:sess-1", "session": "sess-1", "status": "completed",
		"created_at": mustMS("2026-10-30T10:00:00Z"), "updated_at": mustMS("2026-10-30T10:02:00.5Z"), "retries": 0, "units": 12, "no_units": 0,
	}})
	e.checkRows("scans", `"offset"`, []map[string]any{{"ts": ms0, "count": 2, "live": 1, "forgotten": 1, "pages": 12, "ms": 3400}})
	if n := e.count("metrics_other"); n != 0 {
		t.Errorf("%d metrics_other rows", n)
	}
	for _, c := range []struct{ rel, kind string }{
		{mrel(sd, "2026-10"), kindServerDaily}, {mrel(dh, "2026-10"), kindDoctorHistory},
		{mrel(md, "2026-10"), kindMemoryDocs}, {mrel(sh, "2026-10"), kindScanHistory},
	} {
		rs := e.rowsOf(`SELECT kind, last_ts, missing FROM cursors WHERE source_file = ?`, c.rel)
		if len(rs) != 1 || rs[0]["kind"] != c.kind || rs[0]["last_ts"] != ms0 || rs[0]["missing"] != int64(0) {
			t.Errorf("cursor of %s: %v, want kind %s", c.rel, rs, c.kind)
		}
	}
}

// TestCollectionOmittedFieldsAreNull: a count the server did not give is
// omitted from the line and NULL in the index, never 0; a snapshot that
// failed keeps its error.
func TestCollectionOmittedFieldsAreNull(t *testing.T) {
	e := newEnv(t)
	e.appendM(metricsfile.FamilyServerDaily, "2026-10", mline("snapshot", ts0, map[string]any{"day": "2026-10-30", "ok": false, "error": "timeout"})+
		mline("day", ts0, map[string]any{"bucket": "2026-10-29T00:00:00Z"}))
	e.appendM(metricsfile.FamilyMemoryDocs, "2026-10", mline("op", ts0, map[string]any{"operation": "op-2", "status": "pending"}))
	if st := e.ingest(); st.Errors != 0 {
		t.Fatalf("ingest %+v", st)
	}
	nulls := map[string]any{}
	for _, c := range strings.Fields("world experience observation documents links observations ops_pending ops_processing ops_completed ops_failed ops_other consolidation_pending consolidation_failed last_consolidated_at") {
		nulls[c] = nil
	}
	nulls["ts"], nulls["day"], nulls["ok"], nulls["error"] = ms0, "2026-10-30", 0, "timeout"
	e.checkRows("server_snapshots", `"offset"`, []map[string]any{nulls})
	e.checkRows("server_days", `"offset"`, []map[string]any{{"ts": ms0, "bucket": mustMS("2026-10-29T00:00:00Z"), "world": nil, "experience": nil, "observation": nil}})
	if n := e.count("server_day_llm") + e.count("server_day_llm_status") + e.count("server_snapshot_projects"); n != 0 {
		t.Errorf("%d rows of absent maps", n)
	}
	e.checkRows("doc_ops", `"offset"`, []map[string]any{{"ts": ms0, "operation_id": "op-2", "document": nil, "session": nil, "status": "pending",
		"created_at": nil, "updated_at": nil, "retries": nil, "units": nil, "no_units": 0}})
}

func TestMemoryDaysViews(t *testing.T) {
	e := newEnv(t)
	md := metricsfile.FamilyMemoryDocs
	listed := func(at time.Duration, day string, n int, complete bool) string {
		f := map[string]any{"day": day, "n": n}
		if complete {
			f["complete"] = true
		}
		return mline("listed", tsAt(at), f)
	}
	mem := func(at time.Duration, id, created string) string {
		return mline("memory", tsAt(at), map[string]any{"memory": id, "outcome": "found", "doc_kind": "none", "created_day": created, "fact_type": "observation"})
	}
	coverage := func() map[string]any {
		t.Helper()
		rs := e.rowsOf(`SELECT complete_day, covered_until FROM memory_coverage`)
		if len(rs) != 1 {
			t.Fatalf("memory_coverage: %d rows, want 1", len(rs))
		}
		return rs[0]
	}

	// The same memory on two days' lists: the later line wins (in the
	// September file, an hour later), not the later day.
	e.appendM(md, "2026-10", mem(0, "m1", "2026-10-20")+listed(time.Second, "2026-10-21", 5, false))
	e.appendM(md, "2026-09", mem(time.Hour, "m1", "2026-10-19")+listed(time.Hour, "2026-10-21", 7, false))
	e.ingest()
	if rs := e.rowsOf(`SELECT memory_id, created_day, fact_type FROM memory_doc`); len(rs) != 1 || rs[0]["created_day"] != "2026-10-19" || rs[0]["memory_id"] != "m1" {
		t.Errorf("memory_doc %v, want m1 created 2026-10-19 once", rs)
	}
	if rs := e.rowsOf(`SELECT day, n, complete FROM memory_day`); len(rs) != 1 || rs[0]["n"] != int64(7) || rs[0]["complete"] != int64(0) {
		t.Errorf("memory_day %v, want 2026-10-21 once with n 7", rs)
	}
	if c := coverage(); c["complete_day"] != nil || c["covered_until"] != nil {
		t.Errorf("coverage without a complete line %v, want both NULL", c)
	}

	var b strings.Builder
	for _, d := range []string{"2026-10-20", "2026-10-22", "2026-10-23", "2026-10-24", "2026-10-25", "2026-10-26", "2026-10-28"} {
		b.WriteString(listed(2*time.Hour, d, 3, false))
	}
	b.WriteString(listed(3*time.Hour, "2026-10-19", 0, true))
	e.appendM(md, "2026-10", b.String())
	e.ingest()
	if c := coverage(); c["complete_day"] != "2026-10-19" || c["covered_until"] != "2026-10-26" {
		t.Errorf("coverage %v, want complete 2026-10-19, covered until 2026-10-26 (27 is a hole)", c)
	}
	e.appendM(md, "2026-10", listed(4*time.Hour, "2026-10-27", 2, false))
	e.ingest()
	if c := coverage(); c["complete_day"] != "2026-10-19" || c["covered_until"] != "2026-10-28" {
		t.Errorf("coverage %v, want covered until 2026-10-28 once 27 is listed", c)
	}
}

// TestCollectionLatestViews: doc_op and server_day keep, per operation and
// per bucket, the latest line; server_day names that line, so its llm
// rows join it.
func TestCollectionLatestViews(t *testing.T) {
	e := newEnv(t)
	e.appendM(metricsfile.FamilyMemoryDocs, "2026-10",
		mline("op", tsAt(0), map[string]any{"operation": "op-1", "status": "processing"})+
			mline("op", tsAt(time.Hour), map[string]any{"operation": "op-1", "status": "completed", "units": 3})+
			mline("op", tsAt(time.Minute), map[string]any{"operation": "op-2", "status": "failed", "retries": 2}))
	e.appendM(metricsfile.FamilyServerDaily, "2026-10",
		mline("day", tsAt(time.Hour), map[string]any{"bucket": "2026-10-29T00:00:00Z", "new": map[string]any{"world": 9, "experience": 0, "observation": 0},
			"llm": map[string]any{"retain": llmDay(700, 70, nil)}})+
			mline("day", tsAt(0), map[string]any{"bucket": "2026-10-29T00:00:00Z", "new": map[string]any{"world": 1, "experience": 0, "observation": 0}}))
	e.ingest()
	if got := queryStrings(t, e.ix.sh.w, `SELECT operation_id||'/'||status||'/'||coalesce(units,'-')||'/'||coalesce(retries,'-') FROM doc_op ORDER BY operation_id`); !slices.Equal(got, []string{"op-1/completed/3/-", "op-2/failed/-/2"}) {
		t.Errorf("doc_op %v", got)
	}
	got := queryStrings(t, e.ix.sh.w, `SELECT d.bucket||'/'||d.world||'/'||l.operation||'/'||l.input FROM server_day d
		JOIN server_day_llm l ON l.source_file = d.source_file AND l."offset" = d."offset"`)
	if want := []string{fmt.Sprintf("%d/9/retain/700", mustMS("2026-10-29T00:00:00Z"))}; !slices.Equal(got, want) {
		t.Errorf("server_day joined with its llm rows %v, want %v", got, want)
	}
}

func TestUnknownCollectionLinesAreNotErrors(t *testing.T) {
	e := newEnv(t)
	sd := metricsfile.FamilyServerDaily
	e.appendM(sd, "2026-10", mline("snapshot", ts0, map[string]any{"schema": 2, "day": "2026-10-30", "ok": true, "world": 3})+
		mline("future", ts0, map[string]any{"whatever": "text that is never read"}))
	e.appendM(sd, "2026-13", mline("snapshot", ts0, map[string]any{"day": "2026-10-30", "ok": true}))
	e.appendM("future-family", "2026-10", mline("snapshot", ts0, map[string]any{"day": "2026-10-30", "ok": true}))
	st := e.ingest()
	if st.Errors != 0 || st.Lines != 2 {
		t.Errorf("ingest %+v, want 2 lines, no error", st)
	}
	e.checkRows("metrics_other", `"offset"`, []map[string]any{
		{"ts": ms0, "family": sd, "kind": "snapshot"},
		{"ts": ms0, "family": sd, "kind": "future"},
	})
	if n := e.count("ingest_errors") + e.count("server_snapshots"); n != 0 {
		t.Errorf("%d ingest errors and snapshots, want none", n)
	}
	if got := queryStrings(t, e.ix.sh.w, `SELECT source_file FROM cursors ORDER BY source_file`); !slices.Equal(got, []string{mrel(sd, "2026-10")}) {
		t.Errorf("cursors %v: the bad month and the unknown family are not sources", got)
	}
	if hits := textAnywhere(t, e.ix, "never read"); len(hits) != 0 {
		t.Errorf("a field of an unknown kind was kept: %v", hits)
	}
}

func TestCollectionGuards(t *testing.T) {
	e := newEnv(t)
	sd, dh, md := metricsfile.FamilyServerDaily, metricsfile.FamilyDoctorHistory, metricsfile.FamilyMemoryDocs
	e.appendM(md, "2026-10",
		mline("memory", ts0, map[string]any{"memory": "m 1", "outcome": "found", "doc_kind": "none", "created_day": "2026-10-28", "fact_type": "world"})+
			mline("memory", ts0, map[string]any{"memory": "m2", "outcome": "lost", "doc_kind": "note", "document": "note:/home/someone/notes.md",
				"session": "s 1", "created_day": "2026-13-01", "fact_type": "weird"})+
			mline("op", ts0, map[string]any{"operation": "op-1", "document": "conversation:a b", "session": "sess-1", "status": "weird",
				"created_at": "yesterday", "updated_at": "2026-10-30T10:00:00Z"})+
			mline("listed", ts0, map[string]any{"day": "yesterday", "n": 3})+
			mline("listed", ts0, map[string]any{"day": "2026-02-30", "n": 3}))
	e.appendM(dh, "2026-10", mline("run", ts0, map[string]any{"checks": map[string]any{"bad id": "ok", "server": "weird", "index": "warn"}}))
	e.appendM(sd, "2026-10",
		mline("snapshot", ts0, map[string]any{"day": "2026-10-30", "ok": false, "error": "connection refused by 10.0.0.1",
			"last_consolidated_at": "yesterday", "ops": map[string]any{"pending": 1, "queued": 4},
			"projects": map[string]any{"ok-project": 2, "bell\u0007": 3}})+
			mline("snapshot", ts0, map[string]any{"day": "30/10/2026", "ok": true})+
			mline("day", ts0, map[string]any{"bucket": "yesterday"})+
			mline("day", ts0, map[string]any{"bucket": "2026-10-28T00:00:00Z", "llm": map[string]any{
				"retain": llmDay(100, 10, map[string]int64{"success": 3, "timeout": 1, "busy": 2}),
				"weird":  llmDay(200, 20, nil), "strange": llmDay(300, 30, map[string]int64{"success": 1}),
			}}))
	st := e.ingest()
	if st.Errors != 5 {
		t.Errorf("ingest %+v, want 5 errors", st)
	}
	off := func(family string, i int) int64 {
		t.Helper()
		data, err := os.ReadFile(e.mpath(family, "2026-10"))
		if err != nil {
			t.Fatal(err)
		}
		var o int64
		for range i {
			o += int64(strings.IndexByte(string(data[o:]), '\n') + 1)
		}
		return o
	}
	var errs []string
	for _, r := range e.rowsOf(`SELECT source_file, "offset", reason FROM ingest_errors ORDER BY source_file, "offset"`) {
		errs = append(errs, fmt.Sprintf("%s@%d:%s", r["source_file"], r["offset"], r["reason"]))
	}
	want := []string{
		fmt.Sprintf("%s@%d:missing_field", mrel(md, "2026-10"), off(md, 0)), // an id with a space: no memory
		fmt.Sprintf("%s@%d:missing_field", mrel(md, "2026-10"), off(md, 3)), // listed day "yesterday"
		fmt.Sprintf("%s@%d:missing_field", mrel(md, "2026-10"), off(md, 4)), // listed day 2026-02-30
		fmt.Sprintf("%s@%d:missing_field", mrel(sd, "2026-10"), off(sd, 1)), // snapshot day not a date
		fmt.Sprintf("%s@%d:missing_field", mrel(sd, "2026-10"), off(sd, 2)), // bucket not a time
	}
	slices.Sort(want)
	if !slices.Equal(errs, want) {
		t.Errorf("ingest_errors\n got %v\nwant %v", errs, want)
	}
	e.checkRows("memory_docs", `"offset"`, []map[string]any{{
		"ts": ms0, "memory_id": "m2", "outcome": "other", "doc_kind": "note", "document": nil, "session": nil,
		"created_day": nil, "fact_type": "other",
	}})
	e.checkRows("doc_ops", `"offset"`, []map[string]any{{
		"ts": ms0, "operation_id": "op-1", "document": nil, "session": "sess-1", "status": "other",
		"created_at": nil, "updated_at": mustMS("2026-10-30T10:00:00Z"), "retries": nil, "units": nil, "no_units": 0,
	}})
	e.checkRows("doctor_runs", "check_id", []map[string]any{
		{"ts": ms0, "check_id": "index", "status": "warn"},
		{"ts": ms0, "check_id": "server", "status": "other"},
	})
	rs := e.rowsOf(`SELECT error, last_consolidated_at, ops_pending, ops_other FROM server_snapshots`)
	if len(rs) != 1 || rs[0]["error"] != "other" || rs[0]["last_consolidated_at"] != nil || rs[0]["ops_pending"] != int64(1) || rs[0]["ops_other"] != nil {
		t.Errorf("snapshot %v, want error other, no time, pending 1, other NULL (an unknown op key is not other's count)", rs)
	}
	e.checkRows("server_snapshot_projects", "project", []map[string]any{{"ts": ms0, "day": "2026-10-30", "project": "ok-project", "memories": 2}})
	b := mustMS("2026-10-28T00:00:00Z")
	e.checkRows("server_day_llm", "operation", []map[string]any{
		{"ts": ms0, "bucket": b, "operation": "other", "input": 500, "output": 50, "cached": 50, "thoughts": 5, "requests": 5},
		{"ts": ms0, "bucket": b, "operation": "retain", "input": 100, "output": 10, "cached": 10, "thoughts": 1, "requests": 1},
	})
	e.checkRows("server_day_llm_status", "operation, status", []map[string]any{
		{"ts": ms0, "bucket": b, "operation": "other", "status": "success", "n": 1},
		{"ts": ms0, "bucket": b, "operation": "retain", "status": "other", "n": 3},
		{"ts": ms0, "bucket": b, "operation": "retain", "status": "success", "n": 3},
	})
	for _, s := range []string{"10.0.0.1", "refused", "/home/someone", "a b", "s 1", "yesterday", "weird", "strange", "timeout", "busy", "queued"} {
		if hits := textAnywhere(t, e.ix, s); len(hits) != 0 {
			t.Errorf("%q reached the index: %v", s, hits)
		}
	}
}

func TestShowSetsFromShowLines(t *testing.T) {
	e := newEnv(t)
	e.appendJ(feedback.Event{TS: jT0, Kind: "shows", Turn: "t1", Agent: "sub-1", Path: "prompt", Flags: []string{feedback.FlagSetChanged},
		Items: []feedback.Item{{ID: "a", Rank: 1}, {ID: "b", Rank: 2}, {ID: "c", Rank: 3}}})
	e.appendJ(feedback.Event{TS: jT0.Add(time.Second), Kind: "shows", Turn: "t2", Path: "search"})
	e.appendJ(feedback.Event{TS: jT0.Add(2 * time.Second), Kind: feedback.KindRecall, Turn: "t2", Path: "search", Items: []feedback.Item{{ID: "a"}}})
	e.ingest()
	if n := e.count("shows"); n != 3 {
		t.Errorf("%d shows rows, want 3", n)
	}
	e.checkRows("show_sets", `"offset"`, []map[string]any{
		{"ts": ms(jT0), "session": "s1", "turn": "t1", "agent": "sub-1", "path": "prompt", "flags": "set_changed"},
		{"ts": ms(jT0.Add(time.Second)), "session": "s1", "turn": "t2", "agent": nil, "path": "search", "flags": nil},
	})
}

// TestRotatedCollectionRowsAreKept: a month file of a family removed (the
// rotation) keeps its rows, and a rebuild's plan names the month lost.
func TestRotatedCollectionRowsAreKept(t *testing.T) {
	r := newRecEnv(t)
	md := metricsfile.FamilyMemoryDocs
	writeFile(t, filepath.Join(r.d.MetricsDir(), md+"-2025-10.jsonl"),
		mline("memory", "2025-10-03T01:00:00.000Z", map[string]any{"memory": "m1", "outcome": "found", "doc_kind": "none", "created_day": "2025-10-02", "fact_type": "world"})+
			mline("listed", "2025-10-03T01:00:00.000Z", map[string]any{"day": "2025-10-02", "n": 1}))
	writeFile(t, filepath.Join(r.d.MetricsDir(), md+"-2025-11.jsonl"),
		mline("listed", "2025-11-03T01:00:00.000Z", map[string]any{"day": "2025-11-02", "n": 0}))
	ix := r.openUp()
	if err := os.Remove(filepath.Join(r.d.MetricsDir(), md+"-2025-10.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.CatchUp(context.Background(), ViewBudget); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ix.sh.r.QueryRow(`SELECT count(*) FROM memory_doc WHERE memory_id = 'm1' AND created_day = '2025-10-02'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("memory_doc of the removed month: %d rows, err %v", n, err)
	}
	if got := rowsOfSource(t, ix, mrel(md, "2025-10")); got != 2 {
		t.Errorf("%d rows of the removed month kept, want 2", got)
	}
	mustClose(t, ix)
	res, err := r.rebuild(RebuildOptions{Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []LostSource{{SourceFile: mrel(md, "2025-10"), Month: "2025-10", Rows: 2}}; !slices.Equal(res.Lost, want) || !res.NeedsYes {
		t.Errorf("plan lost %v (needs yes %v), want %v", res.Lost, res.NeedsYes, want)
	}
}

// copyState copies the month files of src's telemetry, feedback journal
// and metrics directories into a new state directory.
func copyState(t *testing.T, src paths.Dirs) paths.Dirs {
	t.Helper()
	d := tempDirs(t)
	for _, k := range sourceKinds {
		ents, err := os.ReadDir(k.dir(src))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, ent := range ents {
			if !strings.HasPrefix(ent.Name(), k.prefix) {
				continue
			}
			b, err := os.ReadFile(filepath.Join(k.dir(src), ent.Name()))
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(k.dir(d), ent.Name()), string(b))
		}
	}
	return d
}

// TestIncrementalEqualsRebuildWithCollection: generated state with the
// collection families, ingested in growing prefixes cut at arbitrary
// bytes, equals a fresh build of the whole files, the new tables
// included.
func TestIncrementalEqualsRebuildWithCollection(t *testing.T) {
	src := tempDirs(t)
	indexgen.Generate(t, src.State, indexgen.Spec{Months: 2, TelemetryPerYear: 1200, JournalPerYear: 600, Memories: 300, Seed: 3, Collection: true})
	files := map[string][]byte{}
	for _, k := range sourceKinds {
		ents, err := os.ReadDir(k.dir(src))
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if strings.HasPrefix(ent.Name(), k.prefix) {
				b, err := os.ReadFile(filepath.Join(k.dir(src), ent.Name()))
				if err != nil {
					t.Fatal(err)
				}
				rel, _ := filepath.Rel(src.State, filepath.Join(k.dir(src), ent.Name()))
				files[rel] = b
			}
		}
	}
	e := newEnv(t)
	const cuts = 7
	for i := 1; i <= cuts; i++ {
		for rel, b := range files {
			writeFile(t, filepath.Join(e.d.State, rel), string(b[:len(b)*i/cuts]))
		}
		e.ingest()
	}
	// A collection file cut below its cursor (a reset), then whole again.
	rel := filepath.Join("metrics", metricsfile.FamilyMemoryDocs+"-2026-10.jsonl")
	b, ok := files[rel]
	if !ok {
		t.Fatalf("no %s generated", rel)
	}
	writeFile(t, filepath.Join(e.d.State, rel), string(b[:len(b)/3]))
	e.ingest()
	writeFile(t, filepath.Join(e.d.State, rel), string(b))
	e.ingest()
	if n := e.count("ingest_errors"); n != 0 {
		t.Errorf("%d ingest errors over generated lines", n)
	}
	for _, tbl := range []string{"show_sets", "server_snapshots", "server_snapshot_projects", "server_days", "server_day_llm",
		"server_day_llm_status", "doctor_runs", "memory_docs", "memory_days", "doc_ops", "scans"} {
		if e.count(tbl) == 0 {
			t.Errorf("%s: no row from the generated state", tbl)
		}
	}
	f := envAt(t, copyState(t, e.d))
	f.ingest()
	if got, want := dump(t, e.ix), dump(t, f.ix); got != want {
		t.Errorf("incremental differs from a rebuild (%d and %d bytes of dump)", len(got), len(want))
	}
	rs := e.rowsOf(`SELECT complete_day, covered_until FROM memory_coverage`)
	// The first run lists the day before the first month complete, the
	// last (2026-10-30 01:30) lists 2026-10-29.
	if len(rs) != 1 || rs[0]["complete_day"] != "2026-08-31" || rs[0]["covered_until"] != "2026-10-29" {
		t.Errorf("generated day lists give coverage %v, want complete 2026-08-31, covered until 2026-10-29", rs)
	}
}
