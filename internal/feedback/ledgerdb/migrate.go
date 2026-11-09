package ledgerdb

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Barben360/backsight/internal/sqlitedb"
)

// MigrateResult is what Migrate found and did.
type MigrateResult struct {
	// Built is true when the file (File) already existed before this
	// call, false when it did not: Applied then lists the migrations of
	// the file this call built from the journal (nothing with plan).
	Built bool `json:"built"`
	// Newer: ledger.db is newer than this binary, which uses its own side
	// file instead (File).
	Newer bool `json:"newer"`
	// File is the base name of the file used: ledger.db, or the side file.
	File    string   `json:"file"`
	Pending []string `json:"pending"`
	Applied []string `json:"applied"`
	// SetAside: the file was corrupt; it was set aside as
	// <File>.corrupt and a fresh one built from the journal (Applied
	// lists its migrations).
	SetAside bool `json:"set_aside,omitempty"`
}

// Migrate brings the ledger database to this binary's head: it applies
// the pending migrations of an existing file and builds a missing one
// from the journal (OpenWriter), so that the first tick after an update
// only folds. A ledger.db newer than this binary is left as it is: its
// side file is migrated, or built, instead. A corrupt file is set aside
// and a fresh one built (OpenWriter), which SetAside reports. plan only
// lists the pending
// migrations, every one of them for a missing file, and creates nothing.
// During an uncommitted update a file that needs either returns
// ErrUpdateInProgress, nothing written. Applied is what was pending when
// the file was looked at: an open that succeeds leaves the file at the
// head, whichever process applied them.
func Migrate(ctx context.Context, o Options, plan bool) (MigrateResult, error) {
	o = o.withDefaults()
	reg, err := registry(o)
	if err != nil {
		return MigrateResult{}, err
	}
	t, err := resolve(ctx, o, sqlitedb.Head(reg))
	var ce *corruptErr
	switch {
	case err == nil:
	case !plan && errors.As(err, &ce): // the writer sets it aside and builds a fresh one
		t = target{path: ce.path}
	default:
		return MigrateResult{}, err
	}
	_, serr := os.Lstat(t.path)
	res := MigrateResult{
		Built: !errors.Is(serr, fs.ErrNotExist), Newer: t.side, File: filepath.Base(t.path),
		Pending: []string{}, Applied: []string{},
	}
	if t.known {
		for _, m := range reg {
			if m.Num > t.version {
				res.Pending = append(res.Pending, m.ID)
			}
		}
	}
	if plan {
		if !t.known { // only the writer, which recovers the file, could read it
			return res, ErrUnreadable
		}
		return res, nil
	}
	aside := t.path + ".corrupt"
	before, _ := os.Stat(aside)
	w, err := OpenWriter(ctx, o)
	if err != nil {
		return res, err
	}
	// A newer binary may have migrated ledger.db meanwhile: the writer
	// then took the side file.
	res.Newer, res.File = w.side, filepath.Base(w.path)
	res.Applied = append(res.Applied, res.Pending...)
	if setAsideDuring(before, aside) {
		// The fresh file was built at the head: every migration applied.
		res.SetAside, res.Applied = true, []string{}
		for _, m := range reg {
			res.Applied = append(res.Applied, m.ID)
		}
	}
	return res, w.Close()
}

// setAsideDuring: aside, missing or as before was before the open, is a
// corrupt file the open set aside (setAside stamps its mtime).
func setAsideDuring(before fs.FileInfo, aside string) bool {
	after, err := os.Stat(aside)
	if err != nil {
		return false
	}
	return before == nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size()
}
