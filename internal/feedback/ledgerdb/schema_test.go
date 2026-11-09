package ledgerdb

import (
	"database/sql"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Barben360/backsight/internal/sqlitedb"
)

var updateFixture = flag.Bool("update-fixture", false, "write testdata/schema-NNNN.db of the registry's head")

// released pins every shipped ledger migration (each release adds its own).
var released = map[string]string{
	"0001-initial": "7787154e31e999abde62711bf35b69024de8dab7a93e0372614aed374b6b6808",
}

// tables are the tables of the head schema, schema_migrations aside.
var tables = []string{"memories", "meta", "ledger_cursors", "settled_keys"}

// fixturePath is the committed ledger.db of schema head (one row in each
// table), from the release that shipped it.
func fixturePath(head int) string {
	return filepath.Join("testdata", fmt.Sprintf("schema-%04d.db", head))
}

func embeddedHead(t *testing.T) int {
	t.Helper()
	reg, err := sqlitedb.LoadRegistry(Embedded, "ledger")
	if err != nil {
		t.Fatal(err)
	}
	return sqlitedb.Head(reg)
}

// withTestMigration is the embedded registry plus a test-only migration,
// numbered head+1.
func withTestMigration(t *testing.T) fs.FS {
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
	m[fmt.Sprintf("%04d-test.sql", embeddedHead(t)+1)] = &fstest.MapFile{
		Data: []byte("ALTER TABLE memories ADD COLUMN extra INTEGER NOT NULL DEFAULT 0; CREATE TABLE extra_t(x INTEGER);"),
	}
	return m
}

func TestReleasedLedgerMigrationsArePinned(t *testing.T) {
	reg, err := sqlitedb.LoadRegistry(Embedded, "ledger")
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitedb.CheckPins(reg, released, "ledger"); err != nil {
		t.Fatal(err)
	}
}

func TestHeadIsTheRegistrysLast(t *testing.T) {
	if h, err := Head(Options{}); err != nil || h != embeddedHead(t) || h < 1 {
		t.Errorf("Head = %d, %v; want %d", h, err, embeddedHead(t))
	}
	if h, err := Head(Options{Migrations: withTestMigration(t)}); err != nil || h != embeddedHead(t)+1 {
		t.Errorf("Head with a test migration = %d, %v; want %d", h, err, embeddedHead(t)+1)
	}
}

func queryStrings(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func schemaOf(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return queryStrings(t, db, `SELECT type||'|'||name||'|'||tbl_name||'|'||coalesce(sql,'') FROM sqlite_schema ORDER BY type, name`)
}

func countOf(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// onePerTable inserts one row into every table, each column valued by its
// declared type.
func onePerTable(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, tbl := range tables {
		rows, err := db.Query(`SELECT name, type FROM pragma_table_info(?) ORDER BY cid`, tbl)
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
			switch ty {
			case "INTEGER":
				vals = append(vals, "1")
			case "REAL":
				vals = append(vals, "0.5")
			default:
				vals = append(vals, `'x'`)
			}
		}
		_ = rows.Close()
		q := fmt.Sprintf(`INSERT INTO %s(%s) VALUES (%s)`, tbl, strings.Join(names, ","), strings.Join(vals, ","))
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// TestLedgerSchemaFixture: each committed testdata/schema-NNNN.db, from
// the release that shipped schema NNNN, keeps its rows and equals a fresh
// build once opened with the registry, and with a later migration on top.
func TestLedgerSchemaFixture(t *testing.T) {
	head := embeddedHead(t)
	if *updateFixture {
		o := tempOpts(t)
		w := mustOpenWriter(t, o)
		onePerTable(t, w.db)
		mustCloseWriter(t, w)
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixturePath(head), readBytes(t, mainPath(o)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", fixturePath(head))
	}
	fixtures, err := filepath.Glob("testdata/schema-[0-9][0-9][0-9][0-9].db")
	if err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= head; v++ {
		if !slices.Contains(fixtures, fixturePath(v)) {
			t.Fatalf("%s is missing: run with -update-fixture at that release and commit it", fixturePath(v))
		}
	}
	fresh := func(reg fs.FS) []string {
		o := tempOpts(t)
		o.Migrations = reg
		w := mustOpenWriter(t, o)
		defer mustCloseWriter(t, w)
		return schemaOf(t, w.db)
	}
	for _, fixture := range fixtures {
		for _, tc := range []struct {
			name string
			reg  fs.FS
			head int
		}{{"released", nil, head}, {"with a later migration", withTestMigration(t), head + 1}} {
			t.Run(filepath.Base(fixture)+"/"+tc.name, func(t *testing.T) {
				o := tempOpts(t)
				o.Migrations = tc.reg
				writeFile(t, mainPath(o), readBytes(t, fixture))
				w := mustOpenWriter(t, o)
				defer mustCloseWriter(t, w)
				if got := pragma(t, w.db, "user_version"); got != fmt.Sprint(tc.head) {
					t.Fatalf("user_version %s, want %d", got, tc.head)
				}
				if got, want := schemaOf(t, w.db), fresh(tc.reg); !slices.Equal(got, want) {
					t.Errorf("schema differs from a fresh build\n got %s\nwant %s", strings.Join(got, "\n"), strings.Join(want, "\n"))
				}
				for _, tbl := range tables {
					if n := countOf(t, w.db, tbl); n != 1 {
						t.Errorf("%s: %d rows kept, want the fixture's 1", tbl, n)
					}
				}
				if n := countOf(t, w.db, "schema_migrations"); n != tc.head {
					t.Errorf("schema_migrations %d rows, want %d", n, tc.head)
				}
			})
		}
	}
}
