package indextest

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/paths"
)

func dirsIn(t *testing.T) paths.Dirs {
	root := t.TempDir()
	return paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config"), State: filepath.Join(root, "state"), Bin: filepath.Join(root, "bin")}
}

func optsOf(d paths.Dirs) index.Options {
	return index.Options{Dirs: d, Home: filepath.Dir(d.State), Getenv: func(string) string { return "" }}
}

// A seeded index is at the head: nothing to migrate, and an open applies
// nothing.
func TestSeedIsAtTheHead(t *testing.T) {
	d := dirsIn(t)
	Seed(t, d)
	res, err := index.Migrate(context.Background(), optsOf(d), true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Built || len(res.Pending) != 0 || res.Newer {
		t.Fatalf("seeded index: %+v, want built with nothing pending", res)
	}
	reg, err := index.LoadRegistry(index.Embedded)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := index.Open(context.Background(), optsOf(d))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()
	if ix.Schema() != index.Head(reg) {
		t.Errorf("schema %d, want the head %d", ix.Schema(), index.Head(reg))
	}
}

// A seeded index holds what a fresh build holds: the same schema, the
// same rows in each table (schema_migrations apart, which records when).
func TestSeedEqualsAFreshBuild(t *testing.T) {
	seeded := dirsIn(t)
	Seed(t, seeded)
	fresh := dirsIn(t)
	ix, err := index.Open(context.Background(), optsOf(fresh))
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	gotSchema, gotRows := contents(t, seeded)
	wantSchema, wantRows := contents(t, fresh)
	if !slices.Equal(gotSchema, wantSchema) {
		t.Errorf("schema differs from a fresh build:\n got %q\nwant %q", gotSchema, wantSchema)
	}
	if !slices.Equal(gotRows, wantRows) {
		t.Errorf("rows differ from a fresh build:\n got %q\nwant %q", gotRows, wantRows)
	}
}

// contents is the schema of d's index and, for each table, its name and
// row count (schema_migrations: its ids).
func contents(t *testing.T, d paths.Dirs) (schema, rows []string) {
	t.Helper()
	cp := filepath.Join(t.TempDir(), "copy.db")
	raw, err := os.ReadFile(filepath.Join(d.IndexDir(), index.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+cp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	q := func(query string) []string {
		t.Helper()
		r, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		var out []string
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	schema = q(`SELECT type || ' ' || name || ' ' || coalesce(sql, '') FROM sqlite_schema ORDER BY type, name`)
	for _, tbl := range q(`SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`) {
		if tbl == "schema_migrations" {
			rows = append(rows, q(`SELECT 'schema_migrations ' || id || ' ' || sha256 FROM schema_migrations ORDER BY id`)...)
			continue
		}
		rows = append(rows, q(`SELECT '`+tbl+` ' || count(*) FROM `+tbl)...)
	}
	return schema, rows
}

// Seed never replaces a file already there (a test's own index).
func TestSeedKeepsAnExistingFile(t *testing.T) {
	d := dirsIn(t)
	if err := os.MkdirAll(d.IndexDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d.IndexDir(), index.FileName)
	if err := os.WriteFile(path, []byte("a test's own index"), 0o600); err != nil {
		t.Fatal(err)
	}
	Seed(t, d)
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "a test's own index" {
		t.Errorf("the file was replaced: %q, %v", raw, err)
	}
}
