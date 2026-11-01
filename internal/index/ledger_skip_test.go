package index

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// stampReader is a LedgerReader with a stamp, both steered by the test,
// counting the reads.
type stampReader struct {
	mu   sync.Mutex
	l    feedback.Ledger
	err  error
	st   feedback.Stamp
	alls int
}

func (r *stampReader) All() (feedback.Ledger, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alls++
	return r.l, r.err
}

func (r *stampReader) Stamp() (feedback.Stamp, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st, nil
}

func (r *stampReader) reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.alls
}

func (r *stampReader) setLedger(l feedback.Ledger, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.l, r.err = l, err
}

func (r *stampReader) setStamp(st feedback.Stamp) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st = st
}

// stampAt is a one-file stamp whose newest mtime is newest.
func stampAt(newest time.Time, size int64) feedback.Stamp {
	return feedback.Stamp{
		Files:  []feedback.StampFile{{Name: "ledger.json", Size: size, ModTime: newest.UnixNano()}},
		Newest: newest,
	}
}

// fsOf makes detectFS answer kind for the feedback directory of every
// index the test opens, local for anything else (the index directory).
func fsOf(t *testing.T, kind FSKind) {
	t.Helper()
	old := detectFS
	t.Cleanup(func() { detectFS = old })
	detectFS = func(dir, _ string, _ func(string) string) FSInfo {
		if filepath.Base(dir) == "feedback" {
			return FSInfo{Kind: kind, Name: string(kind)}
		}
		return FSInfo{Kind: FSLocal}
	}
}

// skipEnv is an index whose ledger comes from a stampReader holding m1,
// its stamp settled an hour before the clock, on a local feedback
// directory.
func skipEnv(t *testing.T) (*ingestEnv, *stampReader) {
	t.Helper()
	fsOf(t, FSLocal)
	r := &stampReader{l: ledgerOf(map[string]feedback.Entry{"m1": {Shown: 3}}), st: stampAt(clock.Add(-time.Hour), 10)}
	e := envWith(t, func(o *Options) { o.Ledger = r })
	return e, r
}

func (e *ingestEnv) ledgerIDs() []string {
	e.t.Helper()
	var ids []string
	for _, row := range e.rowsOf(`SELECT memory_id FROM ledger ORDER BY memory_id`) {
		ids = append(ids, row["memory_id"].(string))
	}
	return ids
}

func (e *ingestEnv) metaSHA() string {
	e.t.Helper()
	rows := e.rowsOf(`SELECT value FROM meta WHERE key = ?`, metaLedgerSHA)
	if len(rows) != 1 {
		return ""
	}
	return rows[0]["value"].(string)
}

func (e *ingestEnv) wantReads(r *stampReader, n int, when string) {
	e.t.Helper()
	if got := r.reads(); got != n {
		e.t.Fatalf("%s: %d ledger reads, want %d", when, got, n)
	}
}

// An unchanged, settled stamp spares the second read, and the table stays
// as the first read left it; a changed stamp reads again.
func TestCatchUpSkipsAnUnchangedJournal(t *testing.T) {
	e, r := skipEnv(t)
	e.catchUp(time.Minute)
	e.wantReads(r, 1, "first catch-up")
	e.catchUp(time.Minute)
	e.wantReads(r, 1, "second catch-up, stamp unchanged")
	if ids := e.ledgerIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("ledger table %v", ids)
	}

	r.setLedger(ledgerOf(map[string]feedback.Entry{"m2": {Shown: 1}}), nil)
	r.setStamp(stampAt(clock.Add(-time.Hour), 11))
	e.catchUp(time.Minute)
	e.wantReads(r, 2, "stamp changed")
	if ids := e.ledgerIDs(); len(ids) != 1 || ids[0] != "m2" {
		t.Fatalf("after the change: ledger table %v", ids)
	}
}

