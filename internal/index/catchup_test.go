package index

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/paths"
)

// envWith is envAt with Options changed by mod before the open.
func envWith(t *testing.T, mod func(*Options)) *ingestEnv {
	t.Helper()
	d := tempDirs(t)
	c := &clockT{t: clock}
	o := opts(t, d, nil)
	o.Now = c.Now
	mod(&o)
	ix := mustOpen(t, o)
	t.Cleanup(func() { _ = ix.Close() })
	return &ingestEnv{t: t, d: d, ix: ix, clock: c}
}

func (e *ingestEnv) catchUp(budget time.Duration) Freshness {
	e.t.Helper()
	fr, err := e.ix.CatchUp(context.Background(), budget)
	if err != nil {
		e.t.Fatal(err)
	}
	return fr
}

// setAfterBatch replaces the afterBatch hook for the test.
func setAfterBatch(t *testing.T, fn func()) {
	t.Helper()
	prev := afterBatch
	afterBatch = fn
	t.Cleanup(func() { afterBatch = prev })
}

// fakeReader is a LedgerReader the test steers.
type fakeReader struct {
	mu  sync.Mutex
	l   feedback.Ledger
	err error
}

func (f *fakeReader) All() (feedback.Ledger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.l, f.err
}

func (f *fakeReader) set(l feedback.Ledger, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.l, f.err = l, err
}

func ledgerOf(m map[string]feedback.Entry) feedback.Ledger {
	return feedback.Ledger{Schema: feedback.LedgerSchema, Memories: m}
}

func (e *ingestEnv) errorsOf(reason string) []map[string]any {
	e.t.Helper()
	return e.rowsOf(`SELECT source_file, "offset", reason FROM ingest_errors WHERE reason = ?`, reason)
}

func checkRow(t *testing.T, what string, got, want map[string]any) {
	t.Helper()
	for k, w := range want {
		if !sameValue(got[k], w) {
			t.Errorf("%s.%s = %T %v, want %T %v", what, k, got[k], got[k], w, w)
		}
	}
}

func TestLedgerTableMirrorsTheReader(t *testing.T) {
	ms := func(m int) time.Time { return clock.Add(time.Duration(m) * time.Minute) }
	m1 := feedback.Entry{Shown: 3, Credits: 1.5, CreditsAt: ms(-50), Misses: 0.5, MissesAt: ms(-40), LastShown: ms(-30),
		LastCredited: ms(-20), Wrong: 1, Hidden: &feedback.Hidden{Since: ms(-10), Verdict: "wrong", Note: "note-canary"},
		UnhiddenAt: ms(-60)}
	m2 := feedback.Entry{Shown: 2, Credits: 1, Imported: feedback.Imported{Shown: 2, Credits: 1}}
	r := &fakeReader{l: ledgerOf(map[string]feedback.Entry{"m1": m1, "m2": m2})}
	e := envWith(t, func(o *Options) { o.Ledger = r })

	e.catchUp(time.Minute)
	rows := e.rowsOf(`SELECT * FROM ledger ORDER BY memory_id`)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	checkRow(t, "m1", rows[0], map[string]any{
		"memory_id": "m1", "shown": 3, "credits": 1.5, "credits_at": ms(-50).UnixMilli(), "misses": 0.5,
		"misses_at": ms(-40).UnixMilli(), "last_shown": ms(-30).UnixMilli(), "last_credited": ms(-20).UnixMilli(),
		"wrong": 1, "hidden_since": ms(-10).UnixMilli(), "unhidden_at": ms(-60).UnixMilli(),
		"imported_shown": 0, "imported_credits": 0.0,
	})
	checkRow(t, "m2", rows[1], map[string]any{
		"memory_id": "m2", "shown": 2, "credits": 1.0, "credits_at": nil, "misses": 0.0, "misses_at": nil,
		"last_shown": nil, "last_credited": nil, "wrong": 0, "hidden_since": nil, "unhidden_at": nil,
		"imported_shown": 2, "imported_credits": 1.0,
	})
	if hits := textAnywhere(t, e.ix, "note-canary"); len(hits) > 0 {
		t.Errorf("the hiding's note reached the index: %v", hits)
	}

	r.set(ledgerOf(map[string]feedback.Entry{"m1": m1}), nil)
	e.catchUp(time.Minute)
	if rows := e.rowsOf(`SELECT memory_id FROM ledger`); len(rows) != 1 || rows[0]["memory_id"] != "m1" {
		t.Errorf("after m2 left the ledger: %v", rows)
	}

	r.set(feedback.Ledger{}, errors.New("unreadable"))
	e.catchUp(time.Minute)
	e.catchUp(time.Minute) // a ledger that stays unreadable is recorded once
	if rows := e.rowsOf(`SELECT memory_id, shown FROM ledger`); len(rows) != 1 || rows[0]["memory_id"] != "m1" || rows[0]["shown"] != int64(3) {
		t.Errorf("a reader error changed the table: %v", rows)
	}
	errs := e.errorsOf("ledger_unreadable")
	if len(errs) != 1 {
		t.Fatalf("ledger_unreadable rows: %v", errs)
	}
	checkRow(t, "error", errs[0], map[string]any{"source_file": "feedback/ledger.json", "offset": 0})

	r.set(ledgerOf(map[string]feedback.Entry{"m2": m2}), nil)
	e.catchUp(time.Minute)
	if rows := e.rowsOf(`SELECT memory_id FROM ledger`); len(rows) != 1 || rows[0]["memory_id"] != "m2" {
		t.Errorf("after the reader recovered: %v", rows)
	}
	if errs := e.errorsOf("ledger_unreadable"); len(errs) != 0 {
		t.Errorf("the error stays after a good read: %v", errs)
	}
}

