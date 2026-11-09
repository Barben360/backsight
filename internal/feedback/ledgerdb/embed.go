package ledgerdb

import (
	"embed"
	"io/fs"
	"sync"

	"github.com/Barben360/backsight/internal/sqlitedb"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Embedded is the registry built into the binary (Options.Migrations
// nil): the migrations directory itself, so its root holds the .sql files
// as sqlitedb.LoadRegistry reads them.
var Embedded = mustSub(migrationFiles, "migrations")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err) // dir is a constant that the embed directive guarantees
	}
	return sub
}

// embedded is Embedded's registry, read once per process (the reader
// opens per call, in hooks).
var embedded = sync.OnceValues(func() ([]sqlitedb.Migration, error) {
	return sqlitedb.LoadRegistry(Embedded, "ledger")
})

// registry is o's migrations (nil: Embedded).
func registry(o Options) ([]sqlitedb.Migration, error) {
	if o.Migrations == nil {
		return embedded()
	}
	return sqlitedb.LoadRegistry(o.Migrations, "ledger")
}

// Head is the schema version of o's registry (Embedded when
// o.Migrations is nil): the user_version this binary writes and reads.
func Head(o Options) (int, error) {
	reg, err := registry(o)
	if err != nil {
		return 0, err
	}
	return sqlitedb.Head(reg), nil
}
