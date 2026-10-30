package index

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	afterBatch = func() {}
	fr = e.catchUp(time.Minute)
	if fr.Partial || fr.LagSeconds != 0 {
		t.Errorf("second run %+v", fr)
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
// catches up the index of the state directory it is given.
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
	fr, err := ix.CatchUp(context.Background(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Partial {
		t.Fatalf("partial: %+v", fr)
	}
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
	if got, want := dump(t, e.ix), freshDump(t, e); got != want {
		t.Error("two processes built an index other than a fresh build")
	}
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