// The skip trusts the stamp only while the index still mirrors the read
// it recorded: another process storing another ledger (an older read), or
// an error row, makes the next catch-up read again and restore the table.
func TestSkipRechecksTheMirroredSHA(t *testing.T) {
	e, r := skipEnv(t)
	e.catchUp(time.Minute)
	sum := e.metaSHA()
	if sum == "" {
		t.Fatal("no ledger SHA after the first catch-up")
	}
	if _, err := e.ix.sh.w.Exec(`DELETE FROM ledger; UPDATE meta SET value = 'older-read' WHERE key = ?`, metaLedgerSHA); err != nil {
		t.Fatal(err)
	}
	e.catchUp(time.Minute)
	e.wantReads(r, 2, "after another writer's SHA")
	if ids := e.ledgerIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("table not restored: %v", ids)
	}
	if got := e.metaSHA(); got != sum {
		t.Fatalf("SHA %q, want %q", got, sum)
	}

	e.addError(ledgerSource, 0, errLedgerUnreadable, clock)
	e.catchUp(time.Minute)
	e.wantReads(r, 3, "after another writer's error row")
	if rows := e.errorsOf(errLedgerUnreadable); len(rows) != 0 {
		t.Fatalf("error row kept: %v", rows)
	}
	e.catchUp(time.Minute)
	e.wantReads(r, 3, "mirrored again")
}

// A stamp is recorded only when its newest file is more than stampSettle
// older than the read's start: a file written in the same mtime tick as
// the read could change again without changing its stamp.
func TestStampMustBeSettledAtReadTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		age     time.Duration
		settled bool
	}{
		{"1 s old", time.Second, false},
		{"exactly the settle", stampSettle, false},
		{"1 ns past the settle", stampSettle + time.Nanosecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsOf(t, FSLocal)
			r := &stampReader{l: ledgerOf(map[string]feedback.Entry{"m1": {Shown: 3}}), st: stampAt(clock.Add(-tc.age), 10)}
			e := envWith(t, func(o *Options) { o.Ledger = r })
			e.catchUp(time.Minute)
			e.clock.Add(time.Hour)
			e.catchUp(time.Minute)
			if tc.settled {
				e.wantReads(r, 1, "settled at the first read")
				return
			}
			e.wantReads(r, 2, "unsettled at the first read")
			// Settled at the second read (an hour on): that one is recorded.
			e.catchUp(time.Minute)
			e.wantReads(r, 2, "settled at the second read")
		})
	}
}

// On a network or synced feedback directory (mtimes not to be trusted),
// every catch-up reads the ledger, whatever the index directory is.
func TestSkipOffOnNetworkState(t *testing.T) {
	for _, kind := range []FSKind{FSNetwork, FSSynced} {
		t.Run(string(kind), func(t *testing.T) {
			fsOf(t, kind)
			r := &stampReader{l: ledgerOf(map[string]feedback.Entry{"m1": {Shown: 3}}), st: stampAt(clock.Add(-time.Hour), 10)}
			e := envWith(t, func(o *Options) { o.Ledger = r })
			if e.ix.FS().Kind != FSLocal {
				t.Fatalf("index directory %v, want local", e.ix.FS())
			}
			for i := range 3 {
				e.catchUp(time.Minute)
				e.wantReads(r, i+1, "catch-up")
			}
		})
	}
}

// A feedback directory not created yet is judged by the directory it will
// be created in: on a network state directory, every catch-up reads.
func TestSkipOffOnANetworkStateBeforeTheFeedbackDirExists(t *testing.T) {
	old := detectFS
	t.Cleanup(func() { detectFS = old })
	detectFS = func(dir, _ string, _ func(string) string) FSInfo {
		if filepath.Base(dir) == "state" {
			return FSInfo{Kind: FSNetwork, Name: "nfs"}
		}
		return FSInfo{Kind: FSLocal} // what a statfs of a missing path falls back to
	}
	r := &stampReader{l: ledgerOf(map[string]feedback.Entry{"m1": {Shown: 3}}), st: stampAt(clock.Add(-time.Hour), 10)}
	e := envWith(t, func(o *Options) { o.Ledger = r })
	if _, err := os.Stat(e.d.FeedbackDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the feedback directory exists at open: %v", err)
	}
	for i := range 3 {
		e.catchUp(time.Minute)
		e.wantReads(r, i+1, "catch-up")
	}
}

