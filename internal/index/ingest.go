package index

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

// Ingest limits: lines per transaction, bytes of a file's head checked
// for a replacement, bytes of a line (a longer one is skipped as too_long),
// lines between two looks at the context.
const (
	maxBatchLines = 5000
	headMax       = 4096
	maxLineBytes  = 1 << 20
	ctxEvery      = 64
)

// maxBatchBytes bounds the bytes one transaction reads (it ends after the
// line that reaches it), so that a batch of long lines does not hold the
// write lock for long; a var for the tests.
var maxBatchBytes int64 = 64 << 20

// kindTelemetry is the cursors kind of the telemetry month files.
const kindTelemetry = "telemetry"

// source is one month file (Kind: telemetry | journal): Rel, its path from the state directory with
// forward slashes, is the source_file of its rows and cursor.
type source struct{ Rel, Path, Kind string }

// lineFunc maps one complete line, at off in the file rel, to its rows
// and its time (unix ms), or to an ingest_errors reason.
type lineFunc func(rel string, off int64, line []byte) ([]row, int64, string)

// sourceKind is a kind of month file: where it lives, the prefix of its
// names (then YYYY-MM.jsonl) and how its lines map to rows. The order is
// the order of a month's files in sources().
type sourceKind struct {
	kind, prefix string
	dir          func(paths.Dirs) string
	rows         lineFunc
}

var sourceKinds = []sourceKind{
	{kind: kindTelemetry, prefix: "events-", dir: paths.Dirs.TelemetryDir, rows: telemetryRows},
	{kind: kindJournal, prefix: "journal-", dir: paths.Dirs.FeedbackDir, rows: journalRows},
}

func kindOf(kind string) (sourceKind, bool) {
	for _, k := range sourceKinds {
		if k.kind == kind {
			return k, true
		}
	}
	return sourceKind{}, false
}

// rowTables holds every table whose rows come from a source line, keyed
// by source_file: a reset deletes a file's rows from each, and the
// pruning of old rows walks them.
var rowTables = []string{
	"tel_prompt", "tel_outcome", "tel_reply", "tel_briefing", "tel_briefing_outcome",
	"tel_tool", "tel_tool_use", "tel_credit", "tel_credit_check", "tel_retire",
	"tel_feedback_settle", "tel_feedback_error", "tel_gate", "tel_retain",
	"tel_hook_failure", "tel_job", "tel_attribution", "tel_store", "tel_dashboard",
	"tel_config", "tel_ids", "tel_prompt_scopes", "tel_config_values", "events",
	"turns", "shows", "settles", "credits", "stops", "retirements", "recalls",
	"imports", "journal_other",
}

// afterBatch runs after each committed transaction (tests).
var afterBatch = func() {}

var monthRE = regexp.MustCompile(`^(\d{4})-(0[1-9]|1[0-2])\.jsonl$`)

// sources lists the month files on disk: exact names only, newest month
// first, and within a month in sourceKinds order.
func (ix *Index) sources() ([]source, error) {
	type item struct {
		s     source
		month string
		order int
	}
	var items []item
	state := ix.opts.Dirs.State
	for order, k := range sourceKinds {
		dir := k.dir(ix.opts.Dirs)
		ents, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			name := e.Name()
			rest, ok := strings.CutPrefix(name, k.prefix)
			if !ok || !monthRE.MatchString(rest) || !e.Type().IsRegular() {
				continue
			}
			path := filepath.Join(dir, name)
			rel, err := filepath.Rel(state, path)
			if err != nil {
				return nil, err
			}
			items = append(items, item{source{Rel: filepath.ToSlash(rel), Path: path, Kind: k.kind}, strings.TrimSuffix(rest, ".jsonl"), order})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].month != items[j].month {
			return items[i].month > items[j].month
		}
		return items[i].order < items[j].order
	})
	out := make([]source, len(items))
	for i, it := range items {
		out[i] = it.s
	}
	return out, nil
}

// ingestStats: complete lines read, those skipped as errors, the files
// that could not be read, and More when the deadline stopped the ingest
// before every file was caught up.
type ingestStats struct {
	Lines, Errors, Unreadable int64
	More                      bool
}

// fileError is a failure to read one source file (permissions, I/O, a
// kind this binary does not know): that file is skipped, the others go
// on. Errors of the index itself are not fileErrors.
type fileError struct{ err error }

func (e *fileError) Error() string { return e.err.Error() }
func (e *fileError) Unwrap() error { return e.err }

