package index

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// realSlices is how many growing prefixes of each source file
// TestRealStateIncrementalEqualsRebuild ingests.
const realSlices = 10

// TestRealStateIncrementalEqualsRebuild ingests a real state directory's
// month files in growing prefixes cut at arbitrary bytes (so most slices
// end on a partial line), then compares the index with a fresh build of
// the whole files. It runs only when BACKSIGHT_INDEX_REAL names a state
// directory, which it reads and never writes: point it at a copy.
// On a difference it prints row counts per table, never row values.
func TestRealStateIncrementalEqualsRebuild(t *testing.T) {
	src := os.Getenv("BACKSIGHT_INDEX_REAL")
	if src == "" {
		t.Skip("BACKSIGHT_INDEX_REAL is not set")
	}
	files := map[string][]byte{} // path relative to the state directory → content
	for _, sub := range []string{"telemetry", "feedback"} {
		ents, err := os.ReadDir(filepath.Join(src, sub))
		if err != nil {
			continue
		}
		for _, ent := range ents {
			name := ent.Name()
			month, ok := strings.CutPrefix(name, map[string]string{"telemetry": "events-", "feedback": "journal-"}[sub])
			if !ok || !ent.Type().IsRegular() || !monthRE.MatchString(month) {
				continue
			}
			b, err := os.ReadFile(filepath.Join(src, sub, name))
			if err != nil {
				t.Fatal(err)
			}
			files[filepath.Join(sub, name)] = b
		}
	}
	if len(files) == 0 {
		t.Fatalf("no month file under %s/telemetry or %s/feedback", src, src)
	}
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	e := newEnv(t)
	var lines int64
	for i := 1; i <= realSlices; i++ {
		for _, rel := range rels {
			b := files[rel]
			writeFile(t, filepath.Join(e.d.State, rel), string(b[:len(b)*i/realSlices]))
		}
		lines += e.ingest().Lines
	}

	d := tempDirs(t)
	for _, rel := range rels {
		writeFile(t, filepath.Join(d.State, rel), string(files[rel]))
	}
	f := envAt(t, d)
	full := f.ingest().Lines

	got, want := dump(t, e.ix), dump(t, f.ix)
	if lines != full {
		t.Errorf("the slices read %d lines, a full build %d", lines, full)
	}
	if got != want {
		t.Errorf("incremental differs from a full build: rows per table, incremental %v, full %v; first difference in table %q",
			perTable(got), perTable(want), firstDiffTable(got, want))
	}
	rows := perTable(want)
	if full == 0 || rows["cursors"] != len(rels) {
		t.Errorf("%d lines, %d cursors for %d files", full, rows["cursors"], len(rels))
	}
	t.Logf("%d files, %d lines, rows per table %v", len(rels), full, rows)
}

// perTable counts a dump's lines per table.
func perTable(dump string) map[string]int {
	n := map[string]int{}
	for _, l := range strings.Split(strings.TrimSuffix(dump, "\n"), "\n") {
		if tbl, _, ok := strings.Cut(l, ":"); ok {
			n[tbl]++
		}
	}
	return n
}

// firstDiffTable is the table of the first line where two dumps differ.
func firstDiffTable(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			if x == "" {
				x = y
			}
			tbl, _, _ := strings.Cut(x, ":")
			return tbl
		}
	}
	return ""
}
