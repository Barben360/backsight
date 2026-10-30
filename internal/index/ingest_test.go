package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// promptLine is a valid prompt line numbered n, its project padded so
// that every line of a test has the same length.
func promptLine(n int, pad int) string {
	return tline("prompt", ts0, map[string]any{"decision": "skip", "n": n, "project": strings.Repeat("p", pad)})
}

// fixedLine is a prompt line of exactly size bytes, newline included.
func fixedLine(n, size int) string {
	l := promptLine(n, 0)
	return promptLine(n, size-len(l))
}

func countBatches(t *testing.T) *int {
	t.Helper()
	n := new(int)
	prev := afterBatch
	afterBatch = func() { *n++ }
	t.Cleanup(func() { afterBatch = prev })
	return n
}

func (e *ingestEnv) ns() []int64 {
	e.t.Helper()
	var out []int64
	for _, r := range e.rowsOf(`SELECT n FROM tel_prompt ORDER BY source_file, "offset"`) {
		out = append(out, r["n"].(int64))
	}
	return out
}

// seq is 1 … to.
func seq(to int64) []int64 {
	var out []int64
	for i := int64(1); i <= to; i++ {
		out = append(out, i)
	}
	return out
}

func TestIngestErrors(t *testing.T) {
	e := newEnv(t)
	long := tline("prompt", ts0, map[string]any{"decision": "skip", "pad": strings.Repeat("a", 1_100_000)})
	lines := []string{
		"not json\n",
		tline("prompt", "", nil),
		tline("prompt", ts0, nil),
		tline("prompt", ts0, map[string]any{"decision": "skip", "n": "x"}),
		long,
		tline("prompt", ts0, map[string]any{"decision": "skip", "n": 9}),
	}
	var offs []int64
	var off int64
	for _, l := range lines {
		offs = append(offs, off)
		off += int64(len(l))
	}
	e.write("2026-10", strings.Join(lines, ""))
	st := e.ingest()
	if st.Lines != 6 || st.Errors != 5 {
		t.Errorf("stats %+v", st)
	}
	rs := e.rowsOf(`SELECT "offset", reason, at, source_file FROM ingest_errors ORDER BY "offset"`)
	want := []string{"unparsable", "no_ts", "missing_field", "bad_type", "too_long"}
	if len(rs) != len(want) {
		t.Fatalf("errors %v", rs)
	}
	for i, r := range rs {
		if r["offset"] != offs[i] || r["reason"] != want[i] || r["at"] != clock.UnixMilli() || r["source_file"] != "telemetry/events-2026-10.jsonl" {
			t.Errorf("error %d: %v, want offset %d reason %s", i, r, offs[i], want[i])
		}
	}
	if got := e.ns(); !slices.Equal(got, []int64{9}) {
		t.Errorf("prompts %v", got)
	}
	if c := e.cursor("2026-10"); c["offset"] != off || c["size"] != off {
		t.Errorf("cursor %v, want offset %d", c, off)
	}
	if st := e.ingest(); st.Lines != 0 || st.Errors != 0 || e.count("ingest_errors") != 5 {
		t.Errorf("rerun %+v", st)
	}
}

// TestBadTypes: a mapped field of the wrong JSON type skips the line.
func TestBadTypes(t *testing.T) {
	cases := []map[string]any{
		{"decision": "skip", "n": 1.5},
		{"decision": "skip", "nudged": "yes"},
		{"decision": "skip", "ids": "m1"},
		{"decision": "skip", "ids": []any{"m1", 2}},
		{"decision": "skip", "scopes": []string{"user"}},
		{"decision": "skip", "scopes": map[string]any{"user": map[string]any{"ok": "y"}}},
		{"decision": 3},
		{"decision": "skip", "session": 7},
		{"decision": "skip", "top": "high"},
	}
	e := newEnv(t)
	var b strings.Builder
	for _, c := range cases {
		b.WriteString(tline("prompt", ts0, c))
	}
	b.WriteString(tline("prompt", ts0, map[string]any{"decision": "skip", "n": 2.0, "top": 1}))
	e.write("2026-10", b.String())
	st := e.ingest()
	if st.Errors != int64(len(cases)) {
		t.Errorf("errors %d, want %d", st.Errors, len(cases))
	}
	for _, r := range e.rowsOf(`SELECT reason FROM ingest_errors`) {
		if r["reason"] != "bad_type" {
			t.Errorf("reason %v", r["reason"])
		}
	}
	if got := e.ns(); !slices.Equal(got, []int64{2}) {
		t.Errorf("prompts %v", got)
	}
}

