package index

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"time"
)

// SourceLag is one month file the index follows: what is on disk, how far
// the index read, and how far behind it is.
type SourceLag struct {
	File       string `json:"file"`
	Size       int64  `json:"size"`
	Offset     int64  `json:"offset"`
	LagSeconds int64  `json:"lag_seconds"` // 0 at the end; else now − last_ts (the file's mtime when nothing was read yet)
	Missing    bool   `json:"missing,omitempty"`
	Behind     bool   `json:"behind,omitempty"` // a complete line is left to read (a partial tail is not)
}

// Status is what Inspect found, for `stats index`.
type Status struct {
	File            string         `json:"file"`
	Using           string         `json:"using"` // metrics.db or metrics-v<head>.db
	Exists          bool           `json:"exists"`
	Readable        bool           `json:"readable"`
	Size            int64          `json:"size"` // with -wal
	Schema          int            `json:"schema"`
	Head            int            `json:"head"`
	Pending         int            `json:"pending"`
	Newer           bool           `json:"newer"`
	JournalMode     string         `json:"journal_mode"`
	Filesystem      FSInfo         `json:"filesystem"`
	IngestedAt      time.Time      `json:"ingested_at,omitzero"`
	LagSeconds      int64          `json:"lag_seconds"`
	Sources         []SourceLag    `json:"sources"`
	Errors30d       int            `json:"ingest_errors_30d"`
	Errors24h       int            `json:"ingest_errors_24h"`
	ErrorsByReason  map[string]int `json:"ingest_errors_by_reason,omitempty"`
	SideFiles       []string       `json:"side_files,omitempty"`
	AsideFiles      []string       `json:"aside_files,omitempty"` // metrics.db.corrupt
	RebuiltAt       time.Time      `json:"rebuilt_at,omitzero"`
	StoppedAt       time.Time      `json:"stopped_at,omitzero"`
	MigrateFailedAt time.Time      `json:"migrate_failed_at,omitzero"`
}

var sideNameRE = regexp.MustCompile(`^metrics-v[0-9]+\.db$`)

// Inspect reports the index without changing anything: it never creates
// the file or its directory, migrates, ingests or sets a file aside, takes
// no lock, and works during an update, on an index newer than this binary
// and on a corrupt one (Readable false). The file is read through a
// read-only connection, as the version probe does. With a leftover -wal
// (the probe is then not immutable) that connection may create a -shm,
// SQLite's shared-memory index, which holds no data; nothing else is
// written. Only a registry that
// cannot be loaded, or a source directory that cannot be listed, is an
// error.
func Inspect(ctx context.Context, o Options) (Status, error) {
	o = o.withDefaults()
	reg, err := registry(o)
	if err != nil {
		return Status{}, err
	}
	head := Head(reg)
	dir := o.Dirs.IndexDir()
	st := Status{File: filepath.Join(dir, FileName), Using: FileName, Head: head, Sources: []SourceLag{}}
	st.Filesystem = detectFS(existingDir(dir), o.Home, o.Getenv)
	if rs, err := readStatus(o.Dirs); err == nil {
		st.RebuiltAt, st.StoppedAt, st.MigrateFailedAt = rs.RebuiltAt, rs.StoppedAt, rs.MigrateFailedAt
	}
	st.SideFiles, st.AsideFiles = companions(dir)

	v, built, perr := probeVersion(ctx, st.File)
	if perr == nil && built && v > head {
		st.Newer = true
		st.Using = SideFileName(head)
		st.File = filepath.Join(dir, st.Using)
		v, built, perr = probeVersion(ctx, st.File)
	}
	st.Exists = built
	if !built {
		return st, nil
	}
	st.Size = sizeWithWAL(st.File)
	if perr != nil {
		return st, nil
	}
	st.Readable, st.Schema = true, v
	for _, m := range reg {
		if m.Num > v {
			st.Pending++
		}
	}
	st.JournalMode = journalModeOf(st.File)
	if v == 0 { // no schema yet: nothing to read
		return st, nil
	}
	if err := inspectRows(ctx, o, &st); err != nil {
		var fe *sourcesError
		if errors.As(err, &fe) {
			return st, fe.err
		}
		st.Readable = false
	}
	return st, nil
}

// sourcesError: the source directories could not be listed, which is no
// verdict on the index.
type sourcesError struct{ err error }