// A failed read records its error row and no stamp: the next catch-up,
// the stamp unchanged, reads again and clears the row, and the one after
// skips.
func TestCatchUpRereadsAfterAFailure(t *testing.T) {
	e, r := skipEnv(t)
	r.setLedger(feedback.Ledger{}, errors.New("unreadable"))
	e.catchUp(time.Minute)
	if rows := e.errorsOf(errLedgerUnreadable); len(rows) != 1 {
		t.Fatalf("error rows %v", rows)
	}
	r.setLedger(ledgerOf(map[string]feedback.Entry{"m1": {Shown: 3}}), nil)
	e.catchUp(time.Minute)
	e.wantReads(r, 2, "after the failure")
	if rows := e.errorsOf(errLedgerUnreadable); len(rows) != 0 {
		t.Fatalf("error row kept: %v", rows)
	}
	if ids := e.ledgerIDs(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("ledger table %v", ids)
	}
	e.catchUp(time.Minute)
	e.wantReads(r, 2, "after the good read")
}

// writeDuringRead is a stampReader whose read sees the ledger as it was
// while a write lands meanwhile: the stamp changes, the next read sees it.
type writeDuringRead struct {
	*stampReader
	once sync.Once
}

func (w *writeDuringRead) All() (feedback.Ledger, error) {
	l, err := w.stampReader.All()
	w.once.Do(func() {
		w.setStamp(stampAt(clock.Add(-time.Hour), 20))
		w.setLedger(ledgerOf(map[string]feedback.Entry{"m2": {Shown: 1}}), nil)
	})
	return l, err
}

// The stamp is taken before the read: a write landing during the read
// changes the stamp the next catch-up takes, which reads it.
func TestAWriteDuringTheReadIsReadAgain(t *testing.T) {
	fsOf(t, FSLocal)
	r := &writeDuringRead{stampReader: &stampReader{l: ledgerOf(map[string]feedback.Entry{"m1": {Shown: 3}}), st: stampAt(clock.Add(-time.Hour), 10)}}
	e := envWith(t, func(o *Options) { o.Ledger = r })
	e.catchUp(time.Minute)
	e.catchUp(time.Minute)
	e.wantReads(r.stampReader, 2, "after a write during the first read")
	if ids := e.ledgerIDs(); len(ids) != 1 || ids[0] != "m2" {
		t.Fatalf("ledger table %v, want [m2]", ids)
	}
}

// A failed read forgets the recorded stamp: should the files come back to
// it (an mtime-preserving restore of other content), the next catch-up
// reads rather than trusting the stamp of a read before the failure.
func TestAFailureForgetsTheRecordedStamp(t *testing.T) {
	e, r := skipEnv(t)
	recorded := stampAt(clock.Add(-time.Hour), 10)
	e.catchUp(time.Minute)
	e.wantReads(r, 1, "first catch-up")

	r.setStamp(stampAt(clock.Add(-time.Hour), 11))
	r.setLedger(feedback.Ledger{}, errors.New("unreadable"))
	e.catchUp(time.Minute)
	e.wantReads(r, 2, "a changed stamp, the reader failing")
	if _, err := e.ix.sh.w.Exec(`DELETE FROM ingest_errors`); err != nil { // another process's good read of the same ledger
		t.Fatal(err)
	}

	r.setStamp(recorded)
	r.setLedger(ledgerOf(map[string]feedback.Entry{"m3": {Shown: 1}}), nil)
	e.catchUp(time.Minute)
	e.wantReads(r, 3, "the recorded stamp back after a failure")
	if ids := e.ledgerIDs(); len(ids) != 1 || ids[0] != "m3" {
		t.Fatalf("ledger table %v, want [m3]", ids)
	}
}

