package index

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// tree is every file under the state directory with its bytes: what a
// read-only command must leave as it found it.
func tree(t *testing.T, d paths.Dirs) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(d.State, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || strings.HasSuffix(p, "-shm") { // -shm is lock state, not data
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Fatal(rerr)
		}
		out[p] = string(b)
		return nil
	})
	return out
}

func sameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	var names []string
	for p := range before {
		names = append(names, p)
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	for _, p := range slices.Compact(names) {
		b, inB := before[p]
		a, inA := after[p]
		switch {
		case inB && !inA:
			t.Errorf("%s was removed", p)
		case !inB && inA:
			t.Errorf("%s was created", p)
		case a != b:
			t.Errorf("%s changed", p)
		}
	}
}

func inspect(t *testing.T, o Options) Status {
	t.Helper()
	st, err := Inspect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestInspectNotBuilt(t *testing.T) {
	d := tempDirs(t)
	st := inspect(t, opts(t, d, nil))
	if st.Exists || st.Readable || st.Using != FileName || st.Head != 1 || st.Schema != 0 || st.Size != 0 {
		t.Errorf("%+v", st)
	}
	if len(st.Sources) != 0 || st.SideFiles != nil || st.AsideFiles != nil {
		t.Errorf("%+v", st)
	}
	if _, err := os.Stat(d.State); err == nil {
		t.Error("Inspect created the state directory")
	}
}

func TestInspectShowsItsState(t *testing.T) {
	e := newEnv(t)
	bad := "not json at all\n" + tline("prompt", "", map[string]any{"decision": "skip"}) // unparsable, no_ts
	e.write("2026-08", promptLine(1, 0))
	e.write("2026-09", promptLine(2, 0)+bad)
	e.ingest()
	if err := os.Remove(e.telPath("2026-08")); err != nil {
		t.Fatal(err)
	}
	e.ingest() // marks 2026-08 missing
	e.append("2026-09", promptLine(3, 0))
	e.clock.Add(time.Hour)
	rows := e.count("tel_prompt")

	o := opts(t, e.d, nil)
	o.Now = e.clock.Now
	before := tree(t, e.d)
	st := inspect(t, o)
	sameTree(t, before, tree(t, e.d))
	if e.count("tel_prompt") != rows {
		t.Error("Inspect ingested")
	}

	if !st.Exists || !st.Readable || st.Schema != 1 || st.Head != 1 || st.Pending != 0 || st.Newer || st.Using != FileName {
		t.Errorf("%+v", st)
	}
	if st.JournalMode != "wal" || st.Filesystem.Kind != FSLocal || st.Size <= 0 {
		t.Errorf("%+v", st)
	}
	if st.File != filepath.Join(e.d.IndexDir(), FileName) {
		t.Errorf("file %s", st.File)
	}
	if st.Errors30d != 2 || st.Errors24h != 2 || st.ErrorsByReason["unparsable"] != 1 || st.ErrorsByReason["no_ts"] != 1 {
		t.Errorf("errors %d/%d %v", st.Errors30d, st.Errors24h, st.ErrorsByReason)
	}
	if want := mustMS(ts0); st.IngestedAt.UnixMilli() != want {
		t.Errorf("ingested at %v", st.IngestedAt)
	}
	by := map[string]SourceLag{}
	for _, s := range st.Sources {
		by[s.File] = s
	}
	gone, behind := by["telemetry/events-2026-08.jsonl"], by["telemetry/events-2026-09.jsonl"]
	if !gone.Missing || gone.LagSeconds != 0 {
		t.Errorf("gone %+v", gone)
	}
	if behind.Missing || behind.LagSeconds != 3600 || behind.Offset >= behind.Size || behind.Size == 0 {
		t.Errorf("behind %+v", behind)
	}
	if st.LagSeconds != 3600 {
		t.Errorf("lag %d", st.LagSeconds)
	}
}

// A source the cursors do not know yet is behind, its lag from the file's
// mtime; a file at its end has none.
func TestInspectLagOfUnknownAndCaughtUpFiles(t *testing.T) {
	e := newEnv(t)
	e.write("2026-09", promptLine(1, 0))
	e.ingest()
	e.write("2026-10", promptLine(2, 0)) // no cursor yet
	mtime := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(e.telPath("2026-10"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	o := opts(t, e.d, nil)
	st := inspect(t, o) // clock 12:00
	by := map[string]SourceLag{}
	for _, s := range st.Sources {
		by[s.File] = s
	}
	if s := by["telemetry/events-2026-09.jsonl"]; s.LagSeconds != 0 || s.Offset != s.Size {
		t.Errorf("caught up %+v", s)
	}
	if s := by["telemetry/events-2026-10.jsonl"]; s.LagSeconds != 7200 || s.Offset != 0 || s.Size == 0 {
		t.Errorf("unknown %+v", s)
	}
}

func TestInspectNamesSideAndAsideFiles(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, regB()))
	build(t, opts(t, d, regA())) // the side file of the older binary
	writeFile(t, mainFile(d)+".corrupt", "set aside")
	before := tree(t, d)
	st := inspect(t, opts(t, d, regA()))
	sameTree(t, before, tree(t, d))
	if !st.Newer || st.Using != "metrics-v1.db" || !st.Exists || !st.Readable || st.Schema != 1 || st.Head != 1 {
		t.Errorf("%+v", st)
	}
	if !slices.Equal(st.SideFiles, []string{"metrics-v1.db"}) || !slices.Equal(st.AsideFiles, []string{"metrics.db.corrupt"}) {
		t.Errorf("side %v aside %v", st.SideFiles, st.AsideFiles)
	}
	// the binary that wrote metrics.db sees no newer index
	st = inspect(t, opts(t, d, regB()))
	if st.Newer || st.Using != FileName || st.Schema != 2 {
		t.Errorf("%+v", st)
	}
}

// A newer index whose side file is not built yet: nothing in use.
func TestInspectNewerWithoutSideFile(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, regB()))
	before := tree(t, d)
	st := inspect(t, opts(t, d, regA()))
	sameTree(t, before, tree(t, d))
	if !st.Newer || st.Using != "metrics-v1.db" || st.Exists || st.Readable {
		t.Errorf("%+v", st)
	}
}

