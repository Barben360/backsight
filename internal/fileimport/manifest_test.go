package fileimport

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

func TestManifestRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "import")
	m := Manifest{Schema: 1, Source: "abc123def456", Root: "/r", Project: "home", QueuedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
		MaxCost: 1.5, StartDay: "2026-11-02", StartBase: [4]int64{1, 2, 3, 4},
		Entries: []Entry{{Rel: "a.md", Document: "import:abc123def456/a.md", SHA256: "h", Sent: true}}}
	if _, ok, err := LoadManifest(dir, m.Source); ok || err != nil {
		t.Fatalf("absent manifest: ok %v err %v", ok, err)
	}
	if err := SaveManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadManifest(dir, m.Source)
	if err != nil || !ok || !reflect.DeepEqual(got, m) {
		t.Fatalf("load = %+v %v %v, want %+v", got, ok, err, m)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, m.Source+".json"))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v %v, want 0600", st.Mode().Perm(), err)
		}
	}
	all, err := ListManifests(dir)
	if err != nil || len(all) != 1 {
		t.Fatalf("list = %v %v", all, err)
	}
	if err := DeleteManifest(dir, m.Source); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := LoadManifest(dir, m.Source); ok {
		t.Error("manifest still there after delete")
	}
}

func TestListManifestsSortsByQueuedAt(t *testing.T) {
	dir := t.TempDir()
	for i, src := range []string{"bbbbbbbbbbbb", "aaaaaaaaaaaa"} {
		if err := SaveManifest(dir, Manifest{Schema: 1, Source: src, QueuedAt: time.Unix(int64(100-i), 0).UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := ListManifests(dir)
	if err != nil || len(all) != 2 || all[0].Source != "aaaaaaaaaaaa" {
		t.Errorf("list = %+v %v", all, err)
	}
	if none, err := ListManifests(filepath.Join(dir, "absent")); err != nil || len(none) != 0 {
		t.Errorf("absent dir: %v %v", none, err)
	}
}

func scanOf(files ...File) Scan { return Scan{Root: "/r", Files: files} }

func TestMergeNewChangedUnchangedGone(t *testing.T) {
	m := Manifest{Source: "s", Entries: []Entry{
		{Rel: "a", Document: "import:s/a", SHA256: "h1", Sent: true},
		{Rel: "b", Document: "import:s/b", SHA256: "h2", Sent: true},
		{Rel: "c", Document: "import:s/c", SHA256: "hc", Sent: true},
	}}
	d := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	res := m.Merge(scanOf(File{Rel: "a", SHA256: "h1"}, File{Rel: "b", SHA256: "h3"}, File{Rel: "d", SHA256: "h4", Date: d}), nil)
	if res != (MergeResult{New: 1, Changed: 1, Unchanged: 1, Gone: 1}) {
		t.Errorf("merge = %+v", res)
	}
	want := []Entry{
		{Rel: "a", Document: "import:s/a", SHA256: "h1", Sent: true},
		{Rel: "b", Document: "import:s/b", SHA256: "h3"},
		{Rel: "c", Document: "import:s/c", SHA256: "hc", Sent: true, Gone: true},
		{Rel: "d", Document: "import:s/d", SHA256: "h4", Date: d},
	}
	if !reflect.DeepEqual(m.Entries, want) {
		t.Errorf("entries =\n%+v\nwant\n%+v", m.Entries, want)
	}
	if u := m.Unsent(); len(u) != 2 || u[0].Rel != "b" || u[1].Rel != "d" {
		t.Errorf("unsent = %+v", u)
	}
}

func TestMergeUsesTombstoneSuccessorForNewFiles(t *testing.T) {
	m := Manifest{Source: "s"}
	m.Merge(scanOf(File{Rel: "a", SHA256: "h"}), []state.Tombstone{{Document: "import:s/a"}})
	if m.Entries[0].Document != "import:s/a#2" {
		t.Errorf("document = %q", m.Entries[0].Document)
	}
}

func TestMergeReappearedFileIsNotGone(t *testing.T) {
	m := Manifest{Source: "s", Entries: []Entry{{Rel: "c", Document: "import:s/c", SHA256: "h", Sent: true, Gone: true}}}
	res := m.Merge(scanOf(File{Rel: "c", SHA256: "h"}), nil)
	if m.Entries[0].Gone || !m.Entries[0].Sent || res.Unchanged != 1 {
		t.Errorf("entry = %+v, merge %+v", m.Entries[0], res)
	}
}

func TestMergeSkipsFilesOfOverlappingSources(t *testing.T) {
	outer := Manifest{Source: "A", Root: "/r/USER", Entries: []Entry{{Rel: "ERRATA/e.md"}, {Rel: "p.md"}}}
	inner := Manifest{Source: "B", Root: "/r/USER/ERRATA", Entries: []Entry{{Rel: "x.jsonl"}}}
	sibling := Manifest{Source: "C", Root: "/r/USERX", Entries: []Entry{{Rel: "q.md"}}}
	all := []Manifest{outer, inner, sibling}
	held := HeldByOthers(all, "B", "/r/USER/ERRATA")
	if !reflect.DeepEqual(held, map[string]bool{filepath.FromSlash("/r/USER/ERRATA/e.md"): true}) {
		t.Errorf("inner root held = %v", held)
	}
	held = HeldByOthers(all, "A", "/r/USER")
	if !reflect.DeepEqual(held, map[string]bool{filepath.FromSlash("/r/USER/ERRATA/x.jsonl"): true}) {
		t.Errorf("outer root held = %v", held)
	}
	if held = HeldByOthers(all, "D", "/r/USE"); len(held) != 0 {
		t.Errorf("a root sharing only a name prefix holds %v", held)
	}
	s := Scan{Files: []File{{Rel: "e.md", Abs: filepath.FromSlash("/r/USER/ERRATA/e.md")}, {Rel: "x.jsonl", Abs: filepath.FromSlash("/r/USER/ERRATA/x.jsonl")}},
		Skipped: map[string][]string{}}
	s.Drop(map[string]bool{filepath.FromSlash("/r/USER/ERRATA/e.md"): true})
	if len(s.Files) != 1 || s.Files[0].Rel != "x.jsonl" || !reflect.DeepEqual(s.Skipped["other_source"], []string{"e.md"}) {
		t.Errorf("after drop: files %+v skipped %v", s.Files, s.Skipped)
	}
}

func TestSeedFromServer(t *testing.T) {
	m := Manifest{Source: "s"}
	s := scanOf(File{Rel: "a.md", SHA256: "ha"}, File{Rel: "b.md", SHA256: "hb"})
	SeedFromServer(&m, s, []hindsight.DocumentItem{
		{ID: "import:s/a.md", Metadata: map[string]string{"path": "a.md", "sha256": "ha"}},
		{ID: "import:s/b.md", Metadata: map[string]string{"path": "b.md", "sha256": "old"}},
		{ID: "import:t/a.md", Metadata: map[string]string{"path": "a.md", "sha256": "ha"}},
		{ID: "conversation:x"},
	})
	byRel := map[string]Entry{}
	for _, e := range m.Entries {
		byRel[e.Rel] = e
	}
	if !byRel["a.md"].Sent || byRel["a.md"].Document != "import:s/a.md" || byRel["a.md"].SHA256 != "ha" {
		t.Errorf("a.md = %+v", byRel["a.md"])
	}
	if b, ok := byRel["b.md"]; !ok || b.Sent || b.Document != "import:s/b.md" {
		t.Errorf("b.md = %+v (a changed file is unsent, same document)", b)
	}
	if len(m.Entries) != 2 {
		t.Errorf("entries = %+v", m.Entries)
	}
}