func (e *sourcesError) Error() string { return e.err.Error() }
func (e *sourcesError) Unwrap() error { return e.err }

// existingDir is dir, or its nearest existing parent: the filesystem an
// index not built yet will live on.
func existingDir(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// companions lists the side files of newer indexes and the corrupt one set
// aside in dir.
func companions(dir string) (side, aside []string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	for _, e := range ents {
		switch name := e.Name(); {
		case sideNameRE.MatchString(name):
			side = append(side, name)
		case name == FileName+".corrupt":
			aside = append(aside, name)
		}
	}
	return side, aside
}

func sizeWithWAL(file string) int64 {
	var n int64
	for _, p := range []string{file, file + "-wal"} {
		if info, err := os.Stat(p); err == nil {
			n += info.Size()
		}
	}
	return n
}

// journalModeOf reads the mode from the header (bytes 18 and 19 are 2 in
// WAL mode), so that no pragma is run.
func journalModeOf(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	var h [20]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return ""
	}
	if h[18] == 2 && h[19] == 2 {
		return "wal"
	}
	return "delete"
}

type inspectCursor struct {
	offset     int64
	lastTS     sql.NullInt64
	ingestedAt sql.NullInt64
}

// inspectRows fills st's sources and ingest errors from the file, which it
// opens read only.
func inspectRows(ctx context.Context, o Options, st *Status) error {
	db, err := sql.Open(driverName, probeDSN(st.File, idleWAL(st.File)))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	tables, err := tableNames(ctx, db, `SELECT name FROM sqlite_schema WHERE type = 'table'`)
	if err != nil {
		return err
	}
	if !slices.Contains(tables, "cursors") || !slices.Contains(tables, "ingest_errors") {
		return nil // not the metrics schema: nothing to follow
	}
	cursors := map[string]inspectCursor{}
	rows, err := db.QueryContext(ctx, `SELECT source_file, "offset", last_ts, ingested_at FROM cursors`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var rel string
		var off sql.NullInt64
		var c inspectCursor
		if err := rows.Scan(&rel, &off, &c.lastTS, &c.ingestedAt); err != nil {
			_ = rows.Close()
			return err
		}
		c.offset = off.Int64
		cursors[rel] = c
		if c.ingestedAt.Valid && c.ingestedAt.Int64 > st.IngestedAt.UnixMilli() {
			st.IngestedAt = time.UnixMilli(c.ingestedAt.Int64).UTC()
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}

	now := o.Now()
	srcs, err := (&Index{opts: o}).sources()
	if err != nil {
		return &sourcesError{err}
	}
	onDisk := map[string]bool{}
	for _, s := range srcs {
		onDisk[s.Rel] = true
		info, err := os.Stat(s.Path)
		if err != nil {
			continue // vanished meanwhile: listed as gone below if it has a cursor
		}
		c, known := cursors[s.Rel]
		sl := SourceLag{File: s.Rel, Size: info.Size(), Offset: c.offset}
		if linesLeft(s.Path, c.offset) {
			sl.Behind = true
			from := info.ModTime().UnixMilli()
			if known && c.lastTS.Valid {
				from = c.lastTS.Int64
			}
			sl.LagSeconds = max(now.UnixMilli()-from, 0) / 1000
		}
		st.LagSeconds = max(st.LagSeconds, sl.LagSeconds)
		st.Sources = append(st.Sources, sl)
	}
	var gone []string
	for rel := range cursors {
		if !onDisk[rel] {
			gone = append(gone, rel)
		}
	}
	sort.Strings(gone)
	for _, rel := range gone {
		st.Sources = append(st.Sources, SourceLag{File: rel, Offset: cursors[rel].offset, Missing: true})
	}

	by := map[string]int{}
	rows, err = db.QueryContext(ctx, `SELECT reason, count(*) FROM ingest_errors WHERE at >= ? GROUP BY reason`,
		now.Add(-ErrorsKeepDays*24*time.Hour).UnixMilli())
	if err != nil {
		return err
	}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			_ = rows.Close()
			return err
		}
		by[reason] = n
		st.Errors30d += n
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(by) > 0 {
		st.ErrorsByReason = by
	}
	return db.QueryRowContext(ctx, `SELECT count(*) FROM ingest_errors WHERE at >= ?`,
		now.Add(-24*time.Hour).UnixMilli()).Scan(&st.Errors24h)
}