// An id that is not a name (control characters, too long) is no row.
func TestLedgerTableSkipsABadID(t *testing.T) {
	r := &fakeReader{l: ledgerOf(map[string]feedback.Entry{"ok": {Shown: 1}, "bad\nid": {Shown: 1}, strings.Repeat("x", nameMax+1): {Shown: 1}})}
	e := envWith(t, func(o *Options) { o.Ledger = r })
	e.catchUp(time.Minute)
	if rows := e.rowsOf(`SELECT memory_id FROM ledger`); len(rows) != 1 || rows[0]["memory_id"] != "ok" {
		t.Errorf("rows %v", rows)
	}
}

// Without a reader of its own, the index reads the feedback directory's
// ledger.
func TestLedgerTableReadsTheFeedbackDirectory(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.d.FeedbackDir(), "ledger.json"), `{"schema":1,"memories":{"m9":{"shown":4}}}`)
	e.catchUp(time.Minute)
	if rows := e.rowsOf(`SELECT memory_id, shown FROM ledger`); len(rows) != 1 || rows[0]["memory_id"] != "m9" || rows[0]["shown"] != int64(4) {
		t.Errorf("rows %v", rows)
	}
}

func TestLocalStatusRefresh(t *testing.T) {
	var mu sync.Mutex
	at := func(m int) time.Time { return clock.Add(time.Duration(m) * time.Minute) }
	ls := LocalStatus{NotesQueued: 2, Quarantined: 1, SessionsWaiting: 4, BreakerFailures: 3, UpdateNewer: 2, BackfillSessions: 5,
		NotesOldestAt: at(-1), QuarantineOldestAt: at(-2), ReachLastOK: at(-3), ReachFailingSince: at(-4), BreakerOpenUntil: at(5),
		BackupCreatedAt: at(-6), BackupVerifiedAt: at(-7), UpdateCheckedAt: at(-8), BackupOK: true, UpdateCheckFailed: true, BackfillPaused: true}
	var lsErr error
	e := envWith(t, func(o *Options) {
		o.LocalStatus = func() (LocalStatus, error) {
			mu.Lock()
			defer mu.Unlock()
			return ls, lsErr
		}
	})
	e.catchUp(time.Minute)
	want := map[string]any{
		"id": 1, "ingested_at": clock.UnixMilli(), "notes_queued": 2, "notes_oldest_at": at(-1).UnixMilli(), "quarantined": 1,
		"quarantine_oldest_at": at(-2).UnixMilli(), "sessions_waiting": 4, "reach_last_ok": at(-3).UnixMilli(),
		"reach_failing_since": at(-4).UnixMilli(), "breaker_failures": 3, "breaker_open_until": at(5).UnixMilli(),
		"backup_created_at": at(-6).UnixMilli(), "backup_verified_at": at(-7).UnixMilli(), "backup_ok": 1,
		"update_checked_at": at(-8).UnixMilli(), "update_newer": 2, "update_check_failed": 1, "backfill_sessions": 5,
		"backfill_paused": 1,
	}
	rows := e.rowsOf(`SELECT * FROM local_status`)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	checkRow(t, "local_status", rows[0], want)

	// Zero times are NULL, false is 0; the row is replaced, not added.
	mu.Lock()
	ls = LocalStatus{NotesQueued: 1}
	mu.Unlock()
	e.clock.Add(time.Minute)
	e.catchUp(time.Minute)
	rows = e.rowsOf(`SELECT * FROM local_status`)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	checkRow(t, "local_status", rows[0], map[string]any{"ingested_at": clock.Add(time.Minute).UnixMilli(), "notes_queued": 1,
		"notes_oldest_at": nil, "backup_ok": 0, "breaker_open_until": nil, "backfill_paused": 0})

	// An error keeps the row and is recorded once against state.json.
	mu.Lock()
	lsErr = errors.New("unreadable")
	mu.Unlock()
	e.clock.Add(time.Minute)
	e.catchUp(time.Minute)
	e.catchUp(time.Minute)
	rows = e.rowsOf(`SELECT * FROM local_status`)
	if len(rows) != 1 || !sameValue(rows[0]["ingested_at"], clock.Add(time.Minute).UnixMilli()) {
		t.Errorf("an error changed the row: %v", rows)
	}
	errs := e.errorsOf("local_status_unreadable")
	if len(errs) != 1 {
		t.Fatalf("local_status_unreadable rows: %v", errs)
	}
	checkRow(t, "error", errs[0], map[string]any{"source_file": "state.json", "offset": 0})

	mu.Lock()
	lsErr = nil
	mu.Unlock()
	e.catchUp(time.Minute)
	if errs := e.errorsOf("local_status_unreadable"); len(errs) != 0 {
		t.Errorf("the error stays after a good read: %v", errs)
	}
}

