package index

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

var updateFixture = flag.Bool("update-fixture", false, "rewrite testdata/schema-0001.db")

const fixturePath = "testdata/schema-0001.db"

// released pins every shipped migration (each release adds its own).
var released = map[string]string{
	"0001-initial": "ce52343c6c05776b639824d1435b64fe8e64bb01929be065a9a92632b9be8b7d",
}

// rowKey starts every row table: the line it came from.
const rowKey = "source_file offset "

var schemaTables = []struct{ name, cols string }{
	{"tel_prompt", rowKey + "ts session project binary_version v n decision reason plen lang above injected ids_n top ms nudged hinted detector chars empty"},
	{"tel_outcome", rowKey + "ts session project binary_version v n ids_n searched credited credit_source contradicted"},
	{"tel_reply", rowKey + "ts session project binary_version v n credit searched"},
	{"tel_briefing", rowKey + "ts session project binary_version v ids_n chars source"},
	{"tel_briefing_outcome", rowKey + "ts session project binary_version v used_n"},
	{"tel_tool", rowKey + "ts session project binary_version v name ms hits ok live"},
	{"tel_tool_use", rowKey + "ts session project binary_version v n name ids_n subagent"},
	{"tel_credit", rowKey + "ts session project binary_version v verdict ids_n ok"},
	{"tel_credit_check", rowKey + "ts session project binary_version v outcome shown flags after_block"},
	{"tel_retire", rowKey + "ts session project binary_version v action ids_n"},
	{"tel_feedback_settle", rowKey + "ts session project binary_version v shows credits misses unknown late"},
	{"tel_feedback_error", rowKey + "ts session project binary_version v reason"},
	{"tel_gate", rowKey + "ts session project binary_version v n decision reason"},
	{"tel_retain", rowKey + "ts session project binary_version v document items status ms"},
	{"tel_hook_failure", rowKey + "ts session project binary_version v hook kind ms"},
	{"tel_job", rowKey + "ts session project binary_version v name status ms"},
	{"tel_attribution", rowKey + "ts session project binary_version v turn subagent base applied path_rule source"},
	{"tel_store", rowKey + "ts session project binary_version v result kind"},
	{"tel_dashboard", rowKey + "ts session project binary_version v action ms pages"},
	{"tel_config", rowKey + "ts session project binary_version v sha256"},
	{"tel_ids", rowKey + "ts kind pos memory_id"},
	{"tel_prompt_scopes", rowKey + "ts scope ok ms raw"},
	{"tel_config_values", rowKey + "ts key value"},
	{"events", rowKey + "ts kind session project binary_version fields"},
	{"turns", rowKey + "ts session turn agent project n machine binary_version"},
	{"shows", rowKey + "pos ts session key turn agent path memory_id rank reranker boost rank_without_boost score_without_boost project"},
	{"settles", rowKey + "pos key memory_id show_at outcome settle_ts"},
	{"credits", rowKey + "pos ts session turn agent project memory_id verdict"},
	{"stops", rowKey + "ts session turn agent outcome stop_check flags"},
	{"retirements", rowKey + "pos ts session kind memory_id verdict outcome note_id"},
	{"recalls", rowKey + "pos ts session turn agent path memory_id rank rank_without_boost reranker boost score_without_boost flags"},
	{"imports", rowKey + "pos ts source key memory_id at outcome rank"},
	{"journal_other", rowKey + "ts session kind"},
	{"ledger", "memory_id shown credits credits_at misses misses_at last_shown last_credited wrong hidden_since unhidden_at imported_shown imported_credits"},
	{"local_status", "id ingested_at notes_queued notes_oldest_at quarantined quarantine_oldest_at sessions_waiting reach_last_ok reach_failing_since breaker_failures breaker_open_until backup_created_at backup_verified_at backup_ok update_checked_at update_newer update_check_failed backfill_sessions backfill_paused"},
	{"cursors", "source_file kind size offset head_len head_sha256 ingested_at last_ts missing"},
	{"ingest_errors", "source_file offset reason at"},
	{"meta", "key value"},
}

var schemaViews = []string{"turn_final_stop", "document_status"}

