package index

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Catch-up budgets.
const (
	ViewBudget = 2 * time.Second // block 2's views and block 4's dashboard
	TickBudget = 5 * time.Second
)

// Freshness is how current the index is after a catch-up: when a file was
// last ingested, how far behind its files it is, and whether the budget
// ran out with lines left.
type Freshness struct {
	IngestedAt time.Time `json:"ingested_at"`
	LagSeconds int64     `json:"lag_seconds"`
	Partial    bool      `json:"partial"`
}

// progressMin is the unread size from which a catch-up reports progress.
const progressMin = 1 << 20

// flight is a catch-up in progress on one file in this process.
type flight struct {
	done chan struct{}
	fr   Freshness
	err  error
}

var errCatchUpAborted = errors.New("the catch-up of the metrics index did not finish")

// CatchUp brings the index up to date within budget (by Options.Now; zero
// or less: no time for lines): the month files until the budget runs out
// (a run resumes where the last one stopped), then the ledger table and
// the local_status row. It is single-flight per file in the process: a
// caller arriving during a run waits for it and returns its result,
// computed under the running caller's budget (it may be Partial although
// the waiter's budget would have been enough). A run that ended on its own
// caller's context (cancelled, past its deadline) is not handed on: a
// waiter whose context is alive runs again. A waiter whose context ends
// returns that context's error at once.
func (ix *Index) CatchUp(ctx context.Context, budget time.Duration) (Freshness, error) {
	sh := ix.sh
	for {
		sh.fmu.Lock()
		f := sh.flight
		if f == nil {
			break // still locked: this caller runs
		}
		sh.fmu.Unlock()
		onWait()
		select {
		case <-f.done:
			if ctxErr(f.err) && ctx.Err() == nil {
				continue // the run stopped on its caller's context, not ours
			}
			return f.fr, f.err
		case <-ctx.Done():
			return Freshness{}, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{}), err: errCatchUpAborted}
	sh.flight = f
	sh.fmu.Unlock()
	defer func() {
		sh.fmu.Lock()
		sh.flight = nil
		sh.fmu.Unlock()
		close(f.done)
	}()
	f.fr, f.err = ix.catchUp(ctx, budget)
	return f.fr, f.err
}

// onWait runs when a caller starts waiting for another's run (tests).
var onWait = func() {}

func ctxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (ix *Index) catchUp(ctx context.Context, budget time.Duration) (Freshness, error) {
	start := ix.opts.Now()
	p, err := ix.newProgress(ctx, start)
	if err != nil {
		return Freshness{}, err
	}
	st, err := ix.ingest(ctx, start.Add(max(budget, 0)), p.batch)
	if err != nil {
		return Freshness{}, err
	}
	if err := ix.refreshLedger(ctx); err != nil {
		return Freshness{}, fmt.Errorf("ledger: %w", err)
	}
	if err := ix.refreshLocal(ctx); err != nil {
		return Freshness{}, fmt.Errorf("local status: %w", err)
	}
	fr, err := ix.freshness(ctx, st.More)
	if err != nil {
		return Freshness{}, err
	}
	if !fr.Partial {
		p.ready(st.Lines)
	}
	return fr, nil
}

// Checkpoint copies the WAL into the file and truncates it
// (wal_checkpoint(TRUNCATE)); in DELETE mode there is nothing to do. A
// reader that keeps the checkpoint from finishing is no error: the next
// one finishes it.
func (ix *Index) Checkpoint(ctx context.Context) error {
	if ix.sh.fs.Kind != FSLocal {
		return nil
	}
	var busy, log, done int64
	return ix.sh.w.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &done)
}

// cursorRow is what freshness reads of a cursor.
type cursorRow struct {
	offset     int64
	lastTS     sql.NullInt64
	ingestedAt sql.NullInt64
}