func TestLocalStatusNotRefreshedWithoutASource(t *testing.T) {
	e := newEnv(t)
	e.catchUp(time.Minute)
	if n := e.count("local_status"); n != 0 {
		t.Errorf("%d rows", n)
	}
}

func TestCatchUpIsBounded(t *testing.T) {
	e := newEnv(t)
	var b strings.Builder
	for i := 1; i <= 12_000; i++ {
		b.WriteString(promptLine(i, 1))
	}
	e.write("2026-10", b.String())
	setAfterBatch(t, func() { e.clock.Add(time.Hour) })
	fr := e.catchUp(time.Minute)
	if !fr.Partial {
		t.Error("not partial")
	}
	if n := e.count("tel_prompt"); n != 5000 {
		t.Errorf("%d rows, want 5000", n)
	}
	if fr.LagSeconds != 3600 { // the clock moved an hour past the lines
		t.Errorf("lag %d, want 3600", fr.LagSeconds)
	}
	if fr.Lines != 5000 {
		t.Errorf("lines %d, want 5000", fr.Lines)
	}
	afterBatch = func() {}
	fr = e.catchUp(time.Minute)
	if fr.Partial || fr.LagSeconds != 0 || fr.Lines != 7000 {
		t.Errorf("second run %+v", fr)
	}
	if raw, _ := json.Marshal(fr); strings.Contains(string(raw), "7000") {
		t.Errorf("the line count is in the views' JSON: %s", raw)
	}
	if n := e.count("tel_prompt"); n != 12_000 {
		t.Errorf("%d rows, want 12000", n)
	}
	if !fr.IngestedAt.Equal(clock.Add(time.Hour)) {
		t.Errorf("ingested_at %v", fr.IngestedAt)
	}
}

// lagLine is a prompt line stamped at.
func lagLine(at time.Time) string {
	return tline("prompt", at.UTC().Format(time.RFC3339Nano), map[string]any{"decision": "skip"})
}

func TestLag(t *testing.T) {
	e := newEnv(t)
	e.write("2026-10", lagLine(clock.Add(-2*time.Hour))+lagLine(clock.Add(-90*time.Second)))
	fr := e.catchUp(time.Minute)
	if fr.LagSeconds != 0 || fr.Partial || !fr.IngestedAt.Equal(clock) {
		t.Errorf("everything read: %+v", fr)
	}
	e.append("2026-10", lagLine(clock)+lagLine(clock)+lagLine(clock))
	fr = e.catchUp(0) // no time for lines
	if fr.LagSeconds != 90 || !fr.Partial {
		t.Errorf("3 lines left: %+v, want lag 90 and partial", fr)
	}
	if n := e.count("tel_prompt"); n != 2 {
		t.Errorf("%d rows: a zero budget ingested lines", n)
	}
}

// Lines appended after the ingest's last batch are behind, but the
// budget did not run out: lag, not partial.
func TestLinesAfterTheIngestAreNotPartial(t *testing.T) {
	e := newEnv(t)
	e.write("2026-10", lagLine(clock.Add(-90*time.Second)))
	setAfterBatch(t, func() { e.append("2026-10", lagLine(clock)) })
	if fr := e.catchUp(time.Minute); fr.Partial || fr.LagSeconds != 90 {
		t.Errorf("%+v, want lag 90, not partial", fr)
	}
}

// Behind with nothing timed ingested (every line so far an error), the
// lag cannot be measured: partial, never fresh.
func TestBehindWithoutATimeIsPartial(t *testing.T) {
	e := newEnv(t)
	e.write("2026-10", "not json\n")
	setAfterBatch(t, func() { e.append("2026-10", lagLine(clock)) })
	if fr := e.catchUp(time.Minute); !fr.Partial {
		t.Errorf("%+v, want partial", fr)
	}
	afterBatch = func() {}
	if fr := e.catchUp(time.Minute); fr.Partial || fr.LagSeconds != 0 {
		t.Errorf("caught up: %+v", fr)
	}
}

// A last line without its newline is not a line left: no lag, not partial.
func TestLagIgnoresAPartialLastLine(t *testing.T) {
	e := newEnv(t)
	e.write("2026-10", lagLine(clock.Add(-90*time.Second)))
	e.catchUp(time.Minute)
	e.append("2026-10", strings.TrimSuffix(lagLine(clock), "\n"))
	if fr := e.catchUp(0); fr.LagSeconds != 0 || fr.Partial {
		t.Errorf("%+v", fr)
	}
}

