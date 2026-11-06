package index

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/machines"
)

const (
	pcA = "3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6"
	pcB = "b0b0b0b0-1111-4222-8333-444455556666"
)

// pcFigures are the figures of a document's first day; the second day is
// the same with every count doubled-plus-one, so rows tell the days apart.
var pcFigures = machines.Figures{
	Prompts: 41, Hit: machines.Pair{12, 30}, Used: machines.Pair{5, 9}, Coverage: machines.Pair{9, 11},
	Wrong: machines.Pair{0, 6}, Context: machines.Pair{72000, 4}, Latency: machines.Latency{0, 2, 10, 9, 5, 3, 1, 0, 0},
}

// pcDoc is a valid document of machine id at 10:00 on the test day (two
// hours before the clock) with two days, the first with two versions.
func pcDoc(id, os string) machines.Doc {
	half := machines.Figures{Prompts: 20, Hit: machines.Pair{6, 15}, Used: machines.Pair{2, 4}, Coverage: machines.Pair{4, 5},
		Wrong: machines.Pair{0, 3}, Context: machines.Pair{30000, 2}, Latency: machines.Latency{0, 1, 5, 4, 2, 1, 0, 0, 0}}
	second := machines.Figures{Prompts: 7, Hit: machines.Pair{1, 3}, Used: machines.Pair{1, 2}, Coverage: machines.Pair{2, 2},
		Wrong: machines.Pair{1, 2}, Context: machines.Pair{900, 1}, Latency: machines.Latency{1, 0, 0, 0, 0, 0, 0, 0, 2}}
	return machines.Doc{
		Machine: id, At: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC), OS: os, FirstSeen: "2026-09-02",
		BinaryVersion: "v0.1.30-m7", Until: "2026-10-29",
		Days: []machines.Day{
			{Day: "2026-10-28", Figures: pcFigures, Flow: machines.Pair{3, 4},
				Versions: map[string]machines.Figures{"v0.1.30-m7": half, "v0.1.29": half}},
			{Day: "2026-10-29", Figures: second, Flow: machines.Pair{2, 2}},
		},
	}
}

func cachedDoc(t *testing.T, d machines.Doc) machines.CachedDoc {
	t.Helper()
	raw, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return machines.CachedDoc{Name: machines.DocPrefix + d.Machine, Raw: raw}
}

// machEnv is an index whose Options.Machines reads the cache file under
// the state directory and counts its calls.
func machEnv(t *testing.T) (*ingestEnv, *int) {
	t.Helper()
	calls := new(int)
	e := envWith(t, func(o *Options) {
		dir := o.Dirs.MetricsDir()
		o.Machines = func() (machines.Cache, error) {
			*calls++
			return machines.LoadCache(dir)
		}
	})
	return e, calls
}

var cacheWrites int