// countReader is a LedgerReader without a stamp.
type countReader struct {
	mu   sync.Mutex
	alls int
}

func (r *countReader) All() (feedback.Ledger, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alls++
	return ledgerOf(map[string]feedback.Entry{"m1": {Shown: 1}}), nil
}

func TestCatchUpWithoutStampReadsEveryTime(t *testing.T) {
	fsOf(t, FSLocal)
	r := &countReader{}
	e := envWith(t, func(o *Options) { o.Ledger = r })
	for i := range 3 {
		e.catchUp(time.Minute)
		r.mu.Lock()
		n := r.alls
		r.mu.Unlock()
		if n != i+1 {
			t.Fatalf("catch-up %d: %d reads", i+1, n)
		}
	}
}

// On real files: a forget appended to the journal, its mtime put back to
// what it was (only its size changes), reaches the next catch-up's table.
func TestCatchUpSeesAJournalAppend(t *testing.T) {
	fsOf(t, FSLocal)
	e := envWith(t, func(*Options) {})
	dir := e.d.FeedbackDir()
	old := clock.Add(-time.Hour)
	ledger := filepath.Join(dir, "ledger.json")
	journal := e.jpath("2026-10")
	writeFile(t, ledger, `{"schema":1,"watermark":"2026-10-30T10:00:00Z","memories":{"m1":{"shown":4},"m2":{"shown":2}}}`)
	writeFile(t, journal, jline(t, feedback.Event{TS: clock.Add(-90 * time.Minute), Kind: "turn"}))
	for _, p := range []string{ledger, journal} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	e.catchUp(time.Minute)
	if ids := e.ledgerIDs(); len(ids) != 2 {
		t.Fatalf("first catch-up: %v", ids)
	}
	e.catchUp(time.Minute) // skip-eligible: the stamp is recorded and unchanged

	f, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(jline(t, feedback.Event{TS: clock.Add(-30 * time.Minute), Kind: "forget", IDs: []string{"m1"}})); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(journal, old, old); err != nil {
		t.Fatal(err)
	}
	e.catchUp(time.Minute)
	if ids := e.ledgerIDs(); len(ids) != 1 || ids[0] != "m2" {
		t.Fatalf("after the forget: ledger table %v, want [m2]", ids)
	}
}

// On real files: a hand-edited ledger.json whose journal_at names a file
// outside the journal glob; a forget appended to that file, its mtime put
// back, reaches the next catch-up's table.
func TestCatchUpSeesAnAppendToTheFileJournalAtNames(t *testing.T) {
	fsOf(t, FSLocal)
	e := envWith(t, func(*Options) {})
	dir := e.d.FeedbackDir()
	old := clock.Add(-time.Hour)
	ledger := filepath.Join(dir, "ledger.json")
	notes := filepath.Join(dir, "notes.jsonl")
	writeFile(t, ledger, `{"schema":1,"watermark":"2026-10-30T10:00:00Z","journal_at":{"file":"notes.jsonl","offset":0},"memories":{"m1":{"shown":4},"m2":{"shown":2}}}`)
	writeFile(t, notes, "")
	for _, p := range []string{ledger, notes} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	e.catchUp(time.Minute)
	e.catchUp(time.Minute) // skip-eligible: the stamp is recorded and unchanged
	if ids := e.ledgerIDs(); len(ids) != 2 {
		t.Fatalf("before the forget: %v", ids)
	}
	writeFile(t, notes, jline(t, feedback.Event{TS: clock.Add(-30 * time.Minute), Kind: "forget", IDs: []string{"m1"}}))
	if err := os.Chtimes(notes, old, old); err != nil {
		t.Fatal(err)
	}
	e.catchUp(time.Minute)
	if ids := e.ledgerIDs(); len(ids) != 1 || ids[0] != "m2" {
		t.Fatalf("after the forget: ledger table %v, want [m2]", ids)
	}
}