// A file never read is behind too.
func TestLagCountsANewFile(t *testing.T) {
	e := newEnv(t)
	e.write("2026-09", lagLine(clock.Add(-90*time.Second)))
	e.catchUp(time.Minute)
	e.write("2026-10", lagLine(clock))
	if fr := e.catchUp(0); fr.LagSeconds != 90 || !fr.Partial {
		t.Errorf("%+v", fr)
	}
}

func TestLagNeverNegative(t *testing.T) {
	e := newEnv(t)
	e.write("2026-10", lagLine(clock.Add(time.Hour)))
	e.catchUp(time.Minute)
	e.append("2026-10", lagLine(clock))
	if fr := e.catchUp(0); fr.LagSeconds != 0 || !fr.Partial {
		t.Errorf("%+v, want lag 0 and partial", fr)
	}
}

func TestCatchUpIsSingleFlight(t *testing.T) {
	e := newEnv(t)
	var b strings.Builder
	for i := 1; i <= 20_000; i++ {
		b.WriteString(promptLine(i, i%7))
	}
	e.write("2026-10", b.String())
	var mu sync.Mutex
	active, most, batches := 0, 0, 0
	setAfterBatch(t, func() {
		mu.Lock()
		active++
		batches++
		most = max(most, active)
		first := batches == 1
		mu.Unlock()
		// After the first batch, give a second pass up to a second to
		// commit its own batch.
		for end := time.Now().Add(time.Second); first && time.Now().Before(end); time.Sleep(time.Millisecond) {
			mu.Lock()
			m := most
			mu.Unlock()
			if m > 1 {
				break
			}
		}
		mu.Lock()
		active--
		mu.Unlock()
	})
	var wg sync.WaitGroup
	start := make(chan struct{})
	frs := make([]Freshness, 8)
	errs := make([]error, 8)
	for i := range frs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			frs[i], errs[i] = e.ix.CatchUp(context.Background(), time.Minute)
		}()
	}
	close(start)
	wg.Wait()
	if most > 1 {
		t.Errorf("%d ingest passes at once", most)
	}
	if batches != 4 {
		t.Errorf("%d batches, want 4 (each line read once)", batches)
	}
	for i := range frs {
		if errs[i] != nil || frs[i].Partial || !frs[i].IngestedAt.Equal(clock) {
			t.Errorf("caller %d: %+v, %v", i, frs[i], errs[i])
		}
	}
	afterBatch = func() {}
	if got, want := dump(t, e.ix), freshDump(t, e); got != want {
		t.Error("the index differs from a fresh build")
	}
}

// A caller whose context ends while it waits for another's run returns
// at once with its context's error.
func TestCatchUpWaiterHonoursItsContext(t *testing.T) {
	e := newEnv(t)
	e.write("2026-10", promptLine(1, 1))
	entered, release := make(chan struct{}), make(chan struct{})
	setAfterBatch(t, func() { close(entered); <-release })
	leader := make(chan error, 1)
	go func() {
		_, err := e.ix.CatchUp(context.Background(), time.Minute)
		leader <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waiter := make(chan error, 1)
	go func() {
		_, err := e.ix.CatchUp(ctx, time.Minute)
		waiter <- err
	}()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the waiter ignored its context")
	}
	close(release)
	if err := <-leader; err != nil {
		t.Errorf("the run waited on: %v", err)
	}
}

// A run that stopped on its own caller's context is not handed to a
// waiter whose context is alive: the waiter runs again and gets a result.
func TestCatchUpWaiterRunsAgainAfterTheLeadersContext(t *testing.T) {
	e := newEnv(t)
	var b strings.Builder
	for i := 1; i <= 10_000; i++ { // two batches: the leader stops between them
		b.WriteString(promptLine(i, 1))
	}
	e.write("2026-10", b.String())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	setAfterBatch(t, func() { once.Do(func() { close(entered); <-release }) })
	waiting := make(chan struct{}, 8)
	prev := onWait
	onWait = func() { waiting <- struct{}{} }
	t.Cleanup(func() { onWait = prev })

	lctx, cancel := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() {
		_, err := e.ix.CatchUp(lctx, time.Minute)
		leader <- err
	}()
	<-entered
	type result struct {
		fr  Freshness
		err error
	}
	waiter := make(chan result, 1)
	go func() {
		fr, err := e.ix.CatchUp(context.Background(), time.Minute)
		waiter <- result{fr, err}
	}()
	<-waiting // the waiter waits for the leader's run
	cancel()
	close(release)
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Errorf("leader: %v, want its context's error", err)
	}
	res := <-waiter
	if res.err != nil || res.fr.Partial || res.fr.LagSeconds != 0 {
		t.Errorf("waiter: %+v, %v; want a complete catch-up", res.fr, res.err)
	}
	if n := e.count("tel_prompt"); n != 10_000 {
		t.Errorf("%d rows, want 10000", n)
	}
}