// TestMissingFields: a known kind without a required field is skipped.
func TestMissingFields(t *testing.T) {
	lines := []string{
		tline("prompt", ts0, map[string]any{"decision": ""}),
		tline("prompt", ts0, map[string]any{"decision": nil}),
		tline("tool", ts0, nil),
		tline("tool_use", ts0, nil),
		tline("retain", ts0, map[string]any{"status": "ok"}),
		tline("retain", ts0, map[string]any{"document": "d"}),
		tline("gate", ts0, nil),
		tline("credit", ts0, nil),
		tline("credit_check", ts0, nil),
		tline("job", ts0, map[string]any{"name": "tick"}),
		tline("job", ts0, map[string]any{"status": "done"}),
		tline("hook_failure", ts0, nil),
		tline("retire", ts0, nil),
		tline("store", ts0, nil),
		tline("dashboard", ts0, nil),
		tline("", ts0, nil), // no event
	}
	e := newEnv(t)
	e.write("2026-10", strings.Join(lines, "")+tline("outcome", ts0, nil)+tline("feedback_settle", ts0, nil))
	st := e.ingest()
	if st.Errors != int64(len(lines)) || st.Lines != int64(len(lines))+2 {
		t.Errorf("stats %+v", st)
	}
	for _, r := range e.rowsOf(`SELECT reason FROM ingest_errors`) {
		if r["reason"] != "missing_field" {
			t.Errorf("reason %v", r["reason"])
		}
	}
	if e.count("tel_outcome") != 1 || e.count("tel_feedback_settle") != 1 {
		t.Error("kinds without required fields were refused")
	}
}

func TestPartialLastLineWaits(t *testing.T) {
	e := newEnv(t)
	a := promptLine(1, 3)
	b := promptLine(2, 3)
	e.write("2026-10", a+strings.TrimSuffix(b, "\n")) // a whole object, but no newline yet
	e.ingest()
	if got := e.ns(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("prompts %v", got)
	}
	if c := e.cursor("2026-10"); c["offset"] != int64(len(a)) {
		t.Errorf("offset %v, want %d", c["offset"], len(a))
	}
	e.append("2026-10", "\n")
	e.ingest()
	e.ingest()
	if got := e.ns(); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("prompts %v", got)
	}
	if e.count("ingest_errors") != 0 {
		t.Error("errors")
	}
}

func TestTruncatedFileIsReingested(t *testing.T) {
	t.Run("rewritten shorter", func(t *testing.T) {
		e := newEnv(t)
		var b strings.Builder
		for i := 1; i <= 10; i++ {
			b.WriteString(promptLine(i, 5))
		}
		e.write("2026-10", b.String())
		e.ingest()
		e.write("2026-10", promptLine(101, 5)+promptLine(102, 5)+promptLine(103, 5))
		e.ingest()
		if got := e.ns(); !slices.Equal(got, []int64{101, 102, 103}) {
			t.Errorf("prompts %v", got)
		}
	})
	t.Run("cut below the offset, head unchanged", func(t *testing.T) {
		e := newEnv(t)
		var b strings.Builder
		for i := 1; i <= 10; i++ {
			b.WriteString(fixedLine(i, 1000))
		}
		b.WriteString("garbage\n")
		full := b.String()
		e.write("2026-10", full)
		e.ingest()
		if e.count("ingest_errors") != 1 {
			t.Fatal("no error recorded")
		}
		e.write("2026-10", full[:5000]) // the first 4096 bytes are the same
		st := e.ingest()
		if got := e.ns(); !slices.Equal(got, seq(5)) {
			t.Errorf("prompts %v", got)
		}
		if st.Lines != 5 {
			t.Errorf("read %d lines, want the 5 left", st.Lines)
		}
		if e.count("ingest_errors") != 0 {
			t.Error("the error of a removed line was kept")
		}
		if c := e.cursor("2026-10"); c["offset"] != int64(5000) || c["size"] != int64(5000) {
			t.Errorf("cursor %v", c)
		}
	})
}

