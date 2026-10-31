package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// indexCanary is the session of the telemetry lines written before a
// tick: the index holds them once the tick ingested.
const indexCanary = "sess-index-canary-0042"

// writeIndexLines appends n prompt lines of indexCanary to the month file.
func writeIndexLines(t *testing.T, d paths.Dirs, month string, n int, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(d.TelemetryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(d.TelemetryDir(), "events-"+month+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var b strings.Builder
	for i := 0; i < n; i++ {
		raw, _ := json.Marshal(map[string]any{"event": "prompt", "ts": at.UTC().Format(time.RFC3339Nano), "session": indexCanary, "decision": "skip", "n": i})
		b.Write(raw)
		b.WriteByte('\n')
	}
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
}

// indexCount counts rows of the index under home with a query.
func indexCount(t *testing.T, home, q string, args ...any) int {
	t.Helper()
	ix, err := index.Open(context.Background(), indexOptions(pathsOf(home), home, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()
	var n int
	if err := ix.ReadTx(context.Background(), func(tx *sql.Tx) error { return tx.QueryRow(q, args...).Scan(&n) }); err != nil {
		t.Fatal(err)
	}
	return n
}

func tickJSON(t *testing.T) (int, tickResult) {
	t.Helper()
	code, out, _ := run(t, "jobs", "tick", "--json")
	var env tickEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("exit %d: %v\n%s", code, err, out)
	}
	return code, env.Data
}

func monthOf(at time.Time) string { return at.UTC().Format("2006-01") }

// TestTickIngestsOnEveryExitPath: whichever way a tick ends, it catches
// the index up before its own job event (the index holds the lines
// written before the tick, and no job row of this tick, which the
// telemetry file holds).
func TestTickIngestsOnEveryExitPath(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, home string)
		code   int
		status string
	}{
		{"a normal tick", func(t *testing.T, home string) {}, output.OK, "done"},
		{"another tick is running", func(t *testing.T, home string) {
			l, err := state.TryAcquire(filepath.Join(pathsOf(home).LocksDir(), "job-tick.lock"), "jobs tick")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Release() })
		}, output.OK, "skipped"},
		{"maintenance in progress", func(t *testing.T, home string) {
			d := pathsOf(home)
			_ = os.MkdirAll(filepath.Dir(d.MaintenanceLock()), 0o700)
			_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":1}`), 0o600)
		}, output.OK, "skipped"},
		{"components.write is off", func(t *testing.T, home string) {
			appendConfig(t, home, "[components]\nwrite = false")
		}, output.OK, "skipped"},
		{"the client cannot be set up", func(t *testing.T, home string) {
			// the sent index is unreadable: newFlusher fails
			if err := os.MkdirAll(filepath.Join(pathsOf(home).SentIndexDir(), "index.bin"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, output.Blocked, "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := devInstall(t, true)
			pointAt(t, home, fakeHindsight(t))
			d := pathsOf(home)
			before := time.Now().Add(-40 * 24 * time.Hour)
			writeIndexLines(t, d, monthOf(before), 3, before)
			c.setup(t, home)
			code, res := tickJSON(t)
			if code != c.code || res.Status != c.status {
				t.Fatalf("exit %d status %q, want %d %q (%+v)", code, res.Status, c.code, c.status, res)
			}
			// the lines written before, and the tick's own events before finish
			if !strings.HasPrefix(res.MetricsIndex, "caught up: ") || !strings.HasSuffix(res.MetricsIndex, " line(s)") {
				t.Errorf("metrics_index %q", res.MetricsIndex)
			}
			if n := indexCount(t, home, `SELECT count(*) FROM tel_prompt WHERE session = ?`, indexCanary); n != 3 {
				t.Errorf("%d prompt row(s) of the lines written before the tick, want 3", n)
			}
			if n := indexCount(t, home, `SELECT count(*) FROM tel_job`); n != 0 {
				t.Errorf("%d job row(s): the ingest ran after the tick's job event", n)
			}
			raw, _ := os.ReadFile(filepath.Join(d.TelemetryDir(), "events-"+monthOf(time.Now())+".jsonl"))
			if !strings.Contains(string(raw), `"event":"job"`) || !strings.Contains(string(raw), `"status":"`+c.status+`"`) {
				t.Errorf("the tick's job event is missing: %s", raw)
			}
		})
	}
}

// A tick whose client cannot be set up says why in a fixed reason, in
// its result and its job event (the errors report and doctor show it),
// never the error's text.
func TestTickClientSetupFailureNamesItsReason(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	if err := os.MkdirAll(filepath.Join(d.SentIndexDir(), "index.bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, res := tickJSON(t)
	if code != output.Blocked || res.Status != "failed" || res.Reason != clientSetupFailed {
		t.Fatalf("exit %d status %q reason %q", code, res.Status, res.Reason)
	}
	raw, _ := os.ReadFile(filepath.Join(d.TelemetryDir(), "events-"+monthOf(time.Now())+".jsonl"))
	if !strings.Contains(string(raw), `"reason":"the client could not be set up"`) || strings.Contains(string(raw), "index.bin") {
		t.Errorf("job event: %s", raw)
	}
}

// The text line names the index step on a skipped tick too.
func TestTickTextNamesTheIndex(t *testing.T) {
	home := devInstall(t, true)
	l, err := state.TryAcquire(filepath.Join(pathsOf(home).LocksDir(), "job-tick.lock"), "jobs tick")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	code, out, errOut := run(t, "jobs", "tick")
	if code != output.OK || out != "skipped: another tick is running, metrics index caught up: 0 line(s)\n" || errOut != "" {
		t.Errorf("exit %d\n%q\n%q", code, out, errOut)
	}
}

// TestTickIndexFailureNeverFailsTheTick: an index that cannot be written
// leaves the tick's status and exit code as they are without it; the
// failure is a class, never a path.
func TestTickIndexFailureNeverFailsTheTick(t *testing.T) {
	tick := func(t *testing.T, broken bool) (int, tickResult) {
		home := devInstall(t, true)
		pointAt(t, home, fakeHindsight(t))
		_, _ = notesOutbox(pathsOf(home)).Append(newTestNote())
		if broken {
			d := pathsOf(home)
			if err := os.MkdirAll(d.State, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(d.IndexDir(), []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return tickJSON(t)
	}
	wantCode, want := tick(t, false)
	code, got := tick(t, true)
	if code != wantCode || got.Status != want.Status || got.Notes.Sent != want.Notes.Sent {
		t.Errorf("exit %d status %q sent %d, want %d %q %d", code, got.Status, got.Notes.Sent, wantCode, want.Status, want.Notes.Sent)
	}
	if got.MetricsIndex != "failed: io" {
		t.Errorf("metrics_index %q, want failed: io", got.MetricsIndex)
	}
	if !strings.HasPrefix(want.MetricsIndex, "caught up: ") {
		t.Errorf("the healthy tick's metrics_index %q", want.MetricsIndex)
	}
}

// A corrupt index is set aside and built again by the tick.
func TestTickSetsACorruptIndexAside(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	writeIndexLines(t, d, "2026-09", 2, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err := os.MkdirAll(d.IndexDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(d.IndexDir(), index.FileName)
	if err := os.WriteFile(main, []byte(strings.Repeat("not a database file ", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, res := tickJSON(t)
	if code != output.OK || !strings.HasPrefix(res.MetricsIndex, "caught up: ") {
		t.Errorf("exit %d metrics_index %q", code, res.MetricsIndex)
	}
	if _, err := os.Stat(main + ".corrupt"); err != nil {
		t.Errorf("not set aside: %v", err)
	}
	if n := indexCount(t, home, `SELECT count(*) FROM tel_prompt WHERE session = ?`, indexCanary); n != 2 {
		t.Errorf("%d row(s) in the new index, want 2", n)
	}
}

// On an index a newer backsight migrated, the tick works on its own side
// file and says so; metrics.db is left as it is.
func TestTickOnANewerIndexUsesItsSideFile(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	tickJSON(t) // builds metrics.db
	main := filepath.Join(d.IndexDir(), index.FileName)
	db, err := sql.Open("sqlite", "file:"+main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	before, _ := os.ReadFile(main)
	_, res := tickJSON(t)
	if want := " (metrics-v1.db: a newer backsight's index is kept)"; !strings.HasPrefix(res.MetricsIndex, "caught up: ") || !strings.HasSuffix(res.MetricsIndex, want) {
		t.Errorf("metrics_index %q", res.MetricsIndex)
	}
	if after, _ := os.ReadFile(main); string(after) != string(before) {
		t.Error("the newer metrics.db changed")
	}
}

// TestTickWaitsForAnUncommittedUpdate: building the index is a migration,
// which waits for the update's commit; the tick goes on.
func TestTickWaitsForAnUncommittedUpdate(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	if err := state.WriteUpdateMarker(d, state.UpdateMarker{From: "v1", To: "v2", Committed: false}); err != nil {
		t.Fatal(err)
	}
	code, res := tickJSON(t)
	if code != output.OK || res.MetricsIndex != "skipped: an update is being applied" {
		t.Errorf("exit %d metrics_index %q", code, res.MetricsIndex)
	}
	if _, err := os.Stat(filepath.Join(d.IndexDir(), index.FileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("metrics.db created during the update: %v", err)
	}
}

// TestTickCheckpointsTheWAL: after a tick the write-ahead log is folded
// into the file and truncated, even while another connection (a
// dashboard) keeps the file open, which stops the close from doing it.
func TestTickCheckpointsTheWAL(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	tickJSON(t) // builds it
	main := filepath.Join(d.IndexDir(), index.FileName)
	other, err := sql.Open("sqlite", "file:"+main)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	var n int
	if err := other.QueryRow(`SELECT count(*) FROM cursors`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	writeIndexLines(t, d, "2026-09", 500, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if _, res := tickJSON(t); !strings.HasPrefix(res.MetricsIndex, "caught up: ") {
		t.Fatalf("metrics_index %q", res.MetricsIndex)
	}
	if info, err := os.Stat(main + "-wal"); err == nil && info.Size() != 0 {
		t.Errorf("metrics.db-wal holds %d bytes after the tick", info.Size())
	}
}

// TestTickBoundsTheIngest: past its budget the tick stops between
// batches, and each next tick goes on from there until caught up. The
// catch-up's clock moves a second at each reading, so that the budget
// runs out after the same work on any machine, however slow.
func TestTickBoundsTheIngest(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	const total = 12_000
	writeIndexLines(t, d, "2026-09", total, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	var mu sync.Mutex
	at := time.Now()
	prevClock, prevBudget := tickIndexClock, tickIndexBudget
	tickIndexClock = func() time.Time { mu.Lock(); defer mu.Unlock(); at = at.Add(time.Second); return at }
	tickIndexBudget = index.TickBudget
	t.Cleanup(func() { tickIndexClock, tickIndexBudget = prevClock, prevBudget })
	rows := func() int {
		return indexCount(t, home, `SELECT count(*) FROM tel_prompt WHERE session = ?`, indexCanary)
	}
	prev := 0
	for tick := 1; ; tick++ {
		code, res := tickJSON(t)
		n := rows()
		if code != output.OK {
			t.Fatalf("tick %d: exit %d metrics_index %q", tick, code, res.MetricsIndex)
		}
		if strings.HasPrefix(res.MetricsIndex, "caught up: ") {
			if n != total {
				t.Errorf("tick %d caught up with %d rows, want %d", tick, n, total)
			}
			if tick == 1 {
				t.Error("one tick read all the lines: the budget did not stop it")
			}
			return
		}
		if !strings.HasPrefix(res.MetricsIndex, "partial: ") || !strings.HasSuffix(res.MetricsIndex, " line(s), the next tick goes on") {
			t.Fatalf("tick %d: metrics_index %q", tick, res.MetricsIndex)
		}
		if n <= prev || n >= total {
			t.Fatalf("tick %d: %d rows after %d: a partial tick must go on from the last one and stop short", tick, n, prev)
		}
		if tick == 10 {
			t.Fatalf("still partial after %d ticks (%d rows)", tick, n)
		}
		prev = n
	}
}

type maintainEnvelope struct {
	Data maintainResult `json:"data"`
}

// TestMaintainRunsTheIndexStep: jobs maintain prunes the index by the
// month (no index: nothing created), and a second run prunes nothing.
func TestMaintainRunsTheIndexStep(t *testing.T) {
	home := devInstall(t, true)
	configure(t, home, "[backup]\nenabled = false")
	d := pathsOf(home)
	maintain := func() maintainResult {
		t.Helper()
		code, out, _ := run(t, "jobs", "maintain", "--json")
		var env maintainEnvelope
		if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
			t.Fatalf("exit %d: %v\n%s", code, err, out)
		}
		return env.Data
	}
	if r := maintain(); r.MetricsIndex != "pruned 0 row(s) and 0 ingest error(s)" {
		t.Errorf("no index: %q", r.MetricsIndex)
	}
	if _, err := os.Stat(d.IndexDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("maintain created the index: %v", err)
	}

	pointAt(t, home, fakeHindsight(t))
	configure(t, home, "[backup]\nenabled = false")
	writeIndexLines(t, d, "2025-10", 2, time.Date(2025, 10, 31, 23, 0, 0, 0, time.UTC)) // before the cutoff
	writeIndexLines(t, d, "2025-11", 1, time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC))
	tickJSON(t)
	withClock(t, time.Date(2027, 11, 3, 4, 0, 0, 0, time.UTC)) // the cutoff is 2025-11-01
	r := maintain()
	if r.MetricsIndex != "pruned 2 row(s) and 0 ingest error(s)" {
		t.Errorf("metrics_index %q", r.MetricsIndex)
	}
	if n := indexCount(t, home, `SELECT count(*) FROM tel_prompt WHERE session = ?`, indexCanary); n != 1 {
		t.Errorf("%d row(s) kept, want 1", n)
	}
	if r := maintain(); r.MetricsIndex != "pruned 0 row(s) and 0 ingest error(s)" {
		t.Errorf("second run: %q", r.MetricsIndex)
	}
}

// TestRotatedTelemetryMonthKeepsItsIndexRows: the month file maintain's
// rotation removes keeps its rows in the index, its cursor is marked
// missing at the next tick, and the current month goes on ingesting.
func TestRotatedTelemetryMonthKeepsItsIndexRows(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	configure(t, home, "[backup]\nenabled = false")
	d := pathsOf(home)
	cur := time.Now().UTC()
	old := time.Date(cur.Year(), cur.Month(), 1, 12, 0, 0, 0, time.UTC).AddDate(0, -14, 0) // past the 13 months kept
	oldFile := "events-" + monthOf(old) + ".jsonl"
	writeIndexLines(t, d, monthOf(old), 2, old)
	writeIndexLines(t, d, monthOf(cur), 1, cur)
	tickJSON(t)
	count := func(file string) int {
		t.Helper()
		return indexCount(t, home, `SELECT count(*) FROM tel_prompt WHERE session = ? AND source_file = ?`, indexCanary, "telemetry/"+file)
	}
	if n := count(oldFile); n != 2 {
		t.Fatalf("setup: %d row(s) of the old month", n)
	}

	code, out, _ := run(t, "jobs", "maintain", "--json")
	var env maintainEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d: %v\n%s", code, err, out)
	}
	if got := env.Data.Rotated; len(got) != 1 || got[0] != oldFile {
		t.Fatalf("rotated %v, want [%s]", got, oldFile)
	}
	if _, err := os.Stat(filepath.Join(d.TelemetryDir(), oldFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the rotated file is still there: %v", err)
	}

	writeIndexLines(t, d, monthOf(cur), 1, cur)
	tickJSON(t)
	if n := count(oldFile); n != 2 {
		t.Errorf("%d row(s) of the rotated month kept, want 2", n)
	}
	if n := indexCount(t, home, `SELECT count(*) FROM cursors WHERE source_file = ? AND missing = 1`, "telemetry/"+oldFile); n != 1 {
		t.Errorf("the rotated month's cursor is not marked missing (%d)", n)
	}
	if n := count("events-" + monthOf(cur) + ".jsonl"); n != 2 {
		t.Errorf("%d row(s) of the current month, want 2", n)
	}
}

func TestMaintainIndexText(t *testing.T) {
	for _, c := range []struct {
		r    index.MaintainResult
		err  error
		want string
	}{
		{index.MaintainResult{RowsPruned: 3, ErrorsPruned: 1}, nil, "pruned 3 row(s) and 1 ingest error(s)"},
		{index.MaintainResult{SideRemoved: []string{"metrics-v1.db"}, TempRemoved: []string{"metrics.db.tmp-1-ab"}, SideKept: []string{"metrics-v2.db"}}, nil,
			"pruned 0 row(s) and 0 ingest error(s), removed metrics-v1.db, metrics.db.tmp-1-ab, kept metrics-v2.db (in use)"},
		{index.MaintainResult{TempSkipped: true}, nil, "pruned 0 row(s) and 0 ingest error(s), rebuild temp files left: a rebuild is running"},
		{index.MaintainResult{SideRemoved: []string{"metrics-v1.db"}}, index.ErrUpdateInProgress, "skipped: an update is being applied, removed metrics-v1.db"},
		{index.MaintainResult{}, index.ErrDiskFull, "failed: full"},
		// a failed pruning still names the files the side-file step handled
		{index.MaintainResult{SideRemoved: []string{"metrics-v1.db"}, SideKept: []string{"metrics-v2.db"}, TempSkipped: true}, index.ErrUnreadable,
			"failed: corrupt, removed metrics-v1.db, kept metrics-v2.db (in use), rebuild temp files left: a rebuild is running"},
		{index.MaintainResult{}, &fs.PathError{Op: "open", Path: "/home/someone/secret", Err: fs.ErrPermission}, "failed: io"},
		{index.MaintainResult{}, context.DeadlineExceeded, "failed: timeout"},
		{index.MaintainResult{}, fmt.Errorf("prune: %w", index.ErrSchemaChanged), "skipped: backsight was updated meanwhile"},
	} {
		if got := maintainIndexText(c.r, c.err); got != c.want {
			t.Errorf("%+v %v: %q, want %q", c.r, c.err, got, c.want)
		}
	}
}
