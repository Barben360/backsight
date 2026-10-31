package index

import (
	"embed"
	"io/fs"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Embedded is the registry built into the binary (Options.Migrations nil):
// the migrations directory itself, so its root holds the .sql files as
// LoadRegistry reads them.
var Embedded = mustSub(migrationFiles, "migrations")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err) // dir is a constant that the embed directive guarantees
	}
	return sub
}
