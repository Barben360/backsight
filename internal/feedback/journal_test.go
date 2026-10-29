package feedback

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

var common = Common{Session: "s1", Project: "backsight", Binary: "0.1.8", V: "7"}

// TestHelperAppend is the child process of TestConcurrentAppendsStayWhole:
// it appends from another process to the same journal.
func TestHelperAppend(t *testing.T) {
	dir := os.Getenv("BACKSIGHT_FEEDBACK_HELPER_DIR")
	if dir == "" {
		t.Skip("child process only")
	}
	n, _ := strconv.Atoi(os.Getenv("BACKSIGHT_FEEDBACK_HELPER_N"))
	j := NewJournal(dir, Common{Session: os.Getenv("BACKSIGHT_FEEDBACK_HELPER_SESSION")})
	for range n {
		if err := j.Append(Event{Kind: "shows", Path: "search", Items: []Item{{ID: "m1", Rank: 1, Reranker: 0.9}}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentAppendsStayWhole(t *testing.T) {
	dir := t.TempDir()
	var children []*exec.Cmd
	for i := range 2 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperAppend$", "-test.count=1")
		cmd.Env = append(os.Environ(), "BACKSIGHT_FEEDBACK_HELPER_DIR="+dir, "BACKSIGHT_FEEDBACK_HELPER_N=200",
			fmt.Sprintf("BACKSIGHT_FEEDBACK_HELPER_SESSION=child-%d", i))
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, cmd)
	}
	j := NewJournal(dir, common)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				if err := j.Append(Event{Kind: "shows", Path: "prompt", Items: []Item{{ID: "m2", Rank: 2}}, IDs: []string{"a", "b"}}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	for _, c := range children {
		if err := c.Wait(); err != nil {
			t.Fatalf("child: %v", err)
		}
	}
	events, skipped, err := Read(dir, time.Now().AddDate(0, -1, 0))
	if err != nil || skipped != 0 || len(events) != 2000 {
		t.Fatalf("got %d events, %d skipped, err %v; want 2000, 0, nil", len(events), skipped, err)
	}
	seen := map[string]bool{}
	perSession := map[string]int{}
	for _, e := range events {
		k := fmt.Sprintf("%s/%d", e.Session, e.Seq)
		if seen[k] {
			t.Fatalf("(session, seq) %s twice", k)
		}
		seen[k] = true
		perSession[e.Session]++
		if e.Schema != Schema || e.TS.IsZero() || e.Kind != "shows" {
			t.Fatalf("event not filled: %+v", e)
		}
	}
	if perSession["s1"] != 1600 || perSession["child-0"] != 200 || perSession["child-1"] != 200 {
		t.Errorf("per session: %v", perSession)
	}
}

func TestAppendFillsCommonFieldsAndKeepsTheFilePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	j := NewJournal(dir, common)
	at := time.Date(2026, 10, 28, 11, 0, 0, 123456789, time.FixedZone("CET", 3600))
	j.now = func() time.Time { return at }
	before := at
	if err := j.Append(Event{Kind: "credit", Turn: "t1", IDs: []string{"m1"}, Verdict: "used"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Event{Kind: "turn", Session: "other", Project: "p2"}); err != nil {
		t.Fatal(err)
	}
	events, skipped, err := Read(dir, before)
	if err != nil || skipped != 0 || len(events) != 2 {
		t.Fatalf("%d events, %d skipped, %v", len(events), skipped, err)
	}
	e := events[0]
	if e.Session != "s1" || e.Project != "backsight" || e.Binary != "0.1.8" || e.V != "7" || e.Turn != "t1" || e.Verdict != "used" {
		t.Errorf("common fields not filled: %+v", e)
	}
	if e.TS.Location() != time.UTC || !e.TS.Equal(at) || e.TS.Nanosecond() != 123456789 {
		t.Errorf("ts %v: want %v in UTC, to the nanosecond", e.TS, at)
	}
	if events[1].Session != "other" || events[1].Project != "p2" || events[1].Binary != "0.1.8" {
		t.Errorf("explicit fields overwritten: %+v", events[1])
	}
	if events[1].Seq <= e.Seq {
		t.Errorf("seq not increasing: %d then %d", e.Seq, events[1].Seq)
	}
	file, off := j.Position()
	fi, err := os.Stat(file)
	if err != nil || fi.Size() != off {
		t.Fatalf("position %s:%d, stat %v", file, off, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("journal mode %v, want 0600", fi.Mode().Perm())
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", di.Mode().Perm())
	}
	if filepath.Base(file) != "journal-2026-10.jsonl" {
		t.Errorf("file %s", file)
	}
}

func TestReadSkipsTornAndUnknownLines(t *testing.T) {
	dir := t.TempDir()
	j := NewJournal(dir, common)
	for range 3 {
		if err := j.Append(Event{Kind: "turn", N: 1}); err != nil {
			t.Fatal(err)
		}
	}
	file, _ := j.Position()
	appendRaw(t, file, `{"schema":1,"seq":4,"ts":"2026-`+"\n")
	appendRaw(t, file, `{"schema":99,"seq":5,"ts":"2026-10-28T10:00:00Z","kind":"turn"}`+"\n")
	appendRaw(t, file, `{"schema":1,"seq":6,"ts":"2026-10-28T10:00:00Z","kind":"stop","outcome":"declared"}`+"\n")
	events, skipped, err := Read(dir, time.Now())
	if err != nil || len(events) != 4 || skipped != 2 {
		t.Fatalf("got %d events, %d skipped, %v; want 4, 2", len(events), skipped, err)
	}
	if events[3].Kind != "stop" || events[3].Outcome != "declared" {
		t.Errorf("last event %+v", events[3])
	}

	// A crash mid-write leaves a line without its newline: the next append
	// starts a line of its own, so only the torn line is lost.
	appendRaw(t, file, `{"schema":1,"seq":7,"kind":"tu`)
	if err := j.Append(Event{Kind: "settle"}); err != nil {
		t.Fatal(err)
	}
	events, skipped, err = Read(dir, time.Now())
	if err != nil || len(events) != 5 || skipped != 3 || events[4].Kind != "settle" {
		t.Fatalf("after a torn last line: %d events, %d skipped, %v", len(events), skipped, err)
	}
}

func TestReadSpansMonths(t *testing.T) {
	dir := t.TempDir()
	j := NewJournal(dir, common)
	thisMonth := time.Now().UTC()
	thisMonth = time.Date(thisMonth.Year(), thisMonth.Month(), 1, 0, 0, 1, 0, time.UTC)
	lastMonth := thisMonth.Add(-2 * time.Second) // the last second of the month before
	for _, at := range []time.Time{lastMonth, thisMonth} {
		j.now = func() time.Time { return at }
		if err := j.Append(Event{Kind: "turn", Turn: at.Format("2006-01")}); err != nil {
			t.Fatal(err)
		}
	}
	events, skipped, err := Read(dir, lastMonth)
	if err != nil || skipped != 0 || len(events) != 2 {
		t.Fatalf("%d events, %d skipped, %v", len(events), skipped, err)
	}
	if events[0].Turn != lastMonth.Format("2006-01") || events[1].Turn != thisMonth.Format("2006-01") {
		t.Errorf("order: %s then %s", events[0].Turn, events[1].Turn)
	}
	if !events[0].TS.Equal(lastMonth) {
		t.Errorf("ts %v, want %v", events[0].TS, lastMonth)
	}
	events, _, _ = Read(dir, thisMonth)
	if len(events) != 1 || events[0].Turn != thisMonth.Format("2006-01") {
		t.Errorf("from this month: %+v", events)
	}
	if events, skipped, err := Read(filepath.Join(dir, "missing"), lastMonth); err != nil || skipped != 0 || len(events) != 0 {
		t.Errorf("missing dir: %d events, %d skipped, %v", len(events), skipped, err)
	}
}

func TestReadFromAnOffset(t *testing.T) {
	dir := t.TempDir()
	j := NewJournal(dir, common)
	if err := j.Append(Event{Kind: "turn", Turn: "t1"}); err != nil {
		t.Fatal(err)
	}
	file, off := j.Position()
	if err := j.Append(Event{Kind: "shows", Turn: "t2", Items: []Item{{ID: "m1", Rank: 1}}}); err != nil {
		t.Fatal(err)
	}
	events, skipped, err := ReadFrom(file, off)
	if err != nil || skipped != 0 || len(events) != 1 || events[0].Turn != "t2" || events[0].Items[0].ID != "m1" {
		t.Fatalf("%+v, %d skipped, %v", events, skipped, err)
	}
	if events, _, err := ReadFrom(filepath.Join(dir, "journal-1999-01.jsonl"), 0); err != nil || len(events) != 0 {
		t.Errorf("missing file: %v, %v", events, err)
	}
}

func appendRaw(t *testing.T, file, s string) {
	t.Helper()
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// A turn that began in one month and ends in the next reads both files:
// the first from its offset, the later one whole.
func TestReadSinceCrossesAMonth(t *testing.T) {
	dir := t.TempDir()
	j := NewJournal(dir, common)
	thisMonth := time.Now().UTC()
	thisMonth = time.Date(thisMonth.Year(), thisMonth.Month(), 1, 0, 0, 1, 0, time.UTC)
	lastMonth := thisMonth.Add(-2 * time.Second)
	j.now = func() time.Time { return lastMonth }
	if err := j.Append(Event{Kind: "turn", Turn: "t1"}); err != nil {
		t.Fatal(err)
	}
	file, off := j.Position()
	for _, at := range []time.Time{lastMonth, thisMonth} {
		j.now = func() time.Time { return at }
		if err := j.Append(Event{Kind: "shows", Turn: "t2", Path: at.Format("2006-01")}); err != nil {
			t.Fatal(err)
		}
	}
	events, skipped, err := ReadSince(file, off)
	if err != nil || skipped != 0 || len(events) != 2 || events[0].Path != lastMonth.Format("2006-01") || events[1].Path != thisMonth.Format("2006-01") {
		t.Fatalf("%+v, %d skipped, %v", events, skipped, err)
	}
	if _, _, err := ReadSince("", 0); err == nil {
		t.Error("no position: want an error, not the working directory's journals")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadSince(file, off); err == nil {
		t.Error("a journal that is a directory read without an error")
	}
}

// A line goes to the month file of its own ts: an event of 31 January
// 23:59:59 written on 1 February is January's. One without a ts takes the
// clock's month.
func TestJournalMonthFromEventTime(t *testing.T) {
	dir := t.TempDir()
	j := NewJournal(dir, common)
	j.now = func() time.Time { return time.Date(2026, 2, 1, 0, 0, 5, 0, time.UTC) }
	if err := j.Append(Event{Kind: "turn", Turn: "late", TS: time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Event{Kind: "turn", Turn: "now"}); err != nil {
		t.Fatal(err)
	}
	for file, turn := range map[string]string{"journal-2026-01.jsonl": "late", "journal-2026-02.jsonl": "now"} {
		events, _, err := ReadFrom(filepath.Join(dir, file), 0)
		if err != nil || len(events) != 1 || events[0].Turn != turn {
			t.Errorf("%s: %+v %v, want the %q turn alone", file, events, err, turn)
		}
	}
}
