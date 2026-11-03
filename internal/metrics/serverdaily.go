package metrics

import (
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// The daily record of the server (server_day, server_day_llm): one UTC day
// per line, the latest line of a day winning. Metrics read the server's own
// series first and this record for the days the server no longer holds.

// The days back from now the server's figures reach: the memories series
// (maxSeriesDays) and the LLM stats.
const llmReachDays = 30

// Sources of a day's server figures, as Point.Source says.
const (
	sourceServer = "server"
	sourceRecord = "record"
	sourceNone   = "none"
)

// recordedNew is the memories the server created on a UTC day, by kind, as
// the record holds them.
type recordedNew struct {
	Bucket                         time.Time
	World, Experience, Observation int64
}

func (r recordedNew) total() int64 { return r.World + r.Experience + r.Observation }

// recordedLLM is the tokens of one operation on a UTC day, as the record
// holds them ("all" is every operation).
type recordedLLM struct {
	Bucket time.Time
	Op     string
	T      tokens
}

// readRecordedNew is the days recorded with a memories count in [fromMs,
// toMs) (unix ms), oldest first; a kind the line lacks counts 0, a line
// without any kind is no figure.
func readRecordedNew(e env, fromMs, toMs int64) ([]recordedNew, error) {
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT bucket, COALESCE(world, 0), COALESCE(experience, 0), COALESCE(observation, 0) FROM server_day
WHERE bucket >= ? AND bucket < ? AND NOT (world IS NULL AND experience IS NULL AND observation IS NULL)
ORDER BY bucket`, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []recordedNew
	for rows.Next() {
		var b int64
		r := recordedNew{}
		if err := rows.Scan(&b, &r.World, &r.Experience, &r.Observation); err != nil {
			return nil, err
		}
		r.Bucket = time.UnixMilli(b).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// readRecordedLLM is the tokens recorded for ops in [fromMs, toMs), by day
// then operation. Output holds the thoughts, as tokens.add reads them.
func readRecordedLLM(e env, fromMs, toMs int64, ops []string) ([]recordedLLM, error) {
	args := []any{fromMs, toMs}
	for _, op := range ops {
		args = append(args, op)
	}
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT d.bucket, l.operation, COALESCE(l.input, 0), COALESCE(l.cached, 0), COALESCE(l.output, 0), COALESCE(l.thoughts, 0)
FROM server_day d JOIN server_day_llm l ON l.source_file = d.source_file AND l."offset" = d."offset"
WHERE d.bucket >= ? AND d.bucket < ? AND l.operation IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ops)), ",")+`)
ORDER BY d.bucket, l.operation`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []recordedLLM
	for rows.Next() {
		var b, thoughts int64
		r := recordedLLM{}
		if err := rows.Scan(&b, &r.Op, &r.T.input, &r.T.cached, &r.T.output, &thoughts); err != nil {
			return nil, err
		}
		r.Bucket, r.T.output = time.UnixMilli(b).UTC(), r.T.output+thoughts
		out = append(out, r)
	}
	return out, rows.Err()
}

// recordFrom is the first day the record holds a figure for, as a date: a
// memories count, or with llm an operation's tokens; nil when it holds none.
func recordFrom(e env, llm bool) (*string, error) {
	q := `SELECT MIN(bucket) FROM server_day WHERE NOT (world IS NULL AND experience IS NULL AND observation IS NULL)`
	if llm {
		q = `SELECT MIN(d.bucket) FROM server_day d WHERE EXISTS (
SELECT 1 FROM server_day_llm l WHERE l.source_file = d.source_file AND l."offset" = d."offset")`
	}
	var ms *int64
	if err := e.tx.QueryRowContext(e.ctx, q).Scan(&ms); err != nil {
		return nil, err
	}
	if ms == nil {
		return nil, nil
	}
	return ptr(time.UnixMilli(*ms).UTC().Format(dateLayout)), nil
}

// reachStart is the first instant a server period of days back from now
// holds: the start of the UTC day it falls in.
func reachStart(now time.Time, days int) time.Time {
	return now.Add(-time.Duration(days) * day).UTC().Truncate(day)
}

// recordFromMs is the first unix ms of a UTC day that overlaps a window
// starting at since (a day straddling the start counts whole).
func recordFromMs(since time.Time) int64 { return since.Add(-day).UnixMilli() + 1 }

// firstBucket is the earliest of bs's times overlapping [since, until)
// (buckets width wide); false when none does.
func firstBucket(times []time.Time, width time.Duration, since, until time.Time) (time.Time, bool) {
	var first time.Time
	found := false
	for _, t := range times {
		if !t.Add(width).After(since) || !t.Before(until) {
			continue
		}
		if !found || t.Before(first) {
			first, found = t, true
		}
	}
	return first, found
}

// sourceOf is the source of the day with key: the server's from its first
// day on, else the record's when it holds the day, else none.
func sourceOf(key string, w Window, server bool, serverFrom time.Time, recorded bool) string {
	switch {
	case server && key >= serverFrom.In(w.Loc).Format(dateLayout):
		return sourceServer
	case recorded:
		return sourceRecord
	}
	return sourceNone
}

// recordTokens are the record's tokens over the buckets of a window, for
// the days before the server's first one.
type recordTokens struct {
	byOp  map[string][]tokens // "" is every operation; one slice per bucket
	total map[string]tokens
	held  []bool // per bucket: the record holds the day
	days  int    // distinct local days held
}

// readRecordTokens reads ops (and every operation, ""), for the UTC days
// overlapping the window and before cutoff, placed in buckets as inWindow
// places the server's: by start instant, a day straddling the start in the
// first.
func readRecordTokens(e env, w Window, buckets []Bucket, cutoff time.Time, ops []string) (recordTokens, error) {
	rt := recordTokens{byOp: map[string][]tokens{}, total: map[string]tokens{}, held: make([]bool, len(buckets))}
	for _, op := range append([]string{""}, ops...) {
		rt.byOp[op] = make([]tokens, len(buckets))
	}
	recOps := []string{"all"}
	recOps = append(recOps, ops...)
	rows, err := readRecordedLLM(e, recordFromMs(w.Since), min2(cutoff, w.Until).UnixMilli(), recOps)
	if err != nil {
		return rt, err
	}
	seen := map[string]bool{}
	for _, r := range rows {
		local := r.Bucket.In(w.Loc)
		if local.Before(w.Since) {
			local = w.Since.In(w.Loc)
		}
		i := bucketIndex(buckets, local)
		if i < 0 {
			continue
		}
		op := r.Op
		if op == "all" {
			op = ""
		}
		t := rt.byOp[op][i]
		t.input, t.cached, t.output = t.input+r.T.input, t.cached+r.T.cached, t.output+r.T.output
		rt.byOp[op][i] = t
		s := rt.total[op]
		s.input, s.cached, s.output = s.input+r.T.input, s.cached+r.T.cached, s.output+r.T.output
		rt.total[op] = s
		rt.held[i] = true
		seen[r.Bucket.Format(dateLayout)] = true
	}
	rt.days = len(seen)
	return rt, nil
}

// llmTimes are the times of bs.
func llmTimes(bs []hindsight.LLMBucket) []time.Time {
	out := make([]time.Time, len(bs))
	for i, b := range bs {
		out[i] = b.Time
	}
	return out
}

// recordedExtraction is the record's tokens for the window's days before
// the server's first LLM day, and each bucket's source; nil, nil when the
// window lies inside the server's 30 days (the server alone, as ever).
// all is the server's stats of every operation; named, the operations that
// could be read (only those get the record's figures).
func recordedExtraction(e env, w Window, buckets []Bucket, all []hindsight.LLMBucket, named map[string]tokens) (*recordTokens, []string, error) {
	if !w.Since.Before(reachStart(e.now, llmReachDays)) {
		return nil, nil, nil
	}
	first, hasServer := firstBucket(llmTimes(all), llmWidth(all), w.Since, w.Until)
	cutoff := w.Until
	if hasServer {
		cutoff = first
	}
	var ops []string
	for _, op := range extractionOps {
		if _, ok := named[op]; ok {
			ops = append(ops, op)
		}
	}
	rt, err := readRecordTokens(e, w, buckets, cutoff, ops)
	if err != nil {
		return nil, nil, err
	}
	srcs := make([]string, len(buckets))
	for i, b := range buckets {
		srcs[i] = sourceOf(b.Key, w, hasServer, first, rt.held[i])
	}
	return &rt, srcs, nil
}