func TestReleasedMigrationsArePinned(t *testing.T) {
	reg, err := LoadRegistry(Embedded)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkPins(reg, released); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaColumns(t *testing.T) {
	ix := mustOpen(t, opts(t, tempDirs(t), nil))
	defer mustClose(t, ix)
	if ix.Schema() != 1 {
		t.Fatalf("schema %d, want 1", ix.Schema())
	}
	for _, tc := range schemaTables {
		got := columnsOf(t, ix.sh.w, tc.name)
		if want := strings.Fields(tc.cols); !slices.Equal(got, want) {
			t.Errorf("%s columns\n got %v\nwant %v", tc.name, got, want)
		}
	}
	for _, v := range schemaViews {
		var n int
		if err := ix.sh.w.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='view' AND name=?`, v).Scan(&n); err != nil || n != 1 {
			t.Errorf("view %s: n=%d err=%v", v, n, err)
		}
	}
	var tables int
	if err := ix.sh.w.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_migrations'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != len(schemaTables) {
		t.Errorf("%d tables, the test lists %d: a table is missing from schemaTables", tables, len(schemaTables))
	}
}

func columnsOf(t *testing.T, q interface {
	Query(string, ...any) (*sql.Rows, error)
}, table string) []string {
	t.Helper()
	rows, err := q.Query(fmt.Sprintf(`SELECT name FROM pragma_table_info('%s') ORDER BY cid`, table))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	return cols
}

func TestSchemaConstraints(t *testing.T) {
	ix := mustOpen(t, opts(t, tempDirs(t), nil))
	defer mustClose(t, ix)
	w := ix.sh.w
	ins := `INSERT INTO shows(source_file, "offset", pos, ts) VALUES ('f', 0, 0, 1)`
	if _, err := w.Exec(ins); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(ins); err == nil {
		t.Error("a duplicate (source_file, offset, pos) was accepted")
	}
	if _, err := w.Exec(`INSERT INTO shows(source_file, "offset", pos, ts) VALUES ('f', 0, 1, 1)`); err != nil {
		t.Errorf("another pos of the same line refused: %v", err)
	}
	// pos defaults to 0, so a line's single row and its pos 0 collide.
	if _, err := w.Exec(`INSERT INTO credits(source_file, "offset", ts) VALUES ('f', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	var pos int
	if err := w.QueryRow(`SELECT pos FROM credits`).Scan(&pos); err != nil || pos != 0 {
		t.Errorf("pos default %d, err %v", pos, err)
	}
	bad := []string{
		`INSERT INTO credits(source_file, "offset", ts) VALUES ('f', 0, 2)`,
		`INSERT INTO tel_prompt_scopes(source_file, "offset", ts, scope) VALUES ('f', 0, 1, 'a')`,
		`INSERT INTO tel_prompt_scopes(source_file, "offset", ts, scope) VALUES ('f', 0, 1, 'a')`,
		`INSERT INTO tel_config_values(source_file, "offset", ts, key) VALUES ('f', 0, 1, 'a')`,
		`INSERT INTO tel_config_values(source_file, "offset", ts, key) VALUES ('f', 0, 1, 'a')`,
		`INSERT INTO tel_ids(source_file, "offset", ts, pos, memory_id) VALUES ('f', 0, 1, 0, 'm')`,
		`INSERT INTO tel_ids(source_file, "offset", ts, pos, memory_id) VALUES ('f', 0, 1, 0, 'm')`,
		`INSERT INTO stops(source_file, "offset") VALUES (NULL, 0)`,
		`INSERT INTO cursors(source_file, kind) VALUES ('f', 'other')`,
		`INSERT INTO local_status(id) VALUES (2)`,
		`INSERT INTO ingest_errors(source_file, "offset", reason) VALUES ('f', 0, NULL)`,
		`INSERT INTO tel_ids(source_file, "offset", ts, kind, pos, memory_id) VALUES ('f', 0, 1, 'prompt', 0, NULL)`,
	}
	for i, q := range bad {
		_, err := w.Exec(q)
		first := i == 1 || i == 3 || i == 5 // the first of each duplicate pair is valid
		if first {
			if err != nil {
				t.Errorf("refused: %s: %v", q, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
}

// TestNullTimeRefused: a row without its time never enters the index (the
// line goes to ingest_errors instead).
func TestNullTimeRefused(t *testing.T) {
	ix := mustOpen(t, opts(t, tempDirs(t), nil))
	defer mustClose(t, ix)
	for _, tc := range schemaTables {
		col := "ts"
		switch tc.name {
		case "settles":
			col = "settle_ts"
		case "ledger", "local_status", "cursors", "ingest_errors", "meta":
			continue
		}
		cols, vals := `source_file, "offset", `+col, `'f', 0, NULL`
		if tc.name == "tel_ids" { // memory_id is NOT NULL too: supply it so only the time can fail
			cols, vals = cols+", memory_id", vals+", 'm'"
		}
		q := fmt.Sprintf(`INSERT INTO %s(%s) VALUES (%s)`, tc.name, cols, vals)
		_, err := ix.sh.w.Exec(q)
		if want := "NOT NULL constraint failed: " + tc.name + "." + col; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", q, err, want)
		}
	}
}

// TestSchemaIndexes: the indexes the dashboard queries rely on.
func TestSchemaIndexes(t *testing.T) {
	ix := mustOpen(t, opts(t, tempDirs(t), nil))
	defer mustClose(t, ix)
	want := map[string][]string{}
	for _, tc := range schemaTables {
		f := strings.Fields(tc.cols)
		if !strings.HasPrefix(tc.cols, rowKey) || tc.name == "ingest_errors" {
			continue
		}
		tcol := "ts"
		if tc.name == "settles" {
			tcol = "settle_ts"
		}
		want[tc.name] = append(want[tc.name], tc.name+"_"+tcol+":"+tcol)
		if slices.Contains(f, "memory_id") {
			want[tc.name] = append(want[tc.name], tc.name+"_memory_id:memory_id")
		}
	}
	for _, n := range []string{"shows", "stops", "credits", "turns"} {
		want[n] = append(want[n], n+"_session_turn:session,turn")
	}
	for tbl, idx := range want {
		var got []string
		rows, err := ix.sh.w.Query(`SELECT il.name, group_concat(ii.name) FROM pragma_index_list(?) il
			JOIN pragma_index_info(il.name) ii WHERE il.origin = 'c' GROUP BY il.name`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var n, c string
			if err := rows.Scan(&n, &c); err != nil {
				t.Fatal(err)
			}
			got = append(got, n+":"+c)
		}
		_ = rows.Close()
		slices.Sort(got)
		slices.Sort(idx)
		if !slices.Equal(got, idx) {
			t.Errorf("%s indexes\n got %v\nwant %v", tbl, got, idx)
		}
	}
}

func TestViews(t *testing.T) {
	ix := mustOpen(t, opts(t, tempDirs(t), nil))
	defer mustClose(t, ix)
	w := ix.sh.w
	for _, q := range []string{
		`INSERT INTO stops(source_file, "offset", ts, session, turn, agent, outcome) VALUES
		 ('a', 0, 1, 's', 1, 'main', 'blocked'), ('a', 10, 2, 's', 1, 'main', 'blocked'),
		 ('b', 0, 3, 's', 1, 'main', 'done'), ('a', 5, 4, 's', 2, 'main', 'blocked'), ('a', 7, 5, 's', 1, 'sub', 'ok')`,
		`INSERT INTO tel_retain(source_file, "offset", ts, document, status, ms) VALUES
		 ('a', 0, 5, 'd', 'queued', 1), ('a', 9, 5, 'd', 'done', 2), ('a', 3, 1, 'e', 'failed', 3)`,
	} {
		if _, err := w.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	got := queryStrings(t, w, `SELECT session||'/'||turn||'/'||agent||'/'||outcome||'/'||ts FROM turn_final_stop ORDER BY turn, agent`)
	want := []string{"s/1/main/done/3", "s/1/sub/ok/5", "s/2/main/undeclared/4"}
	if !slices.Equal(got, want) {
		t.Errorf("turn_final_stop %v, want %v", got, want)
	}
	got = queryStrings(t, w, `SELECT document||'/'||status||'/'||ms FROM document_status ORDER BY document`)
	want = []string{"d/done/2", "e/failed/3"}
	if !slices.Equal(got, want) {
		t.Errorf("document_status %v, want %v", got, want)
	}
}

func queryStrings(t *testing.T, w *sql.DB, q string) []string {
	t.Helper()
	rows, err := w.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// onePerTable inserts one row into every table, each column valued by its
// declared type; cursors and local_status carry their constrained values.
func onePerTable(t *testing.T, w *sql.DB) {
	t.Helper()
	for _, tc := range schemaTables {
		rows, err := w.Query(fmt.Sprintf(`SELECT name, type FROM pragma_table_info('%s') ORDER BY cid`, tc.name))
		if err != nil {
			t.Fatal(err)
		}
		var names, vals []string
		for rows.Next() {
			var n, ty string
			if err := rows.Scan(&n, &ty); err != nil {
				t.Fatal(err)
			}
			names = append(names, `"`+n+`"`)
			switch {
			case n == "kind" && tc.name == "cursors":
				vals = append(vals, `'journal'`)
			case n == "id" && tc.name == "local_status":
				vals = append(vals, "1")
			case ty == "INTEGER":
				vals = append(vals, "1")
			case ty == "REAL":
				vals = append(vals, "0.5")
			default:
				vals = append(vals, `'x'`)
			}
		}
		_ = rows.Close()
		q := fmt.Sprintf(`INSERT INTO %s(%s) VALUES (%s)`, tc.name, strings.Join(names, ","), strings.Join(vals, ","))
		if _, err := w.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func schemaOf(t *testing.T, w *sql.DB) []string {
	t.Helper()
	return queryStrings(t, w, `SELECT type||'|'||name||'|'||tbl_name||'|'||coalesce(sql,'') FROM sqlite_schema ORDER BY type, name`)
}

func counts(t *testing.T, w *sql.DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	names := append([]string{"schema_migrations"}, func() []string {
		var n []string
		for _, tc := range schemaTables {
			n = append(n, tc.name)
		}
		return n
	}()...)
	for _, n := range names {
		var c int
		if err := w.QueryRow("SELECT count(*) FROM " + n).Scan(&c); err != nil {
			t.Fatal(err)
		}
		out[n] = c
	}
	return out
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	dst, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// withTest is the embedded registry plus a test-only 0002 migration.
func withTest(t *testing.T) fs.FS {
	t.Helper()
	m := fstest.MapFS{}
	ents, err := fs.ReadDir(Embedded, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := fs.ReadFile(Embedded, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		m[e.Name()] = &fstest.MapFile{Data: b}
	}
	m["0002-test.sql"] = &fstest.MapFile{Data: []byte("ALTER TABLE shows ADD COLUMN extra INTEGER; CREATE INDEX shows_extra ON shows(extra);")}
	return m
}

// TestSchemaFixture: the committed 0001 file, from the first release,
// keeps its rows and equals a fresh build when opened with the registry,
// and with a later migration on top.
func TestSchemaFixture(t *testing.T) {
	if *updateFixture {
		d := tempDirs(t)
		ix := mustOpen(t, opts(t, d, nil))
		onePerTable(t, ix.sh.w)
		mustClose(t, ix)
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		copyFile(t, mainFile(d), fixturePath)
		t.Logf("wrote %s", fixturePath)
	}
	if _, err := os.Stat(fixturePath); err != nil {
		t.Fatalf("%v: run with -update-fixture and commit it", err)
	}
	fresh := func(reg fs.FS) ([]string, map[string]int) {
		ix := mustOpen(t, opts(t, tempDirs(t), reg))
		defer mustClose(t, ix)
		return schemaOf(t, ix.sh.w), counts(t, ix.sh.w)
	}
	for _, tc := range []struct {
		name string
		reg  fs.FS
		head int
	}{{"released", nil, 1}, {"with a later migration", withTest(t), 2}} {
		t.Run(tc.name, func(t *testing.T) {
			d := tempDirs(t)
			copyFile(t, fixturePath, mainFile(d))
			ix := mustOpen(t, opts(t, d, tc.reg))
			defer mustClose(t, ix)
			if ix.Schema() != tc.head {
				t.Fatalf("schema %d, want %d", ix.Schema(), tc.head)
			}
			wantSchema, _ := fresh(tc.reg)
			if got := schemaOf(t, ix.sh.w); !slices.Equal(got, wantSchema) {
				t.Errorf("schema differs from a fresh build\n got %s\nwant %s", strings.Join(got, "\n"), strings.Join(wantSchema, "\n"))
			}
			got := counts(t, ix.sh.w)
			for _, tbl := range schemaTables {
				if got[tbl.name] != 1 {
					t.Errorf("%s: %d rows kept, want 1", tbl.name, got[tbl.name])
				}
			}
			if want := tc.head; got["schema_migrations"] != want {
				t.Errorf("schema_migrations %d rows, want %d", got["schema_migrations"], want)
			}
		})
	}
}