// TestHelperIngest is the child of TestTwoProcessesIngestConcurrently: it
// catches up the index of the state directory it is given, again while a
// run comes back partial (it yielded the write lock to the other child,
// which may hold it past the busy timeout on a slow machine). Never an
// error.
func TestHelperIngest(t *testing.T) {
	dir := os.Getenv("BACKSIGHT_INDEX_HELPER")
	if dir == "" {
		t.Skip("the child of TestTwoProcessesIngestConcurrently")
	}
	ix, err := Open(context.Background(), Options{Dirs: paths.Dirs{State: dir}, Home: t.TempDir(), Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()
	// Each partial run means the other child committed meanwhile: the
	// bound is far above what the 20,000 lines can take.
	for range 100 {
		fr, err := ix.CatchUp(context.Background(), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !fr.Partial {
			return
		}
	}
	t.Fatal("still partial after 100 runs")
}

func TestTwoProcessesIngestConcurrently(t *testing.T) {
	d := tempDirs(t)
	for _, month := range []string{"2026-09", "2026-10"} {
		var b strings.Builder
		for i := 1; i <= 10_000; i++ {
			b.WriteString(promptLine(i, i%11))
		}
		writeFile(t, filepath.Join(d.TelemetryDir(), "events-"+month+".jsonl"), b.String())
	}
	var cmds []*exec.Cmd
	var outs []*bytes.Buffer
	for range 2 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperIngest$", "-test.count=1")
		cmd.Env = append(os.Environ(), "BACKSIGHT_INDEX_HELPER="+d.State)
		out := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = out, out
		cmds, outs = append(cmds, cmd), append(outs, out)
	}
	for _, c := range cmds {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Errorf("child %d: %v\n%s", i, err, outs[i])
		}
	}
	if t.Failed() {
		return
	}
	e := envAt(t, d)
	if n := e.count("tel_prompt"); n != 20_000 {
		t.Errorf("%d rows, want 20000", n)
	}
	if fr := e.catchUp(time.Minute); fr.Partial || fr.Lines != 0 {
		t.Errorf("the children left work: %+v", fr)
	}
	if got, want := dump(t, e.ix), freshDump(t, e); got != want {
		t.Error("two processes built an index other than a fresh build")
	}
}

// shortBusyWait cuts the busy timeout of the connections opened from here
// on (the DSN's busy_timeout, openWriter's retry) to 200 ms.
func shortBusyWait(t *testing.T) {
	t.Helper()
	prev := busyWait
	busyWait = 200 * time.Millisecond
	t.Cleanup(func() { busyWait = prev })
}

// holdWriteLock takes the write lock of the index at path from a
// connection of its own (BEGIN IMMEDIATE), as another backsight process
// in the middle of a batch does; release lets it go.
func holdWriteLock(t *testing.T, path string, fs FSInfo) (release func()) {
	t.Helper()
	db, err := sql.Open(driverName, dsn(path, fs, false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(); _ = db.Close() }) }
	t.Cleanup(release)
	return release
}

// Opening an index another process is writing (a tick in the middle of a
// batch) does not wait for its write lock: nothing to migrate, nothing to
// write.
func TestOpenDoesNotWaitForAnotherWriter(t *testing.T) {
	shortBusyWait(t)
	o := opts(t, tempDirs(t), nil)
	ix := mustOpen(t, o)
	path, fs := ix.Path(), ix.FS()
	mustClose(t, ix)
	holdWriteLock(t, path, fs)
	ix, err := Open(context.Background(), o)
	if err != nil {
		t.Fatalf("open while another process writes: %v", err)
	}
	mustClose(t, ix)
}

// A catch-up whose write transaction finds the lock held past the busy
// timeout leaves the work to the process that holds it: no error, Partial,
// nothing written (the ledger and the local status are not tried either);
// once the lock is free the next catch-up does it all.
func TestCatchUpYieldsToAnotherWriter(t *testing.T) {
	shortBusyWait(t)
	r := &fakeReader{l: ledgerOf(map[string]feedback.Entry{"m1": {Shown: 1}})}
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString(promptLine(i, 1))
	}
	e := envWith(t, func(o *Options) { o.Ledger = r })
	e.write("2026-10", b.String())
	release := holdWriteLock(t, e.ix.Path(), e.ix.FS())
	writes := 0
	prev := onWriteTx
	onWriteTx = func() { writes++ }
	t.Cleanup(func() { onWriteTx = prev })
	fr, err := e.ix.CatchUp(context.Background(), time.Minute)
	if err != nil || !fr.Partial || fr.Lines != 0 {
		t.Fatalf("%+v, %v; want a partial catch-up and no error", fr, err)
	}
	if writes != 1 {
		t.Errorf("%d write transactions tried, want 1: the rest is left to the other process", writes)
	}
	release()
	if fr := e.catchUp(time.Minute); fr.Partial || fr.Lines != 10 {
		t.Errorf("after the lock was freed: %+v", fr)
	}
	if n, m := e.count("tel_prompt"), e.count("ledger"); n != 10 || m != 1 {
		t.Errorf("%d prompt rows and %d ledger rows, want 10 and 1", n, m)
	}
}

