package feedback

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// stampOf is r's stamp, which must come without an error.
func stampOf(t *testing.T, r CurrentReader) Stamp {
	t.Helper()
	st, err := r.Stamp()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// put writes content to dir/name with its mtime set to mt.
func put(t *testing.T, dir, name, content string, mt time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func names(st Stamp) []string {
	var out []string
	for _, f := range st.Files {
		out = append(out, f.Name)
	}
	return out
}

// The stamp names ledger.json and every journal file All may read, sorted,
// with each one's size and nanosecond mtime: an append, a same-size
// rewrite, a new month and a deleted file each change it; other files do
// not appear.
func TestStampSeesEveryChange(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	tm := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	put(t, dir, "ledger.json", `{"schema":1,"memories":{}}`, tm(20))
	put(t, dir, "journal-2026-10.jsonl", "{}\n", tm(30)) // the newest
	put(t, dir, "journal-2026-09.jsonl", "{}\n{}\n", tm(10))
	put(t, dir, "notes.txt", "x", tm(90))
	put(t, dir, "journal-2026-10.jsonl.bak", "x", tm(91))
	r := CurrentReader{Dir: dir}

	st := stampOf(t, r)
	want := []StampFile{
		{Name: "journal-2026-09.jsonl", Size: 6, ModTime: tm(10).UnixNano()},
		{Name: "journal-2026-10.jsonl", Size: 3, ModTime: tm(30).UnixNano()},
		{Name: "ledger.json", Size: 26, ModTime: tm(20).UnixNano()},
	}
	if !slices.Equal(st.Files, want) {
		t.Fatalf("files %+v, want %+v", st.Files, want)
	}
	if !st.Newest.Equal(tm(30)) {
		t.Errorf("newest %v, want %v", st.Newest, tm(30))
	}
	if again := stampOf(t, r); !again.Equal(st) || !st.Equal(again) {
		t.Fatalf("two stamps of an unchanged directory differ: %+v, %+v", st, again)
	}

	// An append, the mtime put back: the size alone tells.
	f, err := os.OpenFile(filepath.Join(dir, "journal-2026-09.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "journal-2026-09.jsonl"), tm(10), tm(10)); err != nil {
		t.Fatal(err)
	}
	appended := stampOf(t, r)
	if appended.Equal(st) || appended.Files[0].Size != 9 || appended.Files[0].ModTime != tm(10).UnixNano() {
		t.Fatalf("after an append: %+v (before %+v)", appended.Files, st.Files)
	}

	// ledger.json rewritten with the same size, 1 µs later: the mtime alone
	// tells (1 µs, not 1 ns: NTFS keeps mtimes in 100 ns steps).
	put(t, dir, "ledger.json", `{"schema":1,"memories":{}}`, tm(20).Add(time.Microsecond))
	rewritten := stampOf(t, r)
	if rewritten.Equal(appended) || rewritten.Files[2].Size != 26 || rewritten.Files[2].ModTime != tm(20).Add(time.Microsecond).UnixNano() {
		t.Fatalf("after a same-size rewrite: %+v (before %+v)", rewritten.Files, appended.Files)
	}

	// A new month, back-dated: one more entry, the newest unchanged.
	put(t, dir, "journal-2026-11.jsonl", "", tm(5))
	month := stampOf(t, r)
	if got := names(month); !slices.Equal(got, []string{"journal-2026-09.jsonl", "journal-2026-10.jsonl", "journal-2026-11.jsonl", "ledger.json"}) {
		t.Fatalf("after a new month: %v", got)
	}
	if month.Equal(rewritten) || !month.Newest.Equal(tm(30)) {
		t.Fatalf("after a new month: equal %v, newest %v", month.Equal(rewritten), month.Newest)
	}

	// Any journal-*.jsonl is read by All (its glob), so it is stamped too.
	put(t, dir, "journal-old.jsonl", "{}\n", tm(1))
	if got := names(stampOf(t, r)); !slices.Contains(got, "journal-old.jsonl") {
		t.Fatalf("a journal file All reads is not stamped: %v", got)
	}
	if err := os.Remove(filepath.Join(dir, "journal-old.jsonl")); err != nil {
		t.Fatal(err)
	}

	// A deleted file: one entry fewer, and the newest follows.
	if err := os.Remove(filepath.Join(dir, "journal-2026-10.jsonl")); err != nil {
		t.Fatal(err)
	}
	gone := stampOf(t, r)
	if got := names(gone); !slices.Equal(got, []string{"journal-2026-09.jsonl", "journal-2026-11.jsonl", "ledger.json"}) {
		t.Fatalf("after a deletion: %v", got)
	}
	if gone.Equal(month) || !gone.Newest.Equal(tm(20).Add(time.Microsecond)) {
		t.Fatalf("after a deletion: equal %v, newest %v", gone.Equal(month), gone.Newest)
	}

	// The same files with another Newest are not the same stamp.
	other := gone
	other.Newest = other.Newest.Add(time.Second)
	if other.Equal(gone) {
		t.Error("stamps with different Newest are equal")
	}
}

// A directory that does not exist is an empty stamp (All reads an empty
// ledger there), and a file appearing in it changes it.
func TestStampOfAMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	r := CurrentReader{Dir: dir}
	empty := stampOf(t, r)
	if len(empty.Files) != 0 || !empty.Newest.IsZero() {
		t.Fatalf("missing directory: %+v", empty)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	put(t, dir, "ledger.json", "{}", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if st := stampOf(t, r); st.Equal(empty) {
		t.Fatalf("a new ledger.json left the stamp unchanged: %+v", st)
	}
}

// A file that cannot be stat'ed, or a directory that cannot be listed, is
// the stamp's error: never a stamp without that file.
func TestStampSurfacesStatErrors(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "ledger.json", "{}", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	put(t, dir, "journal-2026-10.jsonl", "{}\n", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	boom := errors.New("stat-canary")
	oldStat, oldReadDir := stampStat, stampReadDir
	t.Cleanup(func() { stampStat, stampReadDir = oldStat, oldReadDir })

	stampStat = func(p string) (os.FileInfo, error) {
		if filepath.Base(p) == "journal-2026-10.jsonl" {
			return nil, boom
		}
		return oldStat(p)
	}
	if _, err := (CurrentReader{Dir: dir}).Stamp(); !errors.Is(err, boom) {
		t.Errorf("stat error: %v", err)
	}

	// A file listed then gone before its stat is an error too.
	stampStat = func(p string) (os.FileInfo, error) {
		if filepath.Base(p) == "ledger.json" {
			return nil, os.ErrNotExist
		}
		return oldStat(p)
	}
	if _, err := (CurrentReader{Dir: dir}).Stamp(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a listed file gone before its stat: %v", err)
	}

	stampStat = oldStat
	stampReadDir = func(string) ([]os.DirEntry, error) { return nil, boom }
	if _, err := (CurrentReader{Dir: dir}).Stamp(); !errors.Is(err, boom) {
		t.Errorf("read dir error: %v", err)
	}
}

// A hand-edited ledger.json whose journal_at names a file outside the
// journal glob: All reads that file, so the stamp covers it, its absence
// included; a ledger.json that does not parse is no stamp.
func TestStampCoversTheFileJournalAtNames(t *testing.T) {
	dir := t.TempDir()
	mt := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	put(t, dir, "ledger.json", `{"schema":1,"journal_at":{"file":"notes.jsonl","offset":0},"memories":{}}`, mt)
	put(t, dir, "journal-2026-10.jsonl", "", mt)
	r := CurrentReader{Dir: dir}

	missing := stampOf(t, r)
	if !slices.Contains(missing.Files, StampFile{Name: "notes.jsonl", Size: -1}) {
		t.Fatalf("a named file that does not exist is not stamped as such: %+v", missing.Files)
	}
	put(t, dir, "notes.jsonl", "", mt)
	there := stampOf(t, r)
	if there.Equal(missing) || !slices.Contains(there.Files, StampFile{Name: "notes.jsonl", Size: 0, ModTime: mt.UnixNano()}) {
		t.Fatalf("after the named file appeared: %+v", there.Files)
	}
	if got := names(there); !slices.IsSorted(got) || len(got) != 3 {
		t.Fatalf("names %v", got)
	}
	put(t, dir, "notes.jsonl", "{}\n", mt) // an append, the mtime put back
	if grown := stampOf(t, r); grown.Equal(there) {
		t.Fatalf("an append to the named file left the stamp unchanged: %+v", grown.Files)
	}

	// Not a bare name: journalSince does not read it, nor is it stamped.
	put(t, dir, "ledger.json", `{"schema":1,"journal_at":{"file":"../notes.jsonl","offset":0},"memories":{}}`, mt)
	if got := names(stampOf(t, r)); !slices.Equal(got, []string{"journal-2026-10.jsonl", "ledger.json"}) {
		t.Fatalf("a path in journal_at: %v", got)
	}

	put(t, dir, "ledger.json", `{"schema":1,"memories":`, mt)
	if _, err := r.Stamp(); err == nil {
		t.Error("a ledger.json that does not parse gave a stamp")
	}
}