func TestReplacedFileIsReingested(t *testing.T) {
	e := newEnv(t)
	e.write("2026-10", promptLine(1, 5)+promptLine(2, 5))
	e.ingest()
	e.write("2026-10", promptLine(3, 5)+promptLine(2, 5)) // same size
	e.ingest()
	if got := e.ns(); !slices.Equal(got, []int64{3, 2}) {
		t.Errorf("prompts %v", got)
	}
}

func TestSmallFileGrowingHead(t *testing.T) {
	e := newEnv(t)
	first := fixedLine(1, 200)
	e.write("2026-10", first)
	e.ingest()
	c := e.cursor("2026-10")
	if c["head_len"] != int64(200) || c["size"] != int64(200) {
		t.Fatalf("cursor %v", c)
	}
	var b strings.Builder
	for i := 2; i <= 25; i++ {
		b.WriteString(fixedLine(i, 200))
	}
	e.append("2026-10", b.String())
	if st := e.ingest(); st.Lines != 24 {
		t.Errorf("read %d lines after growth, want 24 (no reset)", st.Lines)
	}
	if got := e.ns(); !slices.Equal(got, seq(25)) {
		t.Errorf("prompts %v", got)
	}
	if c := e.cursor("2026-10"); c["head_len"] != int64(4096) || c["size"] != int64(5000) {
		t.Errorf("cursor %v", c)
	}
	// Byte 100 sits in the first line's project.
	data, err := os.ReadFile(e.telPath("2026-10"))
	if err != nil {
		t.Fatal(err)
	}
	if data[100] != 'p' {
		t.Fatalf("byte 100 is %q", data[100])
	}
	data[100] = 'q'
	e.write("2026-10", string(data))
	if st := e.ingest(); st.Lines != 25 {
		t.Errorf("read %d lines after the head changed, want 25", st.Lines)
	}
	if got := e.ns(); !slices.Equal(got, seq(25)) {
		t.Errorf("prompts %v", got)
	}
	rs := e.rowsOf(`SELECT project FROM tel_prompt WHERE "offset" = 0`)
	if !strings.Contains(rs[0]["project"].(string), "q") {
		t.Error("the first line's row is the old content's")
	}
}