// The files caught up, a ledger write that finds the lock held is left to
// the other process too: Partial, no error.
func TestCatchUpYieldsOnTheLedgerWrite(t *testing.T) {
	shortBusyWait(t)
	r := &fakeReader{l: ledgerOf(map[string]feedback.Entry{"m1": {Shown: 1}})}
	e := envWith(t, func(o *Options) { o.Ledger = r })
	e.write("2026-10", promptLine(1, 1))
	e.catchUp(time.Minute)
	r.set(ledgerOf(map[string]feedback.Entry{"m1": {Shown: 1}, "m2": {Shown: 2}}), nil)
	release := holdWriteLock(t, e.ix.Path(), e.ix.FS())
	fr, err := e.ix.CatchUp(context.Background(), time.Minute)
	if err != nil || !fr.Partial {
		t.Fatalf("%+v, %v; want a partial catch-up and no error", fr, err)
	}
	release()
	if fr := e.catchUp(time.Minute); fr.Partial || e.count("ledger") != 2 {
		t.Errorf("after the lock was freed: %+v, %d ledger rows", fr, e.count("ledger"))
	}
}

// The other writers do not yield: a rebuild's build (its own file: a lock
// held there is no catch-up of another process), a migration and the
// pruning return the lock's error.
func TestOnlyTheCatchUpYieldsTheLock(t *testing.T) {
	shortBusyWait(t)
	t.Run("rebuild build", func(t *testing.T) {
		e := newEnv(t)
		e.write("2026-10", promptLine(1, 1))
		holdWriteLock(t, e.ix.Path(), e.ix.FS())
		if _, _, err := e.ix.refresh(context.Background(), clock, time.Time{}, false); Class(err) != "busy" {
			t.Errorf("err %v, want the class busy", err)
		}
	})
	t.Run("migration", func(t *testing.T) {
		d := tempDirs(t)
		ix := mustOpen(t, opts(t, d, regA()))
		path, fs := ix.Path(), ix.FS()
		mustClose(t, ix)
		holdWriteLock(t, path, fs)
		if _, err := Open(context.Background(), opts(t, d, regB())); Class(err) != "busy" {
			t.Errorf("err %v, want the class busy", err)
		}
	})
	t.Run("prune", func(t *testing.T) {
		e := newEnv(t)
		e.addError("telemetry/events-2026-08.jsonl", 0, "bad_json", clock.AddDate(0, 0, -60))
		holdWriteLock(t, e.ix.Path(), e.ix.FS())
		o := opts(t, e.d, nil)
		if _, err := Maintain(context.Background(), o); Class(err) != "busy" {
			t.Errorf("err %v, want the class busy", err)
		}
	})
}

var progressRE = regexp.MustCompile(`^metrics index: \d+\.\d/\d+\.\d MB$`)

// twoMiB writes 10,486 lines of 200 bytes (2,097,200 bytes, 3 batches).
func (e *ingestEnv) twoMiB() {
	e.t.Helper()
	var b strings.Builder
	for i := 1; i <= 10_486; i++ {
		b.WriteString(fixedLine(i, 200))
	}
	e.write("2026-10", b.String())
}

func TestCatchUpReportsProgress(t *testing.T) {
	var buf bytes.Buffer
	e := envWith(t, func(o *Options) { o.Progress = &buf })
	e.twoMiB()
	setAfterBatch(t, func() { e.clock.Add(600 * time.Millisecond) })
	e.catchUp(time.Minute)
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	// Batches end at 0.6, 1.2 and 1.8 s: a line at the first, none 0.6 s
	// later, one at the third.
	if len(lines) != 3 || !progressRE.MatchString(lines[0]) || lines[1] != "metrics index: 2.0/2.0 MB" ||
		lines[2] != "metrics index: ready (10486 lines in 1.8 s)" {
		t.Errorf("progress:\n%s", buf.String())
	}
	if lines[0] != "metrics index: 1.0/2.0 MB" {
		t.Errorf("first line %q", lines[0])
	}
}

func TestCatchUpProgressIsQuietBelowAMebibyte(t *testing.T) {
	var buf bytes.Buffer
	e := envWith(t, func(o *Options) { o.Progress = &buf })
	var b strings.Builder
	for i := 1; i <= 5000; i++ { // 1,000,000 bytes: under 1 MiB
		b.WriteString(fixedLine(i, 200))
	}
	e.write("2026-10", b.String())
	e.catchUp(time.Minute)
	if buf.Len() != 0 {
		t.Errorf("progress below 1 MiB: %q", buf.String())
	}
}

