package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/metricsfile"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
)

// serverDailyTimeout bounds each server read of the server-daily step.
const serverDailyTimeout = 3 * time.Second

// The reach of the answers the step asks for: the memories series over 90
// days, the LLM stats over 30, each the window now − period.
const (
	serverDailySeries      = "90d"
	serverDailyLLM         = "30d"
	serverDailySeriesReach = 90 * 24 * time.Hour
	serverDailyLLMReach    = 30 * 24 * time.Hour
	// serverDailySkew: the server's clock may be ahead of ours. A window
	// that names no start is taken to start that much later.
	serverDailySkew = time.Hour
)

// serverDailyOps are the LLM operations recorded per day: the line's key
// and the operation asked ("" = all of them).
var serverDailyOps = []struct{ key, op string }{
	{"all", ""}, {"retain", "retain"}, {"consolidation", "consolidation"}, {"refresh_mental_model", "refresh_mental_model"},
}

// serverDailyReader is what the step reads of the server (hindsight.ReadOnly).
type serverDailyReader interface {
	BankStats(context.Context) (hindsight.BankStats, error)
	ProjectTags(context.Context) ([]hindsight.TagCount, error)
	MemoriesTimeseries(context.Context, string) (hindsight.Timeseries, error)
	LLMStatsWindow(context.Context, string, string) (hindsight.LLMWindow, error)
}

// readOnlyServer is the configured server's GET-only client, each call
// within serverDailyTimeout: nil and no error when no server is set up (no
// config, or no server URL), an error when the configuration cannot be
// read or names no usable server.
func readOnlyServer() (*hindsight.ReadOnly, error) {
	cfg, sec, err := loadServerConfig()
	switch {
	case output.CodeOf(err) == output.NeedsUser: // no config: backsight setup was not run
		return nil, nil
	case err != nil:
		return nil, err
	case cfg.Server.URL == "":
		return nil, nil
	}
	return hindsight.NewReadOnly(cfg.Server.URL, sec.APIKey, cfg.Server.Bank, serverDailyTimeout)
}

// serverDailyReaderFor is readOnlyServer as the step's reader (a nil
// client is a nil reader). Tests replace it.
var serverDailyReaderFor = func() (serverDailyReader, error) {
	ro, err := readOnlyServer()
	if ro == nil {
		return nil, err
	}
	return ro, nil
}

// maintainServerDaily is jobs maintain's server-daily step: its result
// line, never an error. A configuration it cannot read is a server not
// reached (nothing is written: the record cannot tell the day's server).
func maintainServerDaily(ctx context.Context, d paths.Dirs, clock func() time.Time) string {
	r, err := serverDailyReaderFor()
	if err != nil {
		return "server daily: not reached (not_configured), 0 day(s)"
	}
	w := metricsfile.New(d.MetricsDir(), buildinfo.Version)
	w.Now = clock
	return collectServerDaily(ctx, w, r, clock(), time.Local)
}

// boundedRead is boundedWithin serverDailyTimeout.
func boundedRead[T any](ctx context.Context, read func(context.Context) (T, error)) (T, error) {
	return boundedWithin(ctx, serverDailyTimeout, read)
}

// boundedWithin runs read under its own context bounded by d and abandons
// it when that context ends: a reader that ignores its context cannot hold
// the step. An abandoned read's result is never used.
func boundedWithin[T any](ctx context.Context, d time.Duration, read func(context.Context) (T, error)) (T, error) {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1) // an abandoned read still sends, to no one
	go func() {
		v, err := read(cctx)
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-cctx.Done():
		select {
		case r := <-done: // it ended as its context did
			return r.v, r.err
		default:
		}
		var zero T
		return zero, cctx.Err()
	}
}

// serverDailyRecord is what the family's recent files already hold.
type serverDailyRecord struct {
	okToday, gapToday bool            // a snapshot of today's local date, ok or a gap
	buckets           map[string]bool // the UTC dates of the day lines
}

// readServerDailyRecord reads the family's last four month files: they
// hold every day line the series still reaches (90 days, the files going
// by when a line was written, never before its day) and today's snapshot
// (in the last two).
func readServerDailyRecord(dir string, at time.Time, today string) (serverDailyRecord, error) {
	rec := serverDailyRecord{buckets: map[string]bool{}}
	lines, err := metricsfile.ReadRecent(dir, metricsfile.FamilyServerDaily, at, 4)
	if err != nil {
		return rec, err
	}
	for _, l := range lines {
		var v struct {
			Day    string `json:"day"`
			OK     bool   `json:"ok"`
			Bucket string `json:"bucket"`
		}
		if json.Unmarshal(l.Raw, &v) != nil {
			continue
		}
		switch l.Kind {
		case "snapshot":
			if v.Day == today {
				rec.okToday = rec.okToday || v.OK
				rec.gapToday = rec.gapToday || !v.OK
			}
		case "day":
			if b, err := time.Parse(time.RFC3339Nano, v.Bucket); err == nil {
				rec.buckets[b.UTC().Format(time.DateOnly)] = true
			}
		}
	}
	return rec, nil
}