// unreadableOffset is the "offset" of a file's own ingest_errors row (no
// line has a negative offset): one per file while it cannot be read,
// removed by its next successful read or a reset.
const unreadableOffset = -1

// ingestSources brings every source up to date, one transaction per
// batch of at most maxBatchLines lines. A zero deadline is none; past it
// (by Options.Now), the ingest stops between batches with More.
func (ix *Index) ingestSources(ctx context.Context, deadline time.Time) (ingestStats, error) {
	return ix.ingest(ctx, deadline, nil)
}

// ingest is ingestSources, calling onBatch (when not nil) with the bytes
// each committed batch read.
func (ix *Index) ingest(ctx context.Context, deadline time.Time, onBatch func(read int64)) (ingestStats, error) {
	var st ingestStats
	srcs, err := ix.sources()
	if err != nil {
		return st, err
	}
	gone, err := ix.vanished(ctx, srcs)
	if err != nil {
		return st, err
	}
	for _, s := range append(srcs, gone...) {
		for {
			if err := ctx.Err(); err != nil {
				return st, err
			}
			if !deadline.IsZero() && !ix.opts.Now().Before(deadline) {
				st.More = true
				return st, nil
			}
			b, err := ix.ingestBatch(ctx, s)
			if fe := (*fileError)(nil); errors.As(err, &fe) {
				st.Unreadable++
				if err := ix.recordUnreadable(ctx, s.Rel); err != nil {
					return st, fmt.Errorf("ingest %s: %w", s.Rel, err)
				}
				break
			}
			if err != nil {
				return st, fmt.Errorf("ingest %s: %w", s.Rel, err)
			}
			st.Lines += b.lines
			st.Errors += b.errors
			if onBatch != nil && b.read > 0 {
				onBatch(b.read)
			}
			if !b.full {
				break
			}
		}
	}
	return st, nil
}

// vanished are the files with a cursor, not yet marked missing, that
// sources() no longer lists.
func (ix *Index) vanished(ctx context.Context, listed []source) ([]source, error) {
	seen := map[string]bool{}
	for _, s := range listed {
		seen[s.Rel] = true
	}
	var out []source
	err := ix.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT source_file, kind FROM cursors WHERE missing = 0 ORDER BY source_file`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var s source
			if err := rows.Scan(&s.Rel, &s.Kind); err != nil {
				return err
			}
			if !seen[s.Rel] {
				s.Path = filepath.Join(ix.opts.Dirs.State, filepath.FromSlash(s.Rel))
				out = append(out, s)
			}
		}
		return rows.Err()
	})
	return out, err
}

// recordUnreadable adds rel's unreadable row unless it is there already:
// a file that stays unreadable writes nothing on later runs.
func (ix *Index) recordUnreadable(ctx context.Context, rel string) error {
	return ix.recordOnce(ctx, rel, unreadableOffset, errUnreadable)
}

// recordOnce adds the ingest_errors row (rel, off) unless there is one;
// one already there is seen from a read, without the write lock.
func (ix *Index) recordOnce(ctx context.Context, rel string, off int64, reason string) error {
	var there bool
	if err := ix.ReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		there, err = hasErrorRow(ctx, tx, rel, off)
		return err
	}); err != nil || there {
		return err
	}
	tx, err := ix.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO ingest_errors(source_file, "offset", reason, at) VALUES (?, ?, ?, ?)
		ON CONFLICT(source_file, "offset") DO NOTHING`, rel, off, reason, ix.opts.Now().UnixMilli())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	return ix.commit(tx)
}

