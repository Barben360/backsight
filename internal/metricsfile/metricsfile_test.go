package metricsfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func writerAt(t *testing.T, ts time.Time) (*Writer, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "metrics")
	w := New(dir, "0.1.test")
	w.Now = func() time.Time { return ts }
	return w, dir
}

func lines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		m := map[string]any{}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("unparseable line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func TestAppendWritesCommonFields(t *testing.T) {
	ts := time.Date(2026, 10, 30, 15, 44, 0, 0, time.UTC)
	w, dir := writerAt(t, ts)
	if err := w.Append(Scan{Count: 10, Live: 8, Forgotten: 2, Pages: 3, MS: 450}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "scan-history-2026-10.jsonl")
	ls := lines(t, path)
	if len(ls) != 1 {
		t.Fatalf("%d lines", len(ls))
	}
	m := ls[0]
	want := map[string]any{"schema": 1.0, "ts": "2026-10-30T15:44:00.000Z", "kind": "scan", "v": w.V, "binary_version": "0.1.test",
		"count": 10.0, "live": 8.0, "forgotten": 2.0, "pages": 3.0, "ms": 450.0}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("line = %v, want %v", m, want)
	}
	if w.V == "" {
		t.Fatal("v is empty")
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %v, %v", st.Mode(), err)
		}
		sd, _ := os.Stat(dir)
		if sd.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %v", sd.Mode().Perm())
		}
	}
}

func TestAppendLinesOfACallShareTS(t *testing.T) {
	n := 0
	w, dir := writerAt(t, time.Time{})
	w.Now = func() time.Time { n++; return time.Date(2026, 10, 30, 0, 0, n, 0, time.UTC) }
	if err := w.Append(DoctorRun{Checks: map[string]string{"a": "ok"}}, Scan{}, Scan{}); err != nil {
		t.Fatal(err)
	}
	a := lines(t, filepath.Join(dir, "doctor-history-2026-10.jsonl"))
	b := lines(t, filepath.Join(dir, "scan-history-2026-10.jsonl"))
	if len(a) != 1 || len(b) != 2 || a[0]["ts"] != b[0]["ts"] || b[0]["ts"] != b[1]["ts"] {
		t.Fatalf("ts differ: %v %v", a, b)
	}
}

