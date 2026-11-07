package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/machines"
)

// machinesSource is the cache file's name in ingest_errors (relative to
// the state directory, as the month files'): its unreadable record is
// kept by pruneOrphanErrors.
const machinesSource = "metrics/machines.json"

// pcFetch is pc_fetch's row: the cache file as last ingested.
type pcFetch struct {
	fetchedAt, size, mtime int64
	sha                    string
}

// refreshMachines fills the pc_* tables from Options.Machines (nil: not
// refreshed), which reads the cache of the other PCs' daily figures.
//
//   - Sharing off (Options.MachinesOff) and a missing file read as an
//     empty cache, without error: the rows go.
//   - The file's size and modification time are compared with pc_fetch's
//     first, in a read transaction: when both match nothing is read and no
//     write lock is taken.
//   - Otherwise the file is read (Options.Machines); when its fetched_at
//     and SHA-256 are those of pc_fetch only the size and time are
//     updated, else the four tables are replaced in one transaction.
//   - A cache that does not load keeps the rows and is recorded once
//     (machines_unreadable) until a good read clears it.
//
// Every document is parsed again, at the index's clock; the tables hold
// only what machines.Parse returns (G-untrusted), never a document's text.
func (ix *Index) refreshMachines(ctx context.Context) error {
	if ix.opts.Machines == nil {
		return nil
	}
	var size, mtime int64
	present := false
	if ix.opts.MachinesOff == nil || !ix.opts.MachinesOff() {
		fi, err := os.Stat(machines.CacheFile(ix.opts.Dirs.MetricsDir()))
		switch {
		case err == nil && fi.Mode().IsRegular():
			size, mtime, present = fi.Size(), fi.ModTime().UnixNano(), true
		case err == nil || errors.Is(err, fs.ErrNotExist):
		default:
			return ix.recordOnce(ctx, machinesSource, 0, errMachinesUnreadable)
		}
	}

	var old pcFetch
	have, flagged := false, false
	if err := ix.ReadTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT fetched_at, size, mtime, sha256 FROM pc_fetch WHERE id = 1`).
			Scan(&old.fetchedAt, &old.size, &old.mtime, &old.sha)
		switch {
		case err == nil:
			have = true
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		flagged, err = hasErrorRow(ctx, tx, machinesSource, 0)
		return err
	}); err != nil {
		return err
	}

	if !present { // an empty cache
		if !have && !flagged {
			return nil
		}
		return ix.replaceMachines(ctx, nil, pcFetch{}, false)
	}
	if have && !flagged && old.size == size && old.mtime == mtime {
		return nil
	}

	c, err := ix.opts.Machines()
	if errors.Is(err, fs.ErrNotExist) { // gone since the stat
		if !have && !flagged {
			return nil
		}
		return ix.replaceMachines(ctx, nil, pcFetch{}, false)
	}
	if err != nil {
		return ix.recordOnce(ctx, machinesSource, 0, errMachinesUnreadable)
	}
	canon, err := json.Marshal(c) // the content, whatever the file's spacing
	if err != nil {
		return ix.recordOnce(ctx, machinesSource, 0, errMachinesUnreadable)
	}
	sum := sha256.Sum256(canon)
	now := pcFetch{fetchedAt: c.FetchedAt.UnixMilli(), size: size, mtime: mtime, sha: hex.EncodeToString(sum[:])}
	if have && !flagged && old.fetchedAt == now.fetchedAt && old.sha == now.sha {
		// The same content under another modification time: remember it.
		return ix.writeTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE pc_fetch SET size = ?, mtime = ? WHERE id = 1`, now.size, now.mtime)
			return err
		})
	}
	return ix.replaceMachines(ctx, &c, now, true)
}