// hasErrorRow reports whether ingest_errors holds the row (rel, off).
func hasErrorRow(ctx context.Context, tx *sql.Tx, rel string, off int64) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM ingest_errors WHERE source_file = ? AND "offset" = ?`, rel, off).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// cursor is a file's row in cursors.
type cursor struct {
	size, offset, headLen int64
	headSHA               string
	lastTS                sql.NullInt64
	missing               bool
}

func readCursor(ctx context.Context, tx *sql.Tx, rel string) (cursor, bool, error) {
	var c cursor
	var size, offset, headLen sql.NullInt64
	var sha sql.NullString
	var missing int64
	err := tx.QueryRowContext(ctx, `SELECT size, "offset", head_len, head_sha256, last_ts, missing FROM cursors WHERE source_file = ?`, rel).
		Scan(&size, &offset, &headLen, &sha, &c.lastTS, &missing)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	c.size, c.offset, c.headLen, c.headSHA, c.missing = size.Int64, offset.Int64, headLen.Int64, sha.String, missing != 0
	return c, true, nil
}

// batch is what one transaction did: its lines, those skipped as
// errors, and the bytes read.
type batch struct {
	lines, errors, read int64
	full                bool // a batch limit reached: there may be more
}

// ingestBatch is one transaction on one file: the cursor read under the
// write lock, a reset when the file was cut below the cursor or its head
// changed, up to maxBatchLines complete lines, the cursor advanced. A
// file gone keeps its rows and is marked missing. Nothing changed:
// nothing is written.
func (ix *Index) ingestBatch(ctx context.Context, s source) (batch, error) {
	var b batch
	k, ok := kindOf(s.Kind)
	if !ok {
		return b, &fileError{fmt.Errorf("unknown source kind %q", s.Kind)}
	}
	if same, err := ix.unchanged(ctx, s); err != nil || same {
		return b, err
	}
	// Opened before the write lock: a file that cannot be read takes none.
	f, err := os.Open(s.Path)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return b, &fileError{err}
	}
	if f != nil {
		defer func() { _ = f.Close() }()
	}
	tx, err := ix.beginWrite(ctx)
	if err != nil {
		return b, err
	}
	defer func() { _ = tx.Rollback() }()
	cur, known, err := readCursor(ctx, tx, s.Rel)
	if err != nil {
		return b, err
	}
	gone := func() (batch, error) {
		if !known || cur.missing {
			return b, nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cursors SET missing = 1 WHERE source_file = ?`, s.Rel); err != nil {
			return b, err
		}
		return b, ix.commit(tx)
	}
	if missing {
		return gone()
	}
	info, err := f.Stat()
	if err != nil {
		return b, &fileError{err}
	}
	if !info.Mode().IsRegular() { // a directory now holds its name
		return gone()
	}
	size := info.Size()
	head := make([]byte, min(headMax, size))
	n, err := f.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return b, &fileError{err}
	}
	if n < len(head) { // cut while we looked
		head, size = head[:n], int64(n)
	}

	// No cursor is a reset too: rows of an earlier copy of the file may
	// have outlived its cursor (its ingest errors are kept longer than
	// the prune keeps a cursor of a file gone), and would collide.
	reset := !known || size < cur.offset || cur.headLen > int64(len(head)) || hexSHA(head[:cur.headLen]) != cur.headSHA
	if reset {
		for _, t := range slices.Concat(rowTables, []string{"ingest_errors"}) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE source_file = ?`, s.Rel); err != nil {
				return b, err
			}
		}
		cur = cursor{}
	}

	ins := inserter{tx: tx, stmts: map[string]*sql.Stmt{}}
	defer ins.close()
	lastTS := cur.lastTS
	now := ix.opts.Now().UnixMilli()
	end, lines, more, err := readLines(ctx, io.NewSectionReader(f, cur.offset, size-cur.offset), cur.offset, maxBatchLines, maxBatchBytes,
		func(off int64, line []byte, tooLong bool) error {
			reason := errTooLong
			var rows []row
			var ts int64
			if !tooLong {
				rows, ts, reason = k.rows(s.Rel, off, line)
			}
			if reason != "" {
				b.errors++
				return ins.exec(ctx, `INSERT INTO ingest_errors(source_file, "offset", reason, at) VALUES (?, ?, ?, ?)
					ON CONFLICT(source_file, "offset") DO NOTHING`, s.Rel, off, reason, now)
			}
			if !lastTS.Valid || ts > lastTS.Int64 {
				lastTS = sql.NullInt64{Int64: ts, Valid: true}
			}
			for _, r := range rows {
				if err := ins.row(ctx, r); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return b, err // a fileError from a read, else the index's
	}
	b.lines, b.read, b.full = int64(lines), end-cur.offset, more
	cleared := false // the file was unreadable until now
	if !reset {
		res, err := tx.ExecContext(ctx, `DELETE FROM ingest_errors WHERE source_file = ? AND "offset" = ?`, s.Rel, unreadableOffset)
		if err != nil {
			return b, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return b, err
		}
		cleared = n > 0
	}

	headLen := int64(len(head))
	if known && !reset && !cleared && lines == 0 && !cur.missing && cur.size == size && cur.headLen == headLen {
		return b, nil // nothing new: no write
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO cursors(source_file, kind, size, "offset", head_len, head_sha256, ingested_at, last_ts, missing)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(source_file) DO UPDATE SET kind = excluded.kind, size = excluded.size, "offset" = excluded."offset",
			head_len = excluded.head_len, head_sha256 = excluded.head_sha256, ingested_at = excluded.ingested_at,
			last_ts = excluded.last_ts, missing = 0`,
		s.Rel, s.Kind, size, end, headLen, hexSHA(head), now, lastTS)
	if err != nil {
		return b, err
	}
	return b, ix.commit(tx)
}