// putCache writes the cache file with a modification time no earlier
// write had, so that a rewrite is never mistaken for the same file.
func putCache(t *testing.T, e *ingestEnv, c machines.Cache) {
	t.Helper()
	if err := os.MkdirAll(e.d.MetricsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := machines.SaveCache(e.d.MetricsDir(), c); err != nil {
		t.Fatal(err)
	}
	cacheWrites++
	at := time.Date(2026, 10, 30, 11, 0, 0, 0, time.UTC).Add(time.Duration(cacheWrites) * time.Second)
	if err := os.Chtimes(machines.CacheFile(e.d.MetricsDir()), at, at); err != nil {
		t.Fatal(err)
	}
}

var fetchedAt = time.Date(2026, 10, 30, 11, 30, 0, 0, time.UTC)

func twoPCs(t *testing.T) machines.Cache {
	t.Helper()
	return machines.Cache{FetchedAt: fetchedAt, Docs: []machines.CachedDoc{
		cachedDoc(t, pcDoc(pcA, "linux")), cachedDoc(t, pcDoc(pcB, "windows"))}}
}

func TestMachinesCacheBecomesRows(t *testing.T) {
	e, _ := machEnv(t)
	c := twoPCs(t)
	c.Ignored = []machines.Ignored{{Machine: "deadbeef", Reason: machines.TooLarge}, {Machine: "", Reason: machines.UnknownVersion}}
	putCache(t, e, c)
	e.catchUp(time.Minute)

	docs := e.rowsOf(`SELECT * FROM pc_docs ORDER BY machine`)
	if len(docs) != 2 {
		t.Fatalf("pc_docs: %v", docs)
	}
	checkRow(t, "pc_docs A", docs[0], map[string]any{"machine": pcA, "os": "linux", "first_seen": "2026-09-02",
		"at": time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC).UnixMilli(), "binary_version": "v0.1.30-m7", "until_day": "2026-10-29"})
	checkRow(t, "pc_docs B", docs[1], map[string]any{"machine": pcB, "os": "windows"})

	// A: day one (totals + two versions), day two (totals); B the same.
	if n := e.count("pc_days"); n != 8 {
		t.Fatalf("pc_days has %d rows, want 8", n)
	}
	noon1 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC).UnixMilli()
	rows := e.rowsOf(`SELECT * FROM pc_days WHERE machine = ? AND day = '2026-10-28' AND version = ''`, pcA)
	if len(rows) != 1 {
		t.Fatalf("day totals row: %v", rows)
	}
	checkRow(t, "totals", rows[0], map[string]any{"noon": noon1, "prompts": 41, "hit_n": 12, "hit_d": 30, "used_n": 5, "used_d": 9,
		"cov_n": 9, "cov_d": 11, "wrong_n": 0, "wrong_d": 6, "ctx_chars": 72000, "ctx_sessions": 4, "flow_n": 3, "flow_d": 4,
		"lat0": 0, "lat1": 2, "lat2": 10, "lat3": 9, "lat4": 5, "lat5": 3, "lat6": 1, "lat7": 0, "lat8": 0})
	rows = e.rowsOf(`SELECT * FROM pc_days WHERE machine = ? AND day = '2026-10-28' AND version = 'v0.1.29'`, pcA)
	if len(rows) != 1 {
		t.Fatalf("version row: %v", rows)
	}
	checkRow(t, "version", rows[0], map[string]any{"noon": noon1, "prompts": 20, "hit_n": 6, "hit_d": 15, "used_n": 2, "used_d": 4,
		"cov_n": 4, "cov_d": 5, "wrong_n": 0, "wrong_d": 3, "ctx_chars": 30000, "ctx_sessions": 2,
		"lat0": 0, "lat1": 1, "lat2": 5, "lat3": 4, "lat4": 2, "lat5": 1, "lat6": 0, "lat7": 0, "lat8": 0})
	if rows[0]["flow_n"] != nil || rows[0]["flow_d"] != nil {
		t.Errorf("a version row has flow %v/%v, want NULL", rows[0]["flow_n"], rows[0]["flow_d"])
	}
	rows = e.rowsOf(`SELECT * FROM pc_days WHERE machine = ? AND day = '2026-10-29'`, pcB)
	if len(rows) != 1 || rows[0]["version"] != "" {
		t.Fatalf("second day: %v", rows)
	}
	checkRow(t, "second day", rows[0], map[string]any{"noon": time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC).UnixMilli(),
		"prompts": 7, "flow_n": 2, "flow_d": 2, "lat0": 1, "lat8": 2})

	ign := e.rowsOf(`SELECT pos, machine, reason FROM pc_ignored ORDER BY pos`)
	if len(ign) != 2 {
		t.Fatalf("pc_ignored: %v", ign)
	}
	checkRow(t, "ignored 0", ign[0], map[string]any{"pos": 0, "machine": "deadbeef", "reason": "too_large"})
	checkRow(t, "ignored 1", ign[1], map[string]any{"pos": 1, "machine": "", "reason": "unknown_version"})

	fi, err := os.Stat(machines.CacheFile(e.d.MetricsDir()))
	if err != nil {
		t.Fatal(err)
	}
	f := e.rowsOf(`SELECT * FROM pc_fetch`)
	if len(f) != 1 {
		t.Fatalf("pc_fetch: %v", f)
	}
	checkRow(t, "pc_fetch", f[0], map[string]any{"id": 1, "fetched_at": fetchedAt.UnixMilli(), "size": fi.Size(),
		"mtime": fi.ModTime().UnixNano(), "ingested_at": clock.UnixMilli()})
	if s, _ := f[0]["sha256"].(string); len(s) != 64 {
		t.Errorf("sha256 %q", f[0]["sha256"])
	}
}