// writeTx runs f in a write transaction and commits it.
func (ix *Index) writeTx(ctx context.Context, f func(tx *sql.Tx) error) error {
	tx, err := ix.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// replaceMachines replaces the four tables in one transaction with c (nil:
// empty, and pc_fetch gone) and clears the unreadable record.
func (ix *Index) replaceMachines(ctx context.Context, c *machines.Cache, f pcFetch, withFetch bool) error {
	return ix.writeTx(ctx, func(tx *sql.Tx) error {
		for _, tbl := range []string{"pc_fetch", "pc_docs", "pc_days", "pc_ignored"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+tbl); err != nil {
				return err
			}
		}
		if _, err := clearError(ctx, tx, machinesSource); err != nil {
			return err
		}
		if !withFetch || c == nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pc_fetch(id, fetched_at, size, mtime, sha256, ingested_at) VALUES (1, ?, ?, ?, ?, ?)`,
			f.fetchedAt, f.size, f.mtime, f.sha, ix.opts.Now().UnixMilli()); err != nil {
			return err
		}
		return ix.insertMachines(ctx, tx, *c)
	})
}

// reasonOf is the closed reason of an ignored document, else not_valid.
func reasonOf(r machines.Reason) string {
	switch r {
	case machines.TooLarge, machines.UnknownVersion, machines.NotValid:
		return string(r)
	}
	return string(machines.NotValid)
}

// shortID is s when it is a short machine id (8 lower-case hex digits) or
// empty, else empty.
func shortID(s string) string {
	if len(s) != 8 {
		return ""
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return s
}

const pcDaysCols = `machine, day, version, noon, prompts, hit_n, hit_d, used_n, used_d, cov_n, cov_d, wrong_n, wrong_d,
	ctx_chars, ctx_sessions, flow_n, flow_d, lat0, lat1, lat2, lat3, lat4, lat5, lat6, lat7, lat8`

// insertMachines parses the cached documents at the index's clock and
// writes what they hold. The cache's own ignored entries come first, in
// order, then the documents that fail now, and those whose name came
// before (the first one is kept).
func (ix *Index) insertMachines(ctx context.Context, tx *sql.Tx, c machines.Cache) error {
	ign, err := tx.PrepareContext(ctx, `INSERT INTO pc_ignored(pos, machine, reason) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = ign.Close() }()
	pos := 0
	ignore := func(machine, reason string) error {
		_, err := ign.ExecContext(ctx, pos, machine, reason)
		pos++
		return err
	}
	for _, g := range c.Ignored {
		if err := ignore(shortID(g.Machine), reasonOf(g.Reason)); err != nil {
			return err
		}
	}

	doc, err := tx.PrepareContext(ctx, `INSERT INTO pc_docs(machine, os, first_seen, at, binary_version, until_day) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = doc.Close() }()
	day, err := tx.PrepareContext(ctx, `INSERT INTO pc_days(`+pcDaysCols+`) VALUES (?`+repeatArgs(25)+`)`)
	if err != nil {
		return err
	}
	defer func() { _ = day.Close() }()

	now := ix.opts.Now()
	seen := map[string]bool{}
	for _, cd := range c.Docs {
		if seen[cd.Name] {
			if err := ignore(machines.Short(cd.Name), string(machines.NotValid)); err != nil {
				return err
			}
			continue
		}
		seen[cd.Name] = true
		d, err := machines.Parse(cd.Name, cd.Raw, now)
		if err != nil {
			reason := machines.NotValid
			var pe *machines.ParseError
			if errors.As(err, &pe) {
				reason = machines.Reason(reasonOf(pe.Reason))
			}
			if err := ignore(machines.Short(cd.Name), string(reason)); err != nil {
				return err
			}
			continue
		}
		if _, err := doc.ExecContext(ctx, d.Machine, d.OS, d.FirstSeen, d.At.UnixMilli(), d.BinaryVersion, d.Until); err != nil {
			return err
		}
		for _, dd := range d.Days {
			t, err := time.Parse("2006-01-02", dd.Day)
			if err != nil {
				return err // Parse checked it
			}
			noon := t.Add(12 * time.Hour).UnixMilli()
			if _, err := day.ExecContext(ctx, dayArgs(d.Machine, dd.Day, "", noon, dd.Figures, &dd.Flow)...); err != nil {
				return err
			}
			vs := make([]string, 0, len(dd.Versions))
			for v := range dd.Versions {
				vs = append(vs, v)
			}
			slices.Sort(vs)
			for _, v := range vs {
				if _, err := day.ExecContext(ctx, dayArgs(d.Machine, dd.Day, v, noon, dd.Versions[v], nil)...); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func repeatArgs(n int) string {
	b := make([]byte, 0, n*3)
	for i := 0; i < n; i++ {
		b = append(b, ',', ' ', '?')
	}
	return string(b)
}

// dayArgs are pc_days's values in pcDaysCols' order; flow nil: NULLs.
func dayArgs(machine, day, version string, noon int64, f machines.Figures, flow *machines.Pair) []any {
	a := []any{machine, day, version, noon, f.Prompts, f.Hit[0], f.Hit[1], f.Used[0], f.Used[1], f.Coverage[0], f.Coverage[1],
		f.Wrong[0], f.Wrong[1], f.Context[0], f.Context[1], nil, nil}
	if flow != nil {
		a[15], a[16] = flow[0], flow[1]
	}
	for _, l := range f.Latency {
		a = append(a, l)
	}
	return a
}