func (ix *Index) cursorRows(ctx context.Context) (map[string]cursorRow, error) {
	out := map[string]cursorRow{}
	err := ix.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT source_file, "offset", last_ts, ingested_at FROM cursors`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var rel string
			var off sql.NullInt64
			var c cursorRow
			if err := rows.Scan(&rel, &off, &c.lastTS, &c.ingestedAt); err != nil {
				return err
			}
			c.offset = off.Int64
			out[rel] = c
		}
		return rows.Err()
	})
	return out, err
}

// freshness: IngestedAt is the latest cursor's ingested_at; a file on
// disk with a complete line past its cursor (or without a cursor) is
// behind. Behind, LagSeconds is now − the newest last_ts of the cursors,
// never below 0, and Partial is more (the deadline stopped the ingest).
// Behind with no last_ts at all (nothing with a time ingested yet), the
// lag cannot be measured: Partial is set whatever more says, so that the
// index never reads as fresh with lines left.
func (ix *Index) freshness(ctx context.Context, more bool) (Freshness, error) {
	var fr Freshness
	srcs, err := ix.sources()
	if err != nil {
		return fr, err
	}
	cs, err := ix.cursorRows(ctx)
	if err != nil {
		return fr, err
	}
	var newest sql.NullInt64
	var ingested int64
	for _, c := range cs {
		if c.ingestedAt.Valid && c.ingestedAt.Int64 > ingested {
			ingested = c.ingestedAt.Int64
			fr.IngestedAt = time.UnixMilli(ingested).UTC()
		}
		if c.lastTS.Valid && (!newest.Valid || c.lastTS.Int64 > newest.Int64) {
			newest = c.lastTS
		}
	}
	behind := false
	for _, s := range srcs {
		if linesLeft(s.Path, cs[s.Rel].offset) {
			behind = true
			break
		}
	}
	if !behind {
		return fr, nil
	}
	if !newest.Valid {
		fr.Partial = true
		return fr, nil
	}
	fr.Partial = more
	fr.LagSeconds = max(ix.opts.Now().UnixMilli()-newest.Int64, 0) / 1000
	return fr, nil
}

// linesLeft reports whether path holds a complete line from off on (from
// 0 when the file is now shorter: it will be read again). A file that
// cannot be read has none: it is recorded as unreadable.
func linesLeft(path string, off int64) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	size := info.Size()
	if size < off {
		off = 0
	}
	br := bufio.NewReaderSize(io.NewSectionReader(f, off, size-off), 64<<10)
	for {
		_, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			return true
		case !errors.Is(err, bufio.ErrBufferFull):
			return false
		}
	}
}

// unread is the bytes of the month files past their cursors.
func (ix *Index) unread(ctx context.Context) (int64, error) {
	srcs, err := ix.sources()
	if err != nil {
		return 0, err
	}
	cs, err := ix.cursorRows(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, s := range srcs {
		info, err := os.Stat(s.Path)
		if err != nil {
			continue
		}
		off := cs[s.Rel].offset
		if info.Size() < off {
			off = 0
		}
		total += info.Size() - off
	}
	return total, nil
}

// progress reports a catch-up of at least progressMin unread bytes: a line
// after the first batch, then at most one a second, and the ready line.
// A nil progress reports nothing.
type progress struct {
	w           io.Writer
	now         func() time.Time
	start, last time.Time
	printed     bool
	done, total int64
}

func (ix *Index) newProgress(ctx context.Context, start time.Time) (*progress, error) {
	if ix.opts.Progress == nil {
		return nil, nil
	}
	total, err := ix.unread(ctx)
	if err != nil || total < progressMin {
		return nil, err
	}
	return &progress{w: ix.opts.Progress, now: ix.opts.Now, start: start, total: total}, nil
}

func mib(n int64) float64 { return float64(n) / (1 << 20) }

func (p *progress) batch(read int64) {
	if p == nil {
		return
	}
	p.done += read
	t := p.now()
	if p.printed && t.Sub(p.last) < time.Second {
		return
	}
	p.printed, p.last = true, t
	_, _ = fmt.Fprintf(p.w, "metrics index: %.1f/%.1f MB\n", mib(min(p.done, p.total)), mib(p.total))
}

func (p *progress) ready(lines int64) {
	if p == nil {
		return
	}
	_, _ = fmt.Fprintf(p.w, "metrics index: ready (%d lines in %.1f s)\n", lines, p.now().Sub(p.start).Seconds())
}