// A document is parsed again at the index's clock: one dated beyond
// MaxSkew ahead of it is an ignored row, not a document.
func TestMachinesCacheParsesAtTheIndexClock(t *testing.T) {
	e, _ := machEnv(t)
	future := pcDoc(pcB, "linux")
	future.At = clock.Add(machines.MaxSkew + time.Hour)
	c := machines.Cache{FetchedAt: fetchedAt, Docs: []machines.CachedDoc{cachedDoc(t, pcDoc(pcA, "linux")), cachedDoc(t, future)}}
	putCache(t, e, c)
	e.catchUp(time.Minute)
	if rs := e.rowsOf(`SELECT machine FROM pc_docs`); len(rs) != 1 || rs[0]["machine"] != pcA {
		t.Errorf("pc_docs %v, want only %s", rs, pcA)
	}
	ign := e.rowsOf(`SELECT machine, reason FROM pc_ignored`)
	if len(ign) != 1 || ign[0]["machine"] != pcB[:8] || ign[0]["reason"] != "not_valid" {
		t.Errorf("pc_ignored %v", ign)
	}
}

func TestSameCacheTakesNoWriteLock(t *testing.T) {
	e, _ := machEnv(t)
	putCache(t, e, twoPCs(t))
	n := countWriteTxs(t)
	e.catchUp(time.Minute)
	if *n == 0 || e.count("pc_docs") != 2 {
		t.Fatalf("first catch-up: %d write transactions, %d documents", *n, e.count("pc_docs"))
	}
	*n = 0
	e.clock.Add(time.Hour)
	e.catchUp(time.Minute)
	if *n != 0 {
		t.Errorf("%d write transactions for the same cache file, want 0", *n)
	}
	var changes int64
	if err := e.ix.sh.w.QueryRow(`SELECT total_changes()`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(time.Hour)
	e.catchUp(time.Minute)
	var after int64
	if err := e.ix.sh.w.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != changes {
		t.Errorf("the write connection changed %d rows for the same cache", after-changes)
	}
}

// The same size and modification time: the closure (the file) is not
// called. A touched file with the same content is read but its rows are
// not replaced (ingested_at stays); a new content replaces them.
func TestUnchangedCacheIsNotRead(t *testing.T) {
	e, calls := machEnv(t)
	putCache(t, e, twoPCs(t))
	e.catchUp(time.Minute)
	if *calls != 1 {
		t.Fatalf("%d reads on the first catch-up, want 1", *calls)
	}
	e.clock.Add(time.Minute)
	e.catchUp(time.Minute)
	e.catchUp(time.Minute)
	if *calls != 1 {
		t.Errorf("%d reads, want 1: the unchanged file was read again", *calls)
	}

	e.clock.Add(time.Minute)
	putCache(t, e, twoPCs(t)) // same bytes, new modification time
	e.catchUp(time.Minute)
	if *calls != 2 {
		t.Errorf("%d reads after a touch, want 2", *calls)
	}
	f := e.rowsOf(`SELECT mtime, ingested_at FROM pc_fetch`)
	fi, err := os.Stat(machines.CacheFile(e.d.MetricsDir()))
	if err != nil {
		t.Fatal(err)
	}
	if f[0]["mtime"] != fi.ModTime().UnixNano() || f[0]["ingested_at"] != clock.UnixMilli() {
		t.Errorf("a touched file with the same content: pc_fetch %v, want the new mtime and the old ingested_at", f[0])
	}
	e.catchUp(time.Minute)
	if *calls != 2 {
		t.Errorf("%d reads, the new modification time was not remembered", *calls)
	}

	c := twoPCs(t)
	c.FetchedAt = fetchedAt.Add(time.Hour)
	putCache(t, e, c)
	e.catchUp(time.Minute)
	f = e.rowsOf(`SELECT fetched_at, ingested_at FROM pc_fetch`)
	if f[0]["fetched_at"] != c.FetchedAt.UnixMilli() || f[0]["ingested_at"] != clock.Add(2*time.Minute).UnixMilli() {
		t.Errorf("a new fetch: pc_fetch %v", f[0])
	}
}

func TestNewCacheReplacesTheRows(t *testing.T) {
	e, _ := machEnv(t)
	putCache(t, e, twoPCs(t))
	e.catchUp(time.Minute)
	if e.count("pc_docs") != 2 || e.count("pc_days") != 8 {
		t.Fatalf("setup: %d documents, %d days", e.count("pc_docs"), e.count("pc_days"))
	}
	c := machines.Cache{FetchedAt: fetchedAt.Add(time.Hour), Docs: []machines.CachedDoc{cachedDoc(t, pcDoc(pcB, "windows"))},
		Ignored: []machines.Ignored{{Machine: "cafebabe", Reason: machines.NotValid}}}
	putCache(t, e, c)
	n := countWriteTxs(t)
	e.catchUp(time.Minute)
	if *n != 1 {
		t.Errorf("%d write transactions to replace the rows, want 1", *n)
	}
	if rs := e.rowsOf(`SELECT machine FROM pc_docs`); len(rs) != 1 || rs[0]["machine"] != pcB {
		t.Errorf("pc_docs %v, want only %s", rs, pcB)
	}
	if rs := e.rowsOf(`SELECT DISTINCT machine FROM pc_days`); len(rs) != 1 || rs[0]["machine"] != pcB {
		t.Errorf("pc_days of %v, want only %s", rs, pcB)
	}
	if e.count("pc_days") != 4 || e.count("pc_ignored") != 1 {
		t.Errorf("%d days and %d ignored, want 4 and 1", e.count("pc_days"), e.count("pc_ignored"))
	}
}

func TestMissingCacheIsEmpty(t *testing.T) {
	e, calls := machEnv(t)
	e.catchUp(time.Minute)
	if *calls != 0 {
		t.Errorf("%d reads of a file that is not there", *calls)
	}
	for _, tbl := range []string{"pc_fetch", "pc_docs", "pc_days", "pc_ignored", "ingest_errors"} {
		if n := e.count(tbl); n != 0 {
			t.Errorf("%s: %d rows for a missing cache", tbl, n)
		}
	}
	putCache(t, e, twoPCs(t))
	e.catchUp(time.Minute)
	if e.count("pc_docs") != 2 {
		t.Fatalf("setup: %d documents", e.count("pc_docs"))
	}
	if err := os.Remove(machines.CacheFile(e.d.MetricsDir())); err != nil {
		t.Fatal(err)
	}
	e.catchUp(time.Minute)
	for _, tbl := range []string{"pc_fetch", "pc_docs", "pc_days", "pc_ignored", "ingest_errors"} {
		if n := e.count(tbl); n != 0 {
			t.Errorf("%s: %d rows after the file was removed", tbl, n)
		}
	}
	n := countWriteTxs(t)
	e.catchUp(time.Minute)
	if *n != 0 {
		t.Errorf("%d write transactions for a cache still missing", *n)
	}
}

func TestUnreadableCacheKeepsTheRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  func(t *testing.T, path string)
	}{
		{"truncated", func(t *testing.T, path string) { writeFile(t, path, `{"fetched_at":"2026-10-30T11:`) }},
		{"over the cap", func(t *testing.T, path string) {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			if err := f.Truncate(machines.MaxCacheBytes + 1); err != nil { // sparse
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := machEnv(t)
			putCache(t, e, twoPCs(t))
			e.catchUp(time.Minute)
			path := machines.CacheFile(e.d.MetricsDir())

			tc.bad(t, path)
			e.catchUp(time.Minute)
			e.catchUp(time.Minute)
			if e.count("pc_docs") != 2 || e.count("pc_days") != 8 {
				t.Errorf("rows lost: %d documents, %d days", e.count("pc_docs"), e.count("pc_days"))
			}
			errs := e.errorsOf("machines_unreadable")
			if len(errs) != 1 {
				t.Fatalf("machines_unreadable rows: %v", errs)
			}
			checkRow(t, "error", errs[0], map[string]any{"source_file": "metrics/machines.json", "offset": 0})
			if n := e.count("ingest_errors"); n != 1 {
				t.Errorf("%d ingest errors, want 1", n)
			}

			// The record is not pruned with the file gone: the next read
			// is what clears it.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			e.clock.Add(time.Hour)
			res := e.maintain()
			if res.ErrorsPruned != 0 || len(e.errorsOf("machines_unreadable")) != 1 {
				t.Errorf("Maintain pruned the machines_unreadable record (%d pruned)", res.ErrorsPruned)
			}

			// A good file clears it and replaces the rows.
			c := machines.Cache{FetchedAt: fetchedAt, Docs: []machines.CachedDoc{cachedDoc(t, pcDoc(pcA, "darwin"))}}
			putCache(t, e, c)
			e.catchUp(time.Minute)
			if errs := e.errorsOf("machines_unreadable"); len(errs) != 0 {
				t.Errorf("the record stays after a good file: %v", errs)
			}
			if rs := e.rowsOf(`SELECT machine, os FROM pc_docs`); len(rs) != 1 || rs[0]["os"] != "darwin" {
				t.Errorf("pc_docs %v", rs)
			}
		})
	}
}