// Without a Progress writer nothing reaches stdout or stderr.
func TestCatchUpWithoutProgressIsSilent(t *testing.T) {
	e := newEnv(t)
	e.twoMiB()
	dir := t.TempDir()
	outF, errF := filepath.Join(dir, "out"), filepath.Join(dir, "err")
	stdout, stderr := os.Stdout, os.Stderr
	for _, p := range []struct {
		path string
		to   **os.File
	}{{outF, &os.Stdout}, {errF, &os.Stderr}} {
		f, err := os.Create(p.path)
		if err != nil {
			t.Fatal(err)
		}
		*p.to = f
	}
	_, err := e.ix.CatchUp(context.Background(), time.Minute)
	_ = os.Stdout.Close()
	_ = os.Stderr.Close()
	os.Stdout, os.Stderr = stdout, stderr
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outF, errF} {
		if b, _ := os.ReadFile(p); len(b) != 0 {
			t.Errorf("%s: %q", filepath.Base(p), b)
		}
	}
}

// A partial catch-up says where it stopped, never ready.
func TestCatchUpProgressWhenPartial(t *testing.T) {
	var buf bytes.Buffer
	e := envWith(t, func(o *Options) { o.Progress = &buf })
	e.twoMiB()
	setAfterBatch(t, func() { e.clock.Add(time.Hour) })
	if fr := e.catchUp(time.Minute); !fr.Partial {
		t.Fatal("not partial")
	}
	if strings.Contains(buf.String(), "ready") || !strings.Contains(buf.String(), "metrics index: 1.0/2.0 MB") {
		t.Errorf("progress:\n%s", buf.String())
	}
}

func TestCheckpointTruncatesTheWAL(t *testing.T) {
	e := newEnv(t)
	e.write("2026-10", promptLine(1, 1))
	e.catchUp(time.Minute)
	wal := e.ix.Path() + "-wal"
	if st, err := os.Stat(wal); err != nil || st.Size() == 0 {
		t.Fatalf("no wal to checkpoint: %v", err)
	}
	if err := e.ix.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(wal); err != nil || st.Size() != 0 {
		t.Errorf("wal after the checkpoint: %v, %v", st.Size(), err)
	}
	if n := e.count("tel_prompt"); n != 1 {
		t.Errorf("%d rows", n)
	}
}

// In DELETE mode Checkpoint changes nothing: no error, the journal mode
// stays delete, no -wal appears, the file's bytes stay as they were.
// (wal_checkpoint is itself a no-op there: this guards against a
// checkpoint that fails or switches the journal mode, not the early
// return.)
func TestCheckpointInDeleteModeDoesNothing(t *testing.T) {
	old := detectFS
	detectFS = func(string, string, func(string) string) FSInfo { return FSInfo{FSNetwork, "nfs"} }
	t.Cleanup(func() { detectFS = old })
	e := newEnv(t)
	e.write("2026-10", promptLine(1, 1))
	e.catchUp(time.Minute)
	before, err := os.ReadFile(e.ix.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ix.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := pragma(t, e.ix.sh.w, "journal_mode"); got != "delete" {
		t.Errorf("journal_mode %s", got)
	}
	if _, err := os.Stat(e.ix.Path() + "-wal"); err == nil {
		t.Error("a wal file in DELETE mode")
	}
	if after, err := os.ReadFile(e.ix.Path()); err != nil || !bytes.Equal(before, after) {
		t.Errorf("the file changed (%v)", err)
	}
}

// bumpVersion sets the file's user_version from a connection of its own,
// as a newer backsight's migration would.
func bumpVersion(t *testing.T, path string, v int) {
	t.Helper()
	raw, err := sql.Open(driverName, "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		t.Fatal(err)
	}
}

// TestCatchUpStopsOnASchemaChange: a long-lived handle whose file was
// migrated by another binary after its Open writes nothing more: every
// write transaction reads user_version first, and CatchUp returns
// ErrSchemaChanged (the process must open the index again).
func TestCatchUpStopsOnASchemaChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(e *ingestEnv, r *fakeReader)
	}{
		{"new lines", func(e *ingestEnv, _ *fakeReader) { e.append("2026-10", promptLine(2, 1)) }},
		{"a new file", func(e *ingestEnv, _ *fakeReader) { e.write("2026-11", promptLine(3, 1)) }},
		{"the ledger", func(_ *ingestEnv, r *fakeReader) {
			r.set(ledgerOf(map[string]feedback.Entry{"m-canary-1": {Shown: 1}}), nil)
		}},
		{"the ledger unreadable", func(_ *ingestEnv, r *fakeReader) { r.set(feedback.Ledger{}, errors.New("CANARY-ledger-down")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeReader{}
			r.set(ledgerOf(nil), nil)
			e := envWith(t, func(o *Options) { o.Ledger = r })
			e.write("2026-10", promptLine(1, 1))
			e.catchUp(time.Minute)
			before := dump(t, e.ix)
			ledger := e.count("ledger")
			bumpVersion(t, e.ix.Path(), e.ix.Schema()+1)
			tc.mod(e, r)
			_, err := e.ix.CatchUp(context.Background(), time.Minute)
			if !errors.Is(err, ErrSchemaChanged) {
				t.Fatalf("CatchUp: %v, want ErrSchemaChanged", err)
			}
			if got := dump(t, e.ix); got != before {
				t.Errorf("rows or cursors written after the change\n got %s\nwant %s", got, before)
			}
			if e.count("ledger") != ledger {
				t.Error("the ledger table was written after the change")
			}
		})
	}
}