func TestInspectCountsPendingMigrations(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, regA()))
	// an update that is not committed: Inspect is no migration
	if err := state.WriteUpdateMarker(d, state.UpdateMarker{}); err != nil {
		t.Fatal(err)
	}
	before := tree(t, d)
	st := inspect(t, opts(t, d, regB()))
	sameTree(t, before, tree(t, d))
	if st.Schema != 1 || st.Head != 2 || st.Pending != 1 || st.Newer || !st.Readable {
		t.Errorf("%+v", st)
	}
}

// A file that is no database, or whose pages are damaged: reported
// unreadable, never set aside, rebuilt or touched.
func TestInspectOfAnUnreadableIndex(t *testing.T) {
	t.Run("not a database", func(t *testing.T) {
		d := tempDirs(t)
		writeFile(t, mainFile(d), "not a database")
		before := tree(t, d)
		st := inspect(t, opts(t, d, nil))
		sameTree(t, before, tree(t, d))
		if !st.Exists || st.Readable || st.Size != int64(len("not a database")) || len(st.Sources) != 0 {
			t.Errorf("%+v", st)
		}
	})
	t.Run("damaged pages", func(t *testing.T) {
		d := tempDirs(t)
		e := envAt(t, d)
		e.write("2026-09", promptLine(1, 0))
		e.ingest()
		_ = e.ix.Close()
		info, err := os.Stat(mainFile(d))
		if err != nil {
			t.Fatal(err)
		}
		for p := int64(2); p <= info.Size()/4096; p++ {
			corruptPage(t, mainFile(d), p)
		}
		before := tree(t, d)
		st := inspect(t, opts(t, d, nil))
		sameTree(t, before, tree(t, d))
		if !st.Exists || st.Readable {
			t.Errorf("%+v", st)
		}
	})
}

func TestInspectReportsTheRecoveryState(t *testing.T) {
	d := tempDirs(t)
	build(t, opts(t, d, nil))
	at := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	if err := updateStatus(d, func(st *recoveryState) bool {
		st.RebuiltAt, st.StoppedAt, st.MigrateFailedAt, st.MigrateReason = at, at.Add(time.Hour), at.Add(2*time.Hour), "other"
		return true
	}); err != nil {
		t.Fatal(err)
	}
	st := inspect(t, opts(t, d, nil))
	if !st.RebuiltAt.Equal(at) || !st.StoppedAt.Equal(at.Add(time.Hour)) || !st.MigrateFailedAt.Equal(at.Add(2*time.Hour)) {
		t.Errorf("%+v", st)
	}
}

func TestInspectNamesTheFilesystemAndJournalMode(t *testing.T) {
	old := detectFS
	t.Cleanup(func() { detectFS = old })
	detectFS = func(string, string, func(string) string) FSInfo { return FSInfo{FSNetwork, "nfs"} }
	d := tempDirs(t)
	build(t, opts(t, d, nil)) // a network folder: the rollback journal
	st := inspect(t, opts(t, d, nil))
	if st.Filesystem != (FSInfo{FSNetwork, "nfs"}) || st.JournalMode != "delete" {
		t.Errorf("%+v", st)
	}
	raw, _ := json.Marshal(st.Filesystem)
	if !bytes.Equal(raw, []byte(`{"kind":"network","name":"nfs"}`)) {
		t.Errorf("%s", raw)
	}
}

// The size is the file's and its write-ahead log's.
func TestInspectSizeCountsTheWAL(t *testing.T) {
	d := tempDirs(t)
	e := envAt(t, d)
	e.write("2026-09", promptLine(1, 0))
	e.ingest()
	wal, err := os.Stat(mainFile(d) + "-wal")
	if err != nil || wal.Size() == 0 {
		t.Skipf("no -wal to count (%v)", err)
	}
	main, _ := os.Stat(mainFile(d))
	st := inspect(t, opts(t, d, nil))
	if st.Size != main.Size()+wal.Size() {
		t.Errorf("size %d, want %d+%d", st.Size, main.Size(), wal.Size())
	}
}