func TestDuplicateNamesKeepTheFirst(t *testing.T) {
	e, _ := machEnv(t)
	first, second := cachedDoc(t, pcDoc(pcA, "linux")), cachedDoc(t, pcDoc(pcA, "darwin"))
	putCache(t, e, machines.Cache{FetchedAt: fetchedAt, Docs: []machines.CachedDoc{first, cachedDoc(t, pcDoc(pcB, "windows")), second},
		Ignored: []machines.Ignored{{Machine: "deadbeef", Reason: machines.TooLarge}}})
	e.catchUp(time.Minute)
	docs := e.rowsOf(`SELECT machine, os FROM pc_docs ORDER BY machine`)
	if len(docs) != 2 || docs[0]["machine"] != pcA || docs[0]["os"] != "linux" {
		t.Errorf("pc_docs %v, want %s from the first document (linux)", docs, pcA)
	}
	if n := e.count("pc_days"); n != 8 {
		t.Errorf("%d day rows, want 8 (the second document adds none)", n)
	}
	ign := e.rowsOf(`SELECT machine, reason FROM pc_ignored ORDER BY pos`)
	if len(ign) != 2 {
		t.Fatalf("pc_ignored %v", ign)
	}
	checkRow(t, "cached ignored first", ign[0], map[string]any{"machine": "deadbeef", "reason": "too_large"})
	checkRow(t, "duplicate", ign[1], map[string]any{"machine": pcA[:8], "reason": "not_valid"})
	if n := e.count("ingest_errors"); n != 0 {
		t.Errorf("%d ingest errors: a duplicate must not fail the catch-up", n)
	}
}