// collectServerDaily keeps the server's counts past its own reach: one
// snapshot per local day of loc (a gap line with the error enum when the
// server's stats cannot be read, at most one a day), and one day line per
// past full UTC day the server still holds (series 90 days, LLM stats 30)
// that the record lacks: today's UTC day never, a recorded day never again.
// The reads run at once, each within serverDailyTimeout. The result is a
// line for the job's report, never an error.
func collectServerDaily(ctx context.Context, w *metricsfile.Writer, r serverDailyReader, now time.Time, loc *time.Location) string {
	if r == nil {
		return "server daily: no server set up"
	}
	today := now.In(loc).Format(time.DateOnly)
	rec, err := readServerDailyRecord(w.Dir, now, today)
	if err != nil {
		// Without the record a run would write its days twice.
		return "server daily: not written (" + writeClass(err) + ")"
	}

	var (
		wg        sync.WaitGroup
		stats     hindsight.BankStats
		statsErr  error
		tags      []hindsight.TagCount
		tagsErr   error
		series    hindsight.Timeseries
		seriesErr error
		llm       = make([]hindsight.LLMWindow, len(serverDailyOps))
		llmErr    = make([]error, len(serverDailyOps))
	)
	if !rec.okToday { // the snapshot's reads, once a day
		wg.Go(func() { stats, statsErr = boundedRead(ctx, r.BankStats) })
		wg.Go(func() { tags, tagsErr = boundedRead(ctx, r.ProjectTags) })
	}
	wg.Go(func() {
		series, seriesErr = boundedRead(ctx, func(c context.Context) (hindsight.Timeseries, error) {
			return r.MemoriesTimeseries(c, serverDailySeries)
		})
	})
	for i, o := range serverDailyOps {
		wg.Go(func() {
			llm[i], llmErr[i] = boundedRead(ctx, func(c context.Context) (hindsight.LLMWindow, error) {
				return r.LLMStatsWindow(c, serverDailyLLM, o.op)
			})
		})
	}
	wg.Wait()

	var recs []metricsfile.Record
	head := "server daily: snapshot already taken"
	switch {
	case rec.okToday:
	case statsErr != nil:
		class := metrics.ServerErrorClass(statsErr)
		head = "server daily: not reached (" + class + ")"
		if !rec.gapToday {
			recs = append(recs, metricsfile.Snapshot{Day: today, OK: false, Error: class})
		}
	default:
		head = "server daily: snapshot"
		if tagsErr != nil {
			tags = nil // a snapshot without projects
		}
		recs = append(recs, serverSnapshot(today, stats, tags))
	}
	llmOK := make([]bool, len(llmErr))
	for i, err := range llmErr {
		if llmOK[i] = err == nil; !llmOK[i] {
			llm[i] = hindsight.LLMWindow{}
		}
	}
	if seriesErr != nil {
		series = hindsight.Timeseries{}
	}
	days := serverDayLines(series, seriesErr == nil, llm, llmOK, now, rec.buckets)
	for _, d := range days {
		recs = append(recs, d)
	}
	if err := w.Append(recs...); err != nil {
		return "server daily: not written (" + writeClass(err) + ")"
	}
	return fmt.Sprintf("%s, %d day(s)", head, len(days))
}