func TestVanishedFileKeepsItsRows(t *testing.T) {
	e := newEnv(t)
	content := promptLine(1, 1) + promptLine(2, 1) + promptLine(3, 1)
	e.write("2026-10", content)
	e.write("2026-09", promptLine(9, 1))
	e.ingest()
	if err := os.Remove(e.telPath("2026-10")); err != nil {
		t.Fatal(err)
	}
	e.ingest()
	if got := e.ns(); !slices.Equal(got, []int64{9, 1, 2, 3}) {
		t.Errorf("prompts %v", got)
	}
	if c := e.cursor("2026-10"); c["missing"] != int64(1) {
		t.Errorf("cursor %v", c)
	}
	if c := e.cursor("2026-09"); c["missing"] != int64(0) {
		t.Errorf("cursor %v", c)
	}
	n := countBatches(t)
	e.ingest()
	if *n != 0 {
		t.Errorf("%d commits for a file already missing", *n)
	}

	e.write("2026-10", content) // back, unchanged
	if st := e.ingest(); st.Lines != 0 {
		t.Errorf("read %d lines of an unchanged file", st.Lines)
	}
	if got := e.ns(); !slices.Equal(got, []int64{9, 1, 2, 3}) {
		t.Errorf("prompts %v", got)
	}
	if c := e.cursor("2026-10"); c["missing"] != int64(0) {
		t.Errorf("cursor %v", c)
	}

	if err := os.Remove(e.telPath("2026-10")); err != nil {
		t.Fatal(err)
	}
	e.ingest()
	e.write("2026-10", promptLine(4, 1)+promptLine(5, 1)) // back, changed
	e.ingest()
	if got := e.ns(); !slices.Equal(got, []int64{9, 4, 5}) {
		t.Errorf("prompts %v", got)
	}

	// A directory in its place: gone too, not an error on every run.
	if err := os.Remove(e.telPath("2026-10")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.telPath("2026-10"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.ingest()
	if c := e.cursor("2026-10"); c["missing"] != int64(1) {
		t.Errorf("cursor %v", c)
	}
	if got := e.ns(); !slices.Equal(got, []int64{9, 4, 5}) {
		t.Errorf("prompts %v", got)
	}
}

func TestBatchesOfAtMost5000Lines(t *testing.T) {
	var b strings.Builder
	var end5000 int64
	for i := 1; i <= 12_001; i++ {
		b.WriteString(fixedLine(i, 120))
		if i == 5000 {
			end5000 = int64(b.Len())
		}
	}
	content := b.String()

	t.Run("three commits", func(t *testing.T) {
		e := newEnv(t)
		e.write("2026-10", content)
		n := countBatches(t)
		st := e.ingest()
		if *n != 3 {
			t.Errorf("%d commits, want 3", *n)
		}
		if st.Lines != 12_001 || e.count("tel_prompt") != 12_001 {
			t.Errorf("stats %+v, rows %d", st, e.count("tel_prompt"))
		}
	})
	t.Run("deadline after the first", func(t *testing.T) {
		e := newEnv(t)
		e.write("2026-10", content)
		prev := afterBatch
		afterBatch = func() { e.clock.Add(time.Hour) }
		t.Cleanup(func() { afterBatch = prev })
		st, err := e.ix.ingestSources(context.Background(), clock.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if !st.More || st.Lines != 5000 {
			t.Errorf("stats %+v", st)
		}
		if n := e.count("tel_prompt"); n != 5000 {
			t.Errorf("%d rows", n)
		}
		if c := e.cursor("2026-10"); c["offset"] != end5000 {
			t.Errorf("offset %v, want %d", c["offset"], end5000)
		}
		afterBatch = prev
		e.ingest()
		if got := e.ns(); !slices.Equal(got, seq(12_001)) {
			t.Errorf("%d prompts after the catch-up", len(got))
		}
	})
	t.Run("deadline already passed", func(t *testing.T) {
		e := newEnv(t)
		e.write("2026-10", promptLine(1, 1))
		st, err := e.ix.ingestSources(context.Background(), clock)
		if err != nil {
			t.Fatal(err)
		}
		if !st.More || e.count("tel_prompt") != 0 {
			t.Errorf("stats %+v", st)
		}
	})
}

func TestRerunIsANoOp(t *testing.T) {
	e := newEnv(t)
	e.write("2026-10", promptLine(1, 1)+"junk\n"+tline("failure_recall", ts0, map[string]any{"ms": 2})+"tail")
	e.ingest()
	before := dump(t, e.ix)
	at := e.cursor("2026-10")["ingested_at"]
	e.clock.Add(time.Hour)
	n := countBatches(t)
	if st := e.ingest(); st.Lines != 0 || st.Errors != 0 {
		t.Errorf("stats %+v", st)
	}
	if *n != 0 {
		t.Errorf("%d commits on a rerun", *n)
	}
	if after := dump(t, e.ix); after != before {
		t.Errorf("rerun changed the index\nbefore %s\nafter %s", before, after)
	}
	if got := e.cursor("2026-10")["ingested_at"]; got != at {
		t.Errorf("ingested_at %v, was %v", got, at)
	}
}

func TestIncrementalEqualsRebuild(t *testing.T) {
	e := newEnv(t)
	k := 0
	round := func(r int) string {
		var b strings.Builder
		for i := 0; i < 7; i++ {
			k++
			ts := time.UnixMilli(ms0).UTC().Add(time.Duration(k) * time.Minute).Format("2006-01-02T15:04:05.000Z")
			switch i {
			case 0:
				b.WriteString(tline("prompt", ts, withCommon(map[string]any{"n": k, "decision": "recall", "ids": []string{"a", "b"}, "scopes": map[string]any{"user": map[string]any{"ok": true}}})))
			case 1:
				b.WriteString(tline("tool_use", ts, withCommon(map[string]any{"name": "Read", "ids": 1, "memory_ids": []string{"c"}, "n": k})))
			case 2:
				b.WriteString(tline("failure_recall", ts, withCommon(map[string]any{"decision": "skip", "ms": k})))
			case 3:
				fmt.Fprintf(&b, "broken %d\n", k)
			case 4:
				b.WriteString(tline("config", ts, withCommon(map[string]any{"sha256": "ff", "recall.gap": k})))
			default:
				b.WriteString(tline("retain", ts, withCommon(map[string]any{"document": fmt.Sprintf("d%d", r), "status": "ok", "items": 1})))
			}
		}
		return b.String()
	}
	for r := 0; r < 6; r++ {
		e.append("2026-10", round(r))
		e.append("2026-09", round(r))
		e.ingest()
	}
	// A partial tail, completed later.
	tail := round(6)
	e.append("2026-10", tail[:len(tail)-30])
	e.ingest()
	e.append("2026-10", tail[len(tail)-30:])
	e.ingest()
	// A truncation of the other month.
	data, err := os.ReadFile(e.telPath("2026-09"))
	if err != nil {
		t.Fatal(err)
	}
	e.write("2026-09", string(data[:len(data)/2]))
	e.ingest()
	e.append("2026-09", "\n"+round(7))
	e.ingest()
	// A replacement of the same size.
	data, err = os.ReadFile(e.telPath("2026-10"))
	if err != nil {
		t.Fatal(err)
	}
	data[2] = 'X' // the first key's name: the line stays JSON
	e.write("2026-10", string(data))
	e.ingest()

	got, want := dump(t, e.ix), freshDump(t, e)
	if got != want {
		t.Errorf("incremental differs from a rebuild\n got:\n%s\nwant:\n%s", got, want)
	}
	if e.count("tel_prompt") == 0 || e.count("ingest_errors") == 0 || e.count("events") == 0 || e.count("tel_ids") == 0 {
		t.Error("the rounds did not fill the tables")
	}
}

// TestConcurrentAppendDuringIngest: lines appended while the ingest runs
// are each ingested once.
func TestConcurrentAppendDuringIngest(t *testing.T) {
	e := newEnv(t)
	e.write("2026-10", "")
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		f, err := os.OpenFile(e.telPath("2026-10"), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = f.Close() }()
		for i := 1; i <= 2000; i++ {
			if _, err := f.Write([]byte(promptLine(i, i%50))); err != nil {
				t.Error(err)
				return
			}
			if i%100 == 0 {
				time.Sleep(time.Millisecond) // let ingests run between appends
			}
		}
	}()
	runs := 0
	for running := true; running; runs++ {
		select {
		case <-done:
			running = false
		default:
		}
		if _, err := e.ix.ingestSources(context.Background(), time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if runs < 2 {
		t.Logf("only %d ingests overlapped the appends", runs)
	}
	e.ingest()
	if got := e.ns(); !slices.Equal(got, seq(2000)) {
		t.Errorf("%d prompts", len(got))
	}
	if got, want := dump(t, e.ix), freshDump(t, e); got != want {
		t.Error("the concurrent ingest differs from a rebuild")
	}
}

func TestGarbageInAMonthFile(t *testing.T) {
	e := newEnv(t)
	nul := strings.Repeat("\x00", 4096) + "\n"
	big := `{"event":"prompt","ts":"` + ts0 + `","decision":"skip","pad":"` + strings.Repeat("b", 2<<20) + "\"}\n"
	bad := `{"event":"prompt","ts":"` + ts0 + `","decision":"skip","x":"` + "\xff\xfe" + "\"}\n"
	valid := promptLine(1, 0) + promptLine(2, 0) + promptLine(3, 0)
	e.write("2026-10", nul+big+bad+valid)
	st := e.ingest()
	if st.Errors != 3 || st.Lines != 6 {
		t.Errorf("stats %+v", st)
	}
	rs := e.rowsOf(`SELECT "offset", reason FROM ingest_errors ORDER BY "offset"`)
	want := []struct {
		off    int64
		reason string
	}{{0, "unparsable"}, {int64(len(nul)), "too_long"}, {int64(len(nul) + len(big)), "unparsable"}}
	if len(rs) != len(want) {
		t.Fatalf("errors %v", rs)
	}
	for i, w := range want {
		if rs[i]["offset"] != w.off || rs[i]["reason"] != w.reason {
			t.Errorf("error %d: %v, want %v", i, rs[i], w)
		}
	}
	if got := e.ns(); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("prompts %v", got)
	}
}

func TestOnlyMonthFilesAreRead(t *testing.T) {
	e := newEnv(t)
	line := promptLine(1, 1)
	for _, name := range []string{"events-2026-10.jsonl.bak", "events-2026-1.jsonl", "events-2026-10 (1).jsonl", "events-2026-13.jsonl", "events-2026-00.jsonl", "journal-2026-10.jsonl", "Events-2026-10.jsonl"} {
		writeFile(t, filepath.Join(e.d.TelemetryDir(), name), line)
	}
	if err := os.MkdirAll(filepath.Join(e.d.TelemetryDir(), "events-2026-08.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.ingest()
	if n := e.count("cursors"); n != 0 {
		t.Errorf("%d cursors", n)
	}
	if n := e.count("tel_prompt"); n != 0 {
		t.Errorf("%d rows", n)
	}

	for _, m := range []string{"2026-09", "2025-12", "2026-10"} {
		e.write(m, line)
	}
	srcs, err := e.ix.sources()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range srcs {
		got = append(got, s.Kind+":"+s.Rel)
		if s.Path != filepath.Join(e.d.State, filepath.FromSlash(s.Rel)) {
			t.Errorf("path %s for %s", s.Path, s.Rel)
		}
	}
	want := []string{"telemetry:telemetry/events-2026-10.jsonl", "telemetry:telemetry/events-2026-09.jsonl", "telemetry:telemetry/events-2025-12.jsonl"}
	if !slices.Equal(got, want) {
		t.Errorf("sources\n got %v\nwant %v", got, want)
	}
}

// TestRowTablesAreEveryLineTable: rowTables names each table whose rows
// come from a source line, so a reset and the pruning miss none.
func TestRowTablesAreEveryLineTable(t *testing.T) {
	e := newEnv(t)
	var want []string
	for _, r := range e.rowsOf(`SELECT m.name AS name FROM sqlite_schema m, pragma_table_info(m.name) p
		WHERE m.type = 'table' AND p.name = 'source_file' AND m.name NOT IN ('cursors', 'ingest_errors')`) {
		want = append(want, r["name"].(string))
	}
	got := slices.Clone(rowTables)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("rowTables\n got %v\nwant %v", got, want)
	}
}

// unreadable is the source files with an unreadable row.
func (e *ingestEnv) unreadable() []string {
	e.t.Helper()
	var out []string
	for _, r := range e.rowsOf(`SELECT source_file FROM ingest_errors WHERE reason = 'unreadable' AND "offset" = -1 ORDER BY source_file`) {
		out = append(out, r["source_file"].(string))
	}
	return out
}

// TestUnreadableFileDoesNotBlockOthers: a file that cannot be read is
// recorded once and skipped; the other files are ingested.
func TestUnreadableFileDoesNotBlockOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode 000 does not stop reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	e := newEnv(t)
	e.write("2026-10", promptLine(10, 1)) // newest: read first
	e.write("2026-09", promptLine(9, 1))
	if err := os.Chmod(e.telPath("2026-10"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.telPath("2026-10"), 0o600) })
	st := e.ingest()
	if st.Unreadable != 1 || st.Lines != 1 {
		t.Errorf("stats %+v", st)
	}
	if got := e.ns(); !slices.Equal(got, []int64{9}) {
		t.Errorf("prompts %v", got)
	}
	want := []string{"telemetry/events-2026-10.jsonl"}
	if got := e.unreadable(); !slices.Equal(got, want) {
		t.Errorf("unreadable %v", got)
	}
	n := countBatches(t)
	if st := e.ingest(); st.Unreadable != 1 {
		t.Errorf("rerun %+v", st)
	}
	if *n != 0 {
		t.Errorf("%d commits on a rerun", *n)
	}
	if got := e.unreadable(); !slices.Equal(got, want) || e.count("ingest_errors") != 1 {
		t.Errorf("unreadable %v, %d errors", got, e.count("ingest_errors"))
	}

	if err := os.Chmod(e.telPath("2026-10"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := e.ingest(); st.Unreadable != 0 || st.Lines != 1 {
		t.Errorf("after the fix %+v", st)
	}
	if got := e.ns(); !slices.Equal(got, []int64{9, 10}) {
		t.Errorf("prompts %v", got)
	}
	if got := e.unreadable(); len(got) != 0 {
		t.Errorf("unreadable kept: %v", got)
	}
}

// TestUnknownKindIsUnreadable: a cursor of a kind this binary does not
// read is skipped like an unreadable file, its rows kept.
func TestUnknownKindIsUnreadable(t *testing.T) {
	e := newEnv(t)
	e.write("2026-10", promptLine(1, 1))
	e.ingest()
	prev := sourceKinds
	sourceKinds = nil
	t.Cleanup(func() { sourceKinds = prev })
	if st := e.ingest(); st.Unreadable != 1 {
		t.Errorf("stats %+v", st)
	}
	if got := e.unreadable(); !slices.Equal(got, []string{"telemetry/events-2026-10.jsonl"}) {
		t.Errorf("unreadable %v", got)
	}
	sourceKinds = prev
	if st := e.ingest(); st.Unreadable != 0 || st.Lines != 0 {
		t.Errorf("stats %+v", st)
	}
	if got := e.unreadable(); len(got) != 0 {
		t.Errorf("unreadable kept: %v", got)
	}
	if got := e.ns(); !slices.Equal(got, []int64{1}) {
		t.Errorf("prompts %v", got)
	}
}

// TestStaleRowsWithoutACursorAreReset: a file whose cursor is gone (the
// daily prune drops it once the file is gone and its rows pruned) while
// its ingest errors (kept 30 days) or rows survive is read as a new
// file when it comes back: its old rows are deleted first, and the
// result equals a fresh build.
func TestStaleRowsWithoutACursorAreReset(t *testing.T) {
	content := "CANARY-not-json-line\n" + promptLine(1, 3) + "another-bad-line\n" + promptLine(2, 3)
	for _, tc := range []struct{ name, drop string }{
		{"error rows left", `DELETE FROM cursors; DELETE FROM tel_prompt`},
		{"error and line rows left", `DELETE FROM cursors`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.write("2026-10", content)
			e.ingest()
			if e.count("ingest_errors") != 2 {
				t.Fatalf("%d errors recorded, want 2", e.count("ingest_errors"))
			}
			if _, err := e.ix.sh.w.Exec(tc.drop); err != nil {
				t.Fatal(err)
			}
			st, err := e.ix.ingestSources(context.Background(), time.Time{})
			if err != nil {
				t.Fatalf("ingest with stale rows: %v", err)
			}
			if st.Lines != 4 || st.Errors != 2 {
				t.Errorf("stats %+v, want 4 lines and 2 errors", st)
			}
			if got, want := dump(t, e.ix), freshDump(t, e); got != want {
				t.Errorf("differs from a fresh build\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestAStaleErrorRowAheadOfTheCursorIsKept: an ingest_errors row already
// at the offset of a bad line being read is no conflict: the line is
// skipped as an error and the ingest goes on.
func TestAStaleErrorRowAheadOfTheCursorIsKept(t *testing.T) {
	e := newEnv(t)
	first := promptLine(1, 3)
	e.write("2026-10", first)
	e.ingest()
	if _, err := e.ix.sh.w.Exec(`INSERT INTO ingest_errors(source_file, "offset", reason, at) VALUES (?, ?, 'unparsable', 0)`,
		"telemetry/events-2026-10.jsonl", len(first)); err != nil {
		t.Fatal(err)
	}
	e.append("2026-10", "CANARY-bad-line\n"+promptLine(2, 3))
	st, err := e.ix.ingestSources(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("ingest over a stale error row: %v", err)
	}
	if got := e.ns(); !slices.Equal(got, []int64{1, 2}) || st.Lines != 2 {
		t.Errorf("prompts %v, stats %+v", got, st)
	}
	if e.count("ingest_errors") != 1 {
		t.Errorf("%d error rows, want 1", e.count("ingest_errors"))
	}
}