// Canary strings in an unknown field, a version key, an oversize document,
// a document name and the ignored list never reach the index file, its
// WAL or (after a VACUUM) the pages it freed.
func TestHostileCacheNeverReachesTheTables(t *testing.T) {
	const (
		cUnknown = "CANARY-UNKNOWN-FIELD-31f"
		cScript  = "CANARY-SCRIPT-VERSION-8c2"
		cBig     = "CANARY-OVERSIZE-DOC-5d9"
		cName    = "CANARY-DOC-NAME-a07"
		cIgnored = "CANARY-IGNORED-ENTRY-e44"
	)
	e, _ := machEnv(t)

	good, err := pcDoc(pcA, "linux").Marshal()
	if err != nil {
		t.Fatal(err)
	}
	withUnknown := strings.Replace(good, `{"v":1,`, `{"v":1,"`+cUnknown+`":{"deep":"`+cUnknown+`"},`, 1)
	if withUnknown == good {
		t.Fatal("the unknown field was not added")
	}
	scripty := pcDoc(pcB, "linux")
	raw, err := scripty.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	scriptRaw := strings.Replace(raw, `"v0.1.29":{`, `"<script>`+cScript+`</script>":{`, 1)
	if scriptRaw == raw {
		t.Fatal("the script version was not added")
	}
	big := `{"v":1,"x":"` + strings.Repeat("a", machines.MaxBytes) + cBig + `"}` // over the 1 MiB cap
	const pcC = "c0c0c0c0-1111-4222-8333-444455556666"
	putCache(t, e, machines.Cache{FetchedAt: fetchedAt,
		Docs: []machines.CachedDoc{
			{Name: machines.DocPrefix + pcA, Raw: withUnknown},
			{Name: machines.DocPrefix + pcB, Raw: scriptRaw},
			{Name: machines.DocPrefix + pcC, Raw: big},
			{Name: machines.DocPrefix + cName, Raw: good},
		},
		Ignored: []machines.Ignored{{Machine: cIgnored, Reason: machines.Reason(cIgnored)}}})
	e.catchUp(time.Minute)

	// The unknown field is ignored, so its document is a good one; the
	// other three are dropped whole with their reasons.
	if rs := e.rowsOf(`SELECT machine FROM pc_docs`); len(rs) != 1 || rs[0]["machine"] != pcA {
		t.Errorf("pc_docs %v, want only %s", rs, pcA)
	}
	ign := e.rowsOf(`SELECT machine, reason FROM pc_ignored ORDER BY pos`)
	want := []map[string]any{
		{"machine": "", "reason": "not_valid"},      // the cached entry, scrubbed
		{"machine": pcB[:8], "reason": "not_valid"}, // the <script> version
		{"machine": pcC[:8], "reason": "too_large"}, // over the cap
		{"machine": "", "reason": "not_valid"},      // a name that holds no machine id
	}
	if len(ign) != len(want) {
		t.Fatalf("pc_ignored %v, want %v", ign, want)
	}
	for i := range want {
		checkRow(t, fmt.Sprintf("ignored %d", i), ign[i], want[i])
	}

	grep := func(when string) {
		t.Helper()
		if err := e.ix.Checkpoint(context.Background()); err != nil {
			t.Fatal(err)
		}
		var all []byte
		for _, p := range []string{e.ix.Path(), e.ix.Path() + "-wal"} {
			b, err := os.ReadFile(p)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			all = append(all, b...)
		}
		if !bytes.Contains(all, []byte(pcA)) {
			t.Fatalf("%s: the machine id of the good document is not in the file: the check reads nothing", when)
		}
		for _, c := range []string{cUnknown, cScript, cBig, cName, cIgnored, "<script>"} {
			if bytes.Contains(all, []byte(c)) {
				t.Errorf("%s: %q in the index bytes", when, c)
			}
		}
	}
	grep("after the catch-up")
	// The canaries are in the cache file: the check can see them there.
	cf, err := os.ReadFile(machines.CacheFile(e.d.MetricsDir()))
	if err != nil || !bytes.Contains(cf, []byte(cUnknown)) || !bytes.Contains(cf, []byte(cBig)) {
		t.Fatalf("the cache file does not hold the canaries (%v)", err)
	}
	if _, err := e.ix.sh.w.Exec(`VACUUM`); err != nil {
		t.Fatal(err)
	}
	grep("after VACUUM")
}