// utcDate is the start of t's UTC day.
func utcDate(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// serverSnapshot is the day's ok snapshot: only what the server said (a
// field it left out or sent null stays nil, never 0).
func serverSnapshot(day string, s hindsight.BankStats, tags []hindsight.TagCount) metricsfile.Snapshot {
	count := func(field string, v int) *int64 {
		if !s.Present[field] {
			return nil
		}
		x := int64(v)
		return &x
	}
	kind := func(k string) *int64 {
		v, ok := s.NodesByFactType[k]
		if !ok {
			return nil
		}
		x := int64(v)
		return &x
	}
	snap := metricsfile.Snapshot{Day: day, OK: true,
		World: kind("world"), Experience: kind("experience"), Observation: kind("observation"),
		Documents:            count("total_documents", s.TotalDocuments),
		Links:                count("total_links", s.TotalLinks),
		Observations:         count("total_observations", s.TotalObservations),
		ConsolidationPending: count("pending_consolidation", s.PendingConsolidation),
		ConsolidationFailed:  count("failed_consolidation", s.FailedConsolidation),
	}
	if s.OperationsByStatus != nil {
		// The server's counts by status: a status it lists no operation of
		// has none; one outside the four is other.
		snap.Ops = map[string]int64{"pending": 0, "processing": 0, "completed": 0, "failed": 0, "other": 0}
		for status, c := range s.OperationsByStatus {
			if _, known := snap.Ops[status]; !known {
				status = "other"
			}
			snap.Ops[status] += int64(c)
		}
	}
	if !s.LastConsolidatedAt.IsZero() {
		at := s.LastConsolidatedAt
		snap.LastConsolidatedAt = &at
	}
	for _, t := range tags {
		name, ok := strings.CutPrefix(t.Tag, "project:")
		if !ok || !recordName(name) {
			continue
		}
		if snap.Projects == nil {
			snap.Projects = map[string]int64{}
		}
		snap.Projects[name] += int64(t.Count)
	}
	return snap
}

// serverDayLines are the day lines of every past full UTC day (before
// today's) the answers reach and the record lacks, oldest first, each with
// what the answers say of it, the record being permanent:
//   - an answer covers the days after the one holding its window's start
//     (the start it names, else now − period + serverDailySkew): that day
//     and those before may hold part of their day only, they are never
//     used (the day is an earlier run's, or a hole);
//   - a day an answer covers without a bucket is a day without activity:
//     its counts are written as zeros (the series' new, each LLM
//     operation's entry with no statuses);
//   - a day is written only when every source whose reach covers it
//     answered this run (the series covers 90 days, the LLM stats, all
//     four operations, 30): otherwise it waits for a later run, and at
//     worst is written with new only once it leaves the 30-day reach;
//   - only UTC daily buckets count: an answer in another width, or with a
//     bucket off a UTC day start, gives no section.
func serverDayLines(series hindsight.Timeseries, seriesOK bool, llm []hindsight.LLMWindow, llmOK []bool, now time.Time, recorded map[string]bool) []metricsfile.ServerDay {
	today := utcDate(now)
	allLLM := !slices.Contains(llmOK, false)
	seriesFrom, llmFrom := utcDate(now.Add(-serverDailySeriesReach)), utcDate(now.Add(-serverDailyLLMReach))
	byDay := map[time.Time]*metricsfile.ServerDay{}
	line := func(b time.Time) *metricsfile.ServerDay {
		if !b.Before(today) || recorded[b.Format(time.DateOnly)] {
			return nil
		}
		if (!seriesOK && !b.Before(seriesFrom)) || (!allLLM && !b.Before(llmFrom)) {
			return nil // a source that covers the day did not answer
		}
		d := byDay[b]
		if d == nil {
			d = &metricsfile.ServerDay{Bucket: b}
			byDay[b] = d
		}
		return d
	}
	// covered are the days of an answer's window after the one holding
	// its start, before today's; nil when its buckets are not UTC days.
	covered := func(trunc string, times []time.Time, start time.Time, reach time.Duration) map[time.Time]bool {
		if trunc != "day" {
			return nil
		}
		for _, t := range times {
			if !t.Equal(utcDate(t)) {
				return nil
			}
		}
		if start.IsZero() {
			start = now.Add(-reach + serverDailySkew)
		}
		out := map[time.Time]bool{}
		for d := utcDate(start).AddDate(0, 0, 1); d.Before(today); d = d.AddDate(0, 0, 1) {
			out[d] = true
		}
		return out
	}
	if seriesOK {
		times := make([]time.Time, 0, len(series.Buckets))
		for _, b := range series.Buckets {
			times = append(times, b.Time.UTC())
		}
		days := covered(series.Trunc, times, time.Time{}, serverDailySeriesReach)
		for d := range days {
			if l := line(d); l != nil {
				l.New = &metricsfile.KindCounts{}
			}
		}
		for i, b := range series.Buckets {
			if !days[times[i]] {
				continue
			}
			if d := line(times[i]); d != nil {
				d.New = &metricsfile.KindCounts{World: b.World, Experience: b.Experience, Observation: b.Observation}
			}
		}
	}
	for i, o := range serverDailyOps {
		if !llmOK[i] {
			continue
		}
		w := llm[i]
		times := make([]time.Time, 0, len(w.Buckets))
		for _, b := range w.Buckets {
			times = append(times, b.Time.UTC())
		}
		days := covered(w.Trunc, times, w.Start, serverDailyLLMReach)
		set := func(t time.Time, v metricsfile.LLMDay) {
			d := line(t)
			if d == nil {
				return
			}
			if d.LLM == nil {
				d.LLM = map[string]metricsfile.LLMDay{}
			}
			d.LLM[o.key] = v
		}
		for d := range days {
			set(d, metricsfile.LLMDay{})
		}
		for j, b := range w.Buckets {
			if !days[times[j]] {
				continue
			}
			var st map[string]int64
			for k, v := range b.Statuses {
				if !recordName(k) {
					continue
				}
				if st == nil {
					st = map[string]int64{}
				}
				st[k] = v
			}
			set(times[j], metricsfile.LLMDay{Input: b.Input, Output: b.Output, Cached: b.Cached, Thoughts: b.Thoughts, Requests: b.Total, Statuses: st})
		}
	}
	out := make([]metricsfile.ServerDay, 0, len(byDay))
	for _, d := range byDay {
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b metricsfile.ServerDay) int { return a.Bucket.Compare(b.Bucket) })
	return out
}

// recordName mirrors the metrics files' guard on a name kept as a key (a
// project, a status): non-empty, bounded, valid UTF-8, no control
// character. A name it refuses is left out rather than the whole line.
func recordName(s string) bool {
	return s != "" && len(s) <= 256 && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// writeClass is the class of a metrics file failure: permission, io, or
// refused (a record the writer would not take).
func writeClass(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	case !errors.As(err, new(*fs.PathError)) && !errors.As(err, new(*os.SyscallError)):
		return "refused"
	}
	return "io"
}