func TestMonthFollowsUTC(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	w, dir := writerAt(t, time.Date(2026, 11, 1, 0, 30, 0, 0, cet))
	if err := w.Append(Scan{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scan-history-2026-10.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scan-history-2026-11.jsonl")); err == nil {
		t.Fatal("wrote the local month")
	}
	if got := lines(t, filepath.Join(dir, "scan-history-2026-10.jsonl"))[0]["ts"]; got != "2026-10-31T23:30:00.000Z" {
		t.Fatalf("ts = %v", got)
	}
}

func TestTimesInRecordsAreUTC(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	w, dir := writerAt(t, time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	c := time.Date(2026, 10, 30, 9, 0, 0, 0, cet)
	if err := w.Append(DocOp{Operation: "op1", Document: "conversation:abc", Session: "abc", Status: "completed", CreatedAt: c, UpdatedAt: c}); err != nil {
		t.Fatal(err)
	}
	got := lines(t, filepath.Join(dir, "memory-docs-2026-10.jsonl"))[0]
	if got["created_at"] != "2026-10-30T08:00:00Z" {
		t.Fatalf("created_at = %v", got["created_at"])
	}
}

func TestAppendIsOneWritePerLine(t *testing.T) {
	w1, dir := writerAt(t, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	w2 := New(dir, "0.1.test")
	w2.Now = w1.Now
	// A long line makes a torn interleaving visible.
	ops := map[string]int64{}
	for i := 0; i < 40; i++ {
		ops[strings.Repeat("k", 20)+string(rune('a'+i%26))+string(rune('a'+i/26))] = int64(i)
	}
	var wg sync.WaitGroup
	for _, w := range []*Writer{w1, w2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if err := w.Append(Snapshot{Day: "2026-10-30", OK: true, Ops: ops}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := len(lines(t, filepath.Join(dir, "server-daily-2026-10.jsonl"))); got != 1000 {
		t.Fatalf("%d lines, want 1000", got)
	}
}

func TestNilAndAbsentAreOmitted(t *testing.T) {
	w, dir := writerAt(t, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	zero := int64(0)
	if err := w.Append(Snapshot{Day: "2026-10-30"}, Snapshot{Day: "2026-10-30", OK: true, World: &zero}, ServerDay{Bucket: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	ls := lines(t, filepath.Join(dir, "server-daily-2026-10.jsonl"))
	for _, k := range []string{"world", "experience", "ops", "projects", "error", "last_consolidated_at", "consolidation_pending"} {
		if _, ok := ls[0][k]; ok {
			t.Errorf("nil %s was written", k)
		}
	}
	if v, ok := ls[0]["ok"]; !ok || v != false {
		t.Errorf("ok = %v, %v; want false kept", v, ok)
	}
	if v, ok := ls[1]["world"]; !ok || v != 0.0 {
		t.Errorf("a zero count must be kept: %v, %v", v, ok)
	}
	for _, k := range []string{"new", "llm"} {
		if _, ok := ls[2][k]; ok {
			t.Errorf("absent %s was written", k)
		}
	}
}

func TestRotateKeepsThirteenMonths(t *testing.T) {
	dir := t.TempDir()
	var want []string
	for y, m := 2025, 9; y < 2026 || m <= 10; {
		name := "server-daily-" + time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC).Format("2006-01") + ".jsonl"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if m++; m > 12 {
			y, m = y+1, 1
		}
	}
	want = []string{"server-daily-2025-09.jsonl"}
	for _, n := range []string{"notes.txt", "server-daily-2025-9.jsonl", "other-2020-01.jsonl", "scan-history-2025-13.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := Rotate(dir, time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), KeepMonths)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removed %v, want %v", got, want)
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 13+4 {
		t.Fatalf("%d files left, want 17", len(left))
	}
	if _, err := os.Stat(filepath.Join(dir, "server-daily-2025-10.jsonl")); err != nil {
		t.Fatal("2025-10 was removed")
	}
	if got := Rotate(dir, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), KeepMonths); len(got) != 0 {
		t.Fatalf("second run removed %v", got)
	}
	if got := Rotate(filepath.Join(dir, "absent"), time.Now(), KeepMonths); len(got) != 0 {
		t.Fatal("absent dir removed something")
	}
}

func TestReadRecentSkipsTornLines(t *testing.T) {
	dir := t.TempDir()
	good := func(ts, kind string) string {
		return `{"schema":1,"ts":"` + ts + `","kind":"` + kind + `","count":1}` + "\n"
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("scan-history-2026-09.jsonl", good("2026-09-30T10:00:00.000Z", "scan"))
	write("scan-history-2026-10.jsonl",
		good("2026-10-01T10:00:00.000Z", "scan")+
			`{"schema":1,"ts":"2026-10-02T10:0`+"\n"+ // torn
			`{"schema":2,"ts":"2026-10-03T10:00:00.000Z","kind":"scan"}`+"\n"+ // unknown schema
			`{"schema":1,"ts":"not a time","kind":"scan"}`+"\n"+
			"\n"+
			good("2026-10-04T10:00:00.000Z", "scan")+
			`{"schema":1,"ts":"2026-10-05T10:00:00.000Z","kin`) // torn tail, no newline
	write("scan-history-2026-07.jsonl", good("2026-07-01T10:00:00.000Z", "scan"))  // out of range
	write("doctor-history-2026-10.jsonl", good("2026-10-01T10:00:00.000Z", "run")) // other family
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	got, err := ReadRecent(dir, FamilyScanHistory, at, 2)
	if err != nil {
		t.Fatal(err)
	}
	var tss []string
	for _, l := range got {
		tss = append(tss, l.TS.Format(time.RFC3339))
		if l.Kind != "scan" || !json.Valid(l.Raw) {
			t.Fatalf("bad line %+v", l)
		}
	}
	want := []string{"2026-09-30T10:00:00Z", "2026-10-01T10:00:00Z", "2026-10-04T10:00:00Z"}
	if !reflect.DeepEqual(tss, want) {
		t.Fatalf("got %v, want %v", tss, want)
	}
	// months=1 on the 31st must still reach the previous month with 2.
	if got, _ := ReadRecent(dir, FamilyScanHistory, at, 1); len(got) != 2 {
		t.Fatalf("months=1 gave %d lines", len(got))
	}
	if got, err := ReadRecent(dir, "memory-docs", at, 3); err != nil || len(got) != 0 {
		t.Fatalf("absent family: %v, %v", got, err)
	}
}

func TestMemoryDocRefusesNonSessionDocuments(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		ok   bool
	}{
		{"session doc", MemoryDoc{Memory: "m1", Outcome: "found", DocKind: "session", Document: "conversation:abc@proj", Session: "abc", FactType: "world"}, true},
		{"note doc kind only", MemoryDoc{Memory: "m1", Outcome: "found", DocKind: "note", FactType: "world"}, true},
		{"note id", MemoryDoc{Memory: "m1", Outcome: "found", DocKind: "session", Document: "note:secret-title", FactType: "world"}, false},
		{"note id with note kind", MemoryDoc{Memory: "m1", Outcome: "found", DocKind: "note", Document: "note:secret-title", FactType: "world"}, false},
		{"session kind other id", MemoryDoc{Memory: "m1", Outcome: "found", DocKind: "session", Document: "import:x", FactType: "world"}, false},
		{"session set for note", MemoryDoc{Memory: "m1", Outcome: "found", DocKind: "note", Session: "abc", FactType: "world"}, false},
		{"session mismatch", MemoryDoc{Memory: "m1", Outcome: "found", DocKind: "session", Document: "conversation:abc", Session: "zzz", FactType: "world"}, false},
		{"op non-session", DocOp{Operation: "o", Document: "note:x", Session: "", Status: "pending"}, false},
		{"op session", DocOp{Operation: "o", Document: "subagent:abc:def", Session: "abc", Status: "pending"}, true},
		{"bad outcome", MemoryDoc{Memory: "m1", Outcome: "some free text", DocKind: "none", FactType: "world"}, false},
		{"bad error", Snapshot{Day: "2026-10-30", Error: "dial tcp 10.0.0.1: refused"}, false},
		{"bad day", Snapshot{Day: "today"}, false},
		{"bad check level", DoctorRun{Checks: map[string]string{"a": "it is broken"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, dir := writerAt(t, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
			err := w.Append(Scan{}, c.rec) // a good record before it must not be written either
			if c.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
			if !c.ok {
				if _, e := os.Stat(dir); e == nil {
					ents, _ := os.ReadDir(dir)
					if len(ents) != 0 {
						t.Fatalf("wrote %d files despite the refusal", len(ents))
					}
				}
			}
		})
	}
	// Pointers are validated too.
	w, _ := writerAt(t, time.Now())
	if err := w.Append(&MemoryDoc{Memory: "m", Outcome: "found", DocKind: "session", Document: "note:x", FactType: "world"}); err == nil {
		t.Fatal("pointer record skipped validation")
	}
}

// The allowlist of string fields: each is a date, an enum or an id, and
// validate guards it. A new string field fails this test until it is added
// here with its guard.
func TestRecordsHoldNoFreeText(t *testing.T) {
	allow := map[string]string{
		"Snapshot.Day":        "date",
		"Snapshot.Error":      "enum",
		"MemoryDoc.Memory":    "id",
		"MemoryDoc.Document":  "id (session documents only)",
		"MemoryDoc.Session":   "id",
		"MemoryDoc.Outcome":   "enum",
		"MemoryDoc.FactType":  "enum",
		"MemoryDoc.DocKind":   "enum",
		"DocOp.Operation":     "id",
		"DocOp.Document":      "id (session documents only)",
		"DocOp.Session":       "id",
		"DocOp.Status":        "enum",
		"DoctorRun.Checks[]":  "enum values (keys are check ids)",
		"Snapshot.Ops{}":      "keys: names",
		"Snapshot.Projects{}": "keys: names",
		"LLMDay.Statuses{}":   "keys: names",
		"ServerDay.LLM{}":     "keys: names",
	}
	seen := map[string]bool{}
	var walk func(rt reflect.Type)
	walk = func(rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			key := rt.Name() + "." + f.Name
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.String:
				seen[key] = true
				if _, ok := allow[key]; !ok {
					t.Errorf("string field %s is not in the allowlist", key)
				}
			case reflect.Map:
				if ft.Key().Kind() != reflect.String {
					t.Errorf("%s: map key is not a string", key)
				}
				switch ft.Elem().Kind() {
				case reflect.String:
					seen[key+"[]"] = true
					if _, ok := allow[key+"[]"]; !ok {
						t.Errorf("map %s holds strings and is not in the allowlist", key)
					}
				case reflect.Struct:
					seen[key+"{}"] = true
					if _, ok := allow[key+"{}"]; !ok {
						t.Errorf("map %s is not in the allowlist", key)
					}
					walk(ft.Elem())
				default:
					seen[key+"{}"] = true
					if _, ok := allow[key+"{}"]; !ok {
						t.Errorf("map %s is not in the allowlist", key)
					}
				}
			case reflect.Struct:
				if ft != reflect.TypeOf(time.Time{}) {
					walk(ft)
				}
			case reflect.Slice, reflect.Array, reflect.Interface:
				t.Errorf("%s: %s can hold text", key, ft.Kind())
			}
		}
	}
	for _, r := range []Record{Snapshot{}, ServerDay{}, DoctorRun{}, MemoryDoc{}, DocOp{}, Scan{}} {
		walk(reflect.TypeOf(r))
	}
	for k := range allow {
		if !seen[k] {
			t.Errorf("allowlist entry %s matches no field", k)
		}
	}
}