// Sharing off (Options.MachinesOff) reads as an empty cache without
// looking at the file: the rows go, and come back when it is on again.
func TestMachinesOffEmptiesTheRows(t *testing.T) {
	off := false
	calls := new(int)
	e := envWith(t, func(o *Options) {
		dir := o.Dirs.MetricsDir()
		o.MachinesOff = func() bool { return off }
		o.Machines = func() (machines.Cache, error) { *calls++; return machines.LoadCache(dir) }
	})
	putCache(t, e, twoPCs(t))
	e.catchUp(time.Minute)
	if e.count("pc_docs") != 2 {
		t.Fatalf("setup: %d documents", e.count("pc_docs"))
	}
	off = true
	before := *calls
	e.catchUp(time.Minute)
	for _, tbl := range []string{"pc_fetch", "pc_docs", "pc_days", "pc_ignored"} {
		if n := e.count(tbl); n != 0 {
			t.Errorf("%s: %d rows with sharing off", tbl, n)
		}
	}
	if *calls != before {
		t.Errorf("the file was read with sharing off")
	}
	if _, err := os.Stat(machines.CacheFile(e.d.MetricsDir())); err != nil {
		t.Errorf("the cache file must stay on disk: %v", err)
	}
	off = false
	e.catchUp(time.Minute)
	if e.count("pc_docs") != 2 || e.count("pc_days") != 8 {
		t.Errorf("rows did not come back: %d documents, %d days", e.count("pc_docs"), e.count("pc_days"))
	}
}