// countWriteTxs counts the write transactions begun for the test.
func countWriteTxs(t *testing.T) *int {
	t.Helper()
	var mu sync.Mutex
	n := new(int)
	prev := onWriteTx
	onWriteTx = func() { mu.Lock(); *n++; mu.Unlock() }
	t.Cleanup(func() { onWriteTx = prev })
	return n
}

// TestCatchUpWithNothingNewTakesNoWriteLock: a catch-up that finds every
// source as it left it (a file at its cursor with a partial last line, a
// file gone and marked missing, an unreadable file already recorded, the
// same ledger and local status) begins no write transaction; its
// freshness still says it caught up now. A change takes one again.
func TestCatchUpWithNothingNewTakesNoWriteLock(t *testing.T) {
	r := &fakeReader{}
	r.set(ledgerOf(map[string]feedback.Entry{"m-1": {Shown: 2}}), nil)
	ls := LocalStatus{NotesQueued: 3, ReachLastOK: clock.Add(-time.Minute), BackupOK: true}
	e := envWith(t, func(o *Options) {
		o.Ledger = r
		o.LocalStatus = func() (LocalStatus, error) { return ls, nil }
	})
	e.write("2026-10", promptLine(1, 1)+`{"event":"prompt","ts":"`+ts0+`"`) // a partial last line
	e.write("2026-09", promptLine(2, 1))
	unreadable := runtime.GOOS != "windows" && os.Geteuid() != 0
	if unreadable {
		e.write("2026-08", promptLine(3, 1))
		if err := os.Chmod(e.telPath("2026-08"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(e.telPath("2026-08"), 0o600) })
	}
	e.catchUp(time.Minute)
	if err := os.Remove(e.telPath("2026-09")); err != nil {
		t.Fatal(err)
	}
	e.catchUp(time.Minute) // marks 2026-09 missing
	if c := e.cursor("2026-09"); c["missing"] != int64(1) {
		t.Fatalf("cursor %v", c)
	}

	n := countWriteTxs(t)
	e.clock.Add(time.Hour)
	fr := e.catchUp(time.Minute)
	if *n != 0 {
		t.Errorf("%d write transactions for a catch-up with nothing new", *n)
	}
	if !fr.IngestedAt.Equal(clock.Add(time.Hour)) || fr.Partial {
		t.Errorf("freshness %+v, want caught up at %v", fr, clock.Add(time.Hour))
	}

	e.append("2026-10", "\n") // the partial line completed
	e.catchUp(time.Minute)
	if *n == 0 {
		t.Error("no write transaction for a new line")
	}
	*n = 0
	ls.NotesQueued = 4
	e.catchUp(time.Minute)
	if *n != 1 {
		t.Errorf("%d write transactions for a changed local status, want 1", *n)
	}
	if rs := e.rowsOf(`SELECT notes_queued FROM local_status`); len(rs) != 1 || rs[0]["notes_queued"] != int64(4) {
		t.Errorf("local status %v", rs)
	}
}

// The catch-up budgets are the ones the views and the tick are promised.
func TestCatchUpBudgets(t *testing.T) {
	if TickBudget != 5*time.Second || ViewBudget != 2*time.Second {
		t.Errorf("tick %v, view %v; want 5 s and 2 s", TickBudget, ViewBudget)
	}
}

// An index never caught up has no ingested_at in the views' JSON.
func TestFreshnessLeavesOutAZeroTime(t *testing.T) {
	raw, err := json.Marshal(Freshness{LagSeconds: 3, Partial: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != `{"lag_seconds":3,"partial":true}` {
		t.Errorf("%s", got)
	}
}

// A migration another process committed while this one waited for the
// write lock needs no lock: the open that read version 0 before that
// commit finds it applied once the wait times out, instead of failing
// with SQLITE_BUSY (the process holding the lock runs batches back to
// back, and a waiter can starve past the busy timeout).
func TestMigrationCommittedWhileWaitingIsSkipped(t *testing.T) {
	shortBusyWait(t)
	o := opts(t, tempDirs(t), nil)
	ix := mustOpen(t, o)
	reg, err := registry(o.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	holdWriteLock(t, ix.Path(), ix.FS())
	applied, err := applyPending(context.Background(), ix.sh.w, reg, Head(reg), o.withDefaults())
	if err != nil || len(applied) != 0 {
		t.Fatalf("applyPending = %v, %v; want nothing applied, no error", applied, err)
	}

	// A migration nobody committed is still waited for, then refused.
	next := Migration{ID: "9999-pending", Num: Head(reg) + 1, SQL: "SELECT 1"}
	_, err = applyPending(context.Background(), ix.sh.w, []Migration{next}, next.Num, o.withDefaults())
	if err == nil || !isBusy(err) {
		t.Fatalf("applyPending of an uncommitted migration = %v, want SQLITE_BUSY", err)
	}
	mustClose(t, ix)
}