// unchanged reports, from a read alone, that ingestBatch has nothing to
// write for s: the file matches its cursor (size and head), no complete
// line lies past the offset and no unreadable row waits to be cleared;
// or the file is gone and marked missing already (or never had a
// cursor). A catch-up with nothing new so takes no write lock. Anything
// else goes to the write transaction, which looks again under the lock;
// a file that cannot be read is left to it too (its fileError).
func (ix *Index) unchanged(ctx context.Context, s source) (bool, error) {
	var cur cursor
	var known, flagged bool
	if err := ix.ReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		if cur, known, err = readCursor(ctx, tx, s.Rel); err != nil {
			return err
		}
		flagged, err = hasErrorRow(ctx, tx, s.Rel, unreadableOffset)
		return err
	}); err != nil {
		return false, err
	}
	info, err := os.Stat(s.Path)
	if errors.Is(err, fs.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		return !known || cur.missing, nil
	}
	if err != nil || !known || cur.missing || flagged || info.Size() != cur.size || cur.headLen > cur.size {
		return false, nil
	}
	f, err := os.Open(s.Path)
	if err != nil {
		return false, nil
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, cur.headLen)
	if n, err := f.ReadAt(head, 0); n != len(head) || err != nil && !errors.Is(err, io.EOF) {
		return false, nil
	}
	if hexSHA(head) != cur.headSHA {
		return false, nil
	}
	return !hasLine(f, cur.offset, cur.size), nil
}

func (ix *Index) commit(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	afterBatch()
	return nil
}

func hexSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// readLines calls fn for each complete line of r (newline excluded), at
// most limit of them, ending after the line that brings the bytes read to
// maxBytes; base is r's offset in the file. A line over maxLineBytes
// comes with tooLong and no bytes. A last line without its newline is
// left for later. end is the offset after the last complete line; more,
// that a limit stopped the read. The context is looked at every ctxEvery
// lines. A read error is a fileError; fn's errors and the context's come
// back as they are.
func readLines(ctx context.Context, r io.Reader, base int64, limit int, maxBytes int64, fn func(off int64, line []byte, tooLong bool) error) (end int64, n int, more bool, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	pos, start := base, base
	end = base
	var buf []byte
	tooLong := false
	for {
		if n == limit || end-base >= maxBytes {
			return end, n, true, nil
		}
		if n%ctxEvery == 0 && n > 0 {
			if err := ctx.Err(); err != nil {
				return end, n, false, err
			}
		}
		chunk, rerr := br.ReadSlice('\n')
		pos += int64(len(chunk))
		if !tooLong {
			if len(buf)+len(chunk) > maxLineBytes+1 { // +1: the newline
				tooLong, buf = true, buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case rerr == nil:
			var line []byte
			if !tooLong {
				line = buf[:len(buf)-1]
			}
			if err := fn(start, line, tooLong); err != nil {
				return end, n, false, err
			}
			n++
			start, end = pos, pos
			buf, tooLong = buf[:0], false
		case errors.Is(rerr, bufio.ErrBufferFull):
		case errors.Is(rerr, io.EOF):
			return end, n, false, nil
		default:
			return end, n, false, &fileError{rerr}
		}
	}
}

// inserter keeps one prepared statement per statement text for the
// transaction.
type inserter struct {
	tx    *sql.Tx
	stmts map[string]*sql.Stmt
}

func (in *inserter) exec(ctx context.Context, q string, args ...any) error {
	st, ok := in.stmts[q]
	if !ok {
		var err error
		if st, err = in.tx.PrepareContext(ctx, q); err != nil {
			return err
		}
		in.stmts[q] = st
	}
	_, err := st.ExecContext(ctx, args...)
	return err
}

func (in *inserter) row(ctx context.Context, r row) error {
	q := "INSERT INTO " + r.table + "(" + strings.Join(r.cols, ", ") + ") VALUES (?" + strings.Repeat(", ?", len(r.cols)-1) + ")"
	return in.exec(ctx, q, r.vals...)
}

func (in *inserter) close() {
	for _, st := range in.stmts {
		_ = st.Close()
	}
}