func TestMachinesNotRefreshedWithoutASource(t *testing.T) {
	e := newEnv(t)
	putCache(t, e, twoPCs(t))
	e.catchUp(time.Minute)
	if n := e.count("pc_docs") + e.count("pc_fetch"); n != 0 {
		t.Errorf("%d rows without Options.Machines", n)
	}
}

// machinesDump is the four tables, without the times of the ingest and of
// the file.
func machinesDump(t *testing.T, e *ingestEnv) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, fetched_at, size, sha256 FROM pc_fetch`,
		`SELECT * FROM pc_docs ORDER BY machine`,
		`SELECT * FROM pc_days ORDER BY machine, day, version`,
		`SELECT * FROM pc_ignored ORDER BY pos`,
	} {
		for _, r := range e.rowsOf(q) {
			fmt.Fprintf(&b, "%v\n", r)
		}
	}
	return b.String()
}

// TestIncrementalEqualsRebuildWithMachines: caches that come and go over
// successive catch-ups leave what a new index reads from the last one.
func TestIncrementalEqualsRebuildWithMachines(t *testing.T) {
	src := tempDirs(t)
	gen := func(n int, seed uint64) machines.Cache {
		indexgen.Generate(t, src.State, indexgen.Spec{Months: 2, TelemetryPerYear: 12, JournalPerYear: 12, Memories: 3, Seed: seed, OtherPCs: n})
		c, err := machines.LoadCache(src.MetricsDir())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	e, _ := machEnv(t)
	for _, c := range []machines.Cache{gen(3, 1), gen(5, 2), gen(2, 3)} {
		putCache(t, e, c)
		e.catchUp(time.Minute)
	}
	if err := os.Remove(machines.CacheFile(e.d.MetricsDir())); err != nil {
		t.Fatal(err)
	}
	e.catchUp(time.Minute)
	if n := e.count("pc_docs"); n != 0 {
		t.Fatalf("%d documents after the file went", n)
	}
	final := gen(4, 4)
	putCache(t, e, final)
	e.catchUp(time.Minute)

	f, _ := machEnv(t)
	putCache(t, f, final)
	f.catchUp(time.Minute)
	if f.count("pc_docs") < 3 || f.count("pc_days") < 100 || f.count("pc_ignored") != 1 {
		t.Fatalf("the generated cache filled %d documents, %d days, %d ignored", f.count("pc_docs"), f.count("pc_days"), f.count("pc_ignored"))
	}
	if got, want := machinesDump(t, e), machinesDump(t, f); got != want {
		t.Errorf("incremental differs from a rebuild (%d and %d bytes of dump)", len(got), len(want))
	}
}

// The generated cache files sit where the cli reads them.
func TestMachinesSourceIsTheCacheFile(t *testing.T) {
	if machinesSource != "metrics/machines.json" {
		t.Errorf("machinesSource %q", machinesSource)
	}
	d := tempDirs(t)
	rel, err := filepath.Rel(d.State, machines.CacheFile(d.MetricsDir()))
	if err != nil || filepath.ToSlash(rel) != machinesSource {
		t.Errorf("the cache file is %q under the state directory, machinesSource %q", rel, machinesSource)
	}
}
