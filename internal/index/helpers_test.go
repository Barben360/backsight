package index

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

// ts0 is the time of the synthetic lines; ms0 the same in unix ms.
const ts0 = "2026-10-30T12:00:00.000Z"

var ms0 = mustMS(ts0)

func mustMS(ts string) int64 {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		panic(err)
	}
	return t.UnixMilli()
}

// tline is one telemetry line, newline included: event and ts (left out
// when empty) plus fields, as the writer would emit them.
func tline(event, ts string, fields map[string]any) string {
	m := map[string]any{}
	for k, v := range fields {
		m[k] = v
	}
	if event != "" {
		m["event"] = event
	}
	if ts != "" {
		m["ts"] = ts
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// clockT is a settable clock for Options.Now.
type clockT struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clockT) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clockT) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ingestEnv is a state directory with an index open on it.
type ingestEnv struct {
	t     *testing.T
	d     paths.Dirs
	ix    *Index
	clock *clockT
}

func newEnv(t *testing.T) *ingestEnv {
	t.Helper()
	return envAt(t, tempDirs(t))
}

func envAt(t *testing.T, d paths.Dirs) *ingestEnv {
	t.Helper()
	c := &clockT{t: clock}
	o := opts(t, d, nil)
	o.Now = c.Now
	ix := mustOpen(t, o)
	t.Cleanup(func() { _ = ix.Close() })
	return &ingestEnv{t: t, d: d, ix: ix, clock: c}
}

// telPath is the month file of the telemetry directory.
func (e *ingestEnv) telPath(month string) string {
	return filepath.Join(e.d.TelemetryDir(), "events-"+month+".jsonl")
}

func (e *ingestEnv) write(month, content string) {
	e.t.Helper()
	writeFile(e.t, e.telPath(month), content)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *ingestEnv) append(month, content string) {
	e.t.Helper()
	if err := os.MkdirAll(e.d.TelemetryDir(), 0o700); err != nil {
		e.t.Fatal(err)
	}
	f, err := os.OpenFile(e.telPath(month), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		e.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		e.t.Fatal(err)
	}
}

// ingest runs ingestSources without a deadline.
func (e *ingestEnv) ingest() ingestStats {
	e.t.Helper()
	st, err := e.ix.ingestSources(context.Background(), time.Time{})
	if err != nil {
		e.t.Fatal(err)
	}
	if st.More {
		e.t.Fatal("More without a deadline")
	}
	return st
}

// rowsOf is every row of a query as column → value maps.
func (e *ingestEnv) rowsOf(q string, args ...any) []map[string]any {
	e.t.Helper()
	rows, err := e.ix.sh.r.Query(q, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		e.t.Fatal(err)
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			e.t.Fatal(err)
		}
		m := map[string]any{}
		for i, c := range cols {
			m[c] = vals[i]
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *ingestEnv) count(table string) int {
	e.t.Helper()
	var n int
	if err := e.ix.sh.r.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *ingestEnv) cursor(month string) map[string]any {
	e.t.Helper()
	rs := e.rowsOf(`SELECT * FROM cursors WHERE source_file = ?`, "telemetry/events-"+month+".jsonl")
	if len(rs) != 1 {
		e.t.Fatalf("cursor of %s: %d rows", month, len(rs))
	}
	return rs[0]
}

// dump is every row table, cursors and ingest_errors, each ordered by its
// primary key, without ingested_at (cursors) and at (ingest_errors).
func dump(t *testing.T, ix *Index) string {
	t.Helper()
	tables := append(append([]string{}, rowTables...), "cursors", "ingest_errors")
	var b strings.Builder
	for _, tbl := range tables {
		type col struct {
			name string
			pk   int
		}
		rs, err := ix.sh.r.Query(fmt.Sprintf(`SELECT name, pk FROM pragma_table_info('%s')`, tbl))
		if err != nil {
			t.Fatal(err)
		}
		var cols []col
		for rs.Next() {
			var c col
			if err := rs.Scan(&c.name, &c.pk); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, c)
		}
		_ = rs.Close()
		var sel, order []string
		pks := []col{}
		for _, c := range cols {
			if (tbl == "cursors" && c.name == "ingested_at") || (tbl == "ingest_errors" && c.name == "at") {
				continue
			}
			sel = append(sel, `"`+c.name+`"`)
			if c.pk > 0 {
				pks = append(pks, c)
			}
		}
		sort.Slice(pks, func(i, j int) bool { return pks[i].pk < pks[j].pk })
		for _, c := range pks {
			order = append(order, `"`+c.name+`"`)
		}
		q := fmt.Sprintf("SELECT %s FROM %s ORDER BY %s", strings.Join(sel, ","), tbl, strings.Join(order, ","))
		rows, err := ix.sh.r.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for rows.Next() {
			vals := make([]any, len(sel))
			ptrs := make([]any, len(sel))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s:", tbl)
			for i, v := range vals {
				fmt.Fprintf(&b, " %s=%v", sel[i], v)
			}
			b.WriteByte('\n')
		}
		_ = rows.Close()
	}
	return b.String()
}

// freshDump is the dump of a new index built over a copy of e's
// telemetry files.
func freshDump(t *testing.T, e *ingestEnv) string {
	t.Helper()
	d := tempDirs(t)
	ents, err := os.ReadDir(e.d.TelemetryDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		b, err := os.ReadFile(filepath.Join(e.d.TelemetryDir(), ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(d.TelemetryDir(), ent.Name()), string(b))
	}
	f := envAt(t, d)
	f.ingest()
	return dump(t, f.ix)
}

// dbBytes is the index file and its -wal, as they are on disk.
func dbBytes(t *testing.T, ix *Index) []byte {
	t.Helper()
	var out []byte
	for _, p := range []string{ix.Path(), ix.Path() + "-wal"} {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	return out
}

// textAnywhere reports whether any TEXT value of any table contains s.
func textAnywhere(t *testing.T, ix *Index, s string) []string {
	t.Helper()
	var hits []string
	tables := []string{}
	rs, err := ix.sh.r.Query(`SELECT name FROM sqlite_schema WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var n string
		if err := rs.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	_ = rs.Close()
	for _, tbl := range tables {
		rows, err := ix.sh.r.Query("SELECT * FROM " + tbl)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				var str string
				switch x := v.(type) {
				case string:
					str = x
				case []byte:
					str = string(x)
				default:
					continue
				}
				if strings.Contains(str, s) {
					hits = append(hits, tbl+"."+cols[i]+"="+str)
				}
			}
		}
		_ = rows.Close()
	}
	return hits
}

// noBytes fails when s appears in the index file or its -wal.
func noBytes(t *testing.T, ix *Index, s string) {
	t.Helper()
	if bytes.Contains(dbBytes(t, ix), []byte(s)) {
		t.Errorf("%q found in the index file bytes", s)
	}
}

// sameValue compares a scanned value with a wanted Go value: ints as
// int64, nil as NULL.
func sameValue(got, want any) bool {
	switch w := want.(type) {
	case int:
		want = int64(w)
	case bool:
		if w {
			want = int64(1)
		} else {
			want = int64(0)
		}
	}
	if b, ok := got.([]byte); ok {
		got = string(b)
	}
	return fmt.Sprintf("%T %v", got, got) == fmt.Sprintf("%T %v", want, want)
}
