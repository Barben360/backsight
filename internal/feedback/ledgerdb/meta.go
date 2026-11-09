package ledgerdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/Barben360/backsight/internal/feedback"
)

// queryer is a transaction or a connection pool.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// readCursors is ledger_cursors: each month file's base name to the
// offset its folded lines end at.
func readCursors(ctx context.Context, q queryer) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT file, offset FROM ledger_cursors`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cursors := map[string]int64{}
	for rows.Next() {
		var f string
		var off int64
		if err := rows.Scan(&f, &off); err != nil {
			return nil, err
		}
		cursors[f] = off
	}
	return cursors, rows.Err()
}

func writeCursors(ctx context.Context, tx *sql.Tx, ends map[string]int64) error {
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO ledger_cursors(file, offset) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, name := range slices.Sorted(maps.Keys(ends)) {
		if _, err := stmt.ExecContext(ctx, name, ends[name]); err != nil {
			return err
		}
	}
	return nil
}

// meta is the meta table, read whole. importsCut is
// imports_dropped_before_ns: a rebuild without imports drops every
// import journaled at or before its start, and the folds that follow
// keep them out (a later import applies); 0, the key absent, drops none.
type meta struct {
	built           bool // a generation row: folded once
	watermark       int64
	foldedAt        int64 // folded_at_ns, read only (write takes it)
	historyImported int64
	params          string
	importsCut      int64
	generation      int64
	buildID         string
	exported        int64
}

// dropImports is events without the import events (an import-complete
// mark aside, as feedback.DropImports) journaled at or before cut, unix
// ns; cut 0 drops none.
func dropImports(events []feedback.Event, cut int64) []feedback.Event {
	if cut == 0 {
		return events
	}
	return slices.DeleteFunc(slices.Clone(events), func(e feedback.Event) bool {
		return e.Kind == feedback.KindImport && e.Outcome != feedback.ImportComplete && toNS(e.TS) <= cut
	})
}

func readMeta(ctx context.Context, q queryer) (meta, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return meta{}, err
	}
	defer func() { _ = rows.Close() }()
	var m meta
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return meta{}, err
		}
		n := func(dst *int64) error {
			var err error
			if *dst, err = strconv.ParseInt(v, 10, 64); err != nil {
				return fmt.Errorf("meta %s: %w", k, err)
			}
			return nil
		}
		switch k {
		case "watermark_ns":
			err = n(&m.watermark)
		case "folded_at_ns":
			err = n(&m.foldedAt)
		case "history_imported_ns":
			err = n(&m.historyImported)
		case "generation":
			m.built, err = true, n(&m.generation)
		case "exported_generation":
			err = n(&m.exported)
		case "params":
			m.params = v
		case "imports_dropped_before_ns":
			err = n(&m.importsCut)
		case "build_id":
			m.buildID = v
		}
		if err != nil {
			return meta{}, err
		}
	}
	return m, rows.Err()
}

// write writes every key a fold owns (exported_generation is the
// export's), folded_at_ns as foldedAt; imports_dropped_before_ns only
// when set.
func (m meta) write(ctx context.Context, tx *sql.Tx, foldedAt int64) error {
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	itoa := func(n int64) string { return strconv.FormatInt(n, 10) }
	for _, kv := range [][2]string{
		{"watermark_ns", itoa(m.watermark)},
		{"folded_at_ns", itoa(foldedAt)},
		{"history_imported_ns", itoa(m.historyImported)},
		{"params", m.params},
		{"generation", itoa(m.generation)},
		{"build_id", m.buildID},
	} {
		if _, err := stmt.ExecContext(ctx, kv[0], kv[1]); err != nil {
			return err
		}
	}
	if m.importsCut == 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM meta WHERE key = 'imports_dropped_before_ns'`)
		return err
	}
	_, err = stmt.ExecContext(ctx, "imports_dropped_before_ns", itoa(m.importsCut))
	return err
}

// paramsJSON is p as meta.params records it: the JSON of p.Record(), its
// keys sorted.
func paramsJSON(p feedback.Params) string {
	b, err := json.Marshal(p.Record())
	if err != nil { // a half-life JSON cannot hold (NaN, ±Inf): no Params gives one
		return ""
	}
	return string(b)
}
