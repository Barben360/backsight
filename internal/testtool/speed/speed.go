// Package speed measures how fast the machine running a time budget is,
// with a reference workload that does not depend on backsight's code: a
// fixed SQLite workload (constant schema and rows, through the same pure-Go
// driver the index uses) on an in-memory database. GitHub's Ubuntu runners
// come in speed classes: the dashboard's pages take about 1.8 times longer
// on the slowest ones than on the fastest, on the same code. A budget scaled
// by Factor holds every runner to the same standard.
package speed

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"
)

// Baseline is the reference's best time on the GitHub Ubuntu runners where
// the dashboard's pages are fast (TestPageBudgets' /?machines=all about
// 0.5 s): a runner as fast as those gets Factor 1. Re-measure it when the
// workload, the SQLite driver or the Go version changes: run the bench job
// a few times (`gh run rerun <run> --job <bench job>` draws a new runner
// each time), read the "runner speed" line TestPageBudgets logs, and take
// the reference of the runs whose /?machines=all is in the fast class.
const Baseline = 60 * time.Millisecond

// MaxFactor bounds Factor: a slow runner loosens a budget by half at most,
// and a doubled cost still fails on the slowest runner seen (the pages'
// slow class is 1.8 times the fast one, under the base limit's margin of 3).
const MaxFactor = 1.5

// Measure is the reference's best time on this machine and the factor it
// gives.
type Measure struct {
	Reference time.Duration
	Factor    float64
}

func (m Measure) String() string {
	return fmt.Sprintf("reference %v (baseline %v), factor %.2f", m.Reference.Round(100*time.Microsecond), Baseline, m.Factor)
}

var (
	once     sync.Once
	measured Measure
	errRun   error
)

// Runner is this machine's Measure, taken once per test binary: the best
// of 5 runs of the reference workload (under half a second on a slow
// runner).
func Runner() (Measure, error) {
	once.Do(func() {
		var ref time.Duration
		ref, errRun = reference(5)
		measured = Measure{Reference: ref, Factor: FactorOf(ref, Baseline)}
	})
	return measured, errRun
}

// FactorOf is ref over baseline, never below 1 (a fast machine keeps the
// base limit) and never above MaxFactor.
func FactorOf(ref, baseline time.Duration) float64 {
	if baseline <= 0 {
		return 1
	}
	return min(max(float64(ref)/float64(baseline), 1), MaxFactor)
}

// Scale is limit times f.
func Scale(limit time.Duration, f float64) time.Duration {
	return time.Duration(float64(limit) * f)
}

// The workload's volume: about 60 ms a run on the fast runners.
const (
	rows    = 40_000
	queries = 6
)

// reference builds the fixed database once, then times runs of its
// queries and returns the best.
func reference(runs int) (time.Duration, error) {
	db, err := sql.Open("sqlite", "file:speedref?mode=memory")
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1) // one connection: the in-memory database is its own
	if err := fill(db); err != nil {
		return 0, err
	}
	best := time.Duration(1<<63 - 1)
	for range runs {
		start := time.Now()
		if err := work(db); err != nil {
			return 0, err
		}
		best = min(best, time.Since(start))
	}
	return best, nil
}

// fill writes the constant rows: a deterministic sequence, no randomness.
func fill(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE ev (id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, session TEXT NOT NULL, kind TEXT NOT NULL, n INTEGER, ms INTEGER)`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	st, err := tx.Prepare(`INSERT INTO ev (ts, session, kind, n, ms) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	x := uint32(1)
	for i := range rows {
		x = x*1664525 + 1013904223 // a fixed linear congruential sequence
		kinds := [...]string{"prompt", "reply", "tool", "credit"}
		if _, err := st.Exec(int64(i)*60_000, fmt.Sprintf("sess-%04d", x%2000), kinds[x>>8%4], int(x>>12%50), int(x>>16%3000)); err != nil {
			return err
		}
	}
	if err := st.Close(); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX ev_ts ON ev(ts)`); err != nil {
		return err
	}
	return tx.Commit()
}

// work runs the fixed queries, reading every row they return: grouped
// aggregates, an index range scan, a sort, a join.
func work(db *sql.DB) error {
	qs := [queries]string{
		`SELECT kind, count(*), sum(ms), avg(n) FROM ev GROUP BY kind`,
		`SELECT session, count(*), max(ts) FROM ev GROUP BY session ORDER BY 2 DESC`,
		`SELECT count(*), sum(ms) FROM ev WHERE ts BETWEEN 400000000 AND 1800000000`,
		`SELECT ts / 86400000 AS day, kind, count(*) FROM ev GROUP BY day, kind`,
		`SELECT a.session, count(*) FROM ev a JOIN ev b ON b.id = a.id + 1 AND b.session = a.session GROUP BY a.session`,
		`SELECT id, ms FROM ev WHERE kind = 'tool' ORDER BY ms DESC, id LIMIT 500`,
	}
	for _, q := range qs {
		r, err := db.Query(q)
		if err != nil {
			return err
		}
		cols, err := r.Columns()
		if err != nil {
			_ = r.Close()
			return err
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for r.Next() {
			if err := r.Scan(ptrs...); err != nil {
				_ = r.Close()
				return err
			}
		}
		if err := r.Close(); err != nil {
			return err
		}
	}
	return nil
}
