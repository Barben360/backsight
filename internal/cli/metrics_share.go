package cli

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/machines"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
)

// metricsShareBudget bounds the tick's publish of this PC's daily figures:
// the catch-up of the index, the computation and the write.
var metricsShareBudget = 30 * time.Second

// metricsRetryAfter: a failed publish is tried again after this long (at
// once when its time is ahead of the clock).
const metricsRetryAfter = time.Hour

// metricsWriteMax is the size the written document fits (a var for the
// tests: the cap itself cannot be exceeded by one day of figures).
var metricsWriteMax = machines.WriteMax

// dailyFigures and publishCatchUp are the seams of the tests.
var dailyFigures = metrics.Daily
var publishCatchUp = func(ctx context.Context, ix *index.Index, b time.Duration) (index.Freshness, error) {
	return ix.CatchUp(ctx, b)
}

// The classes of a failed publish (the publish record's failure, the job
// event's metrics_failure); never an error's text.
const (
	failCompute  = "compute"
	failBudget   = "budget"
	failServer   = "server"
	failTooLarge = "too_large"
)

// metricsShare is what the tick's publish did. Up: the document was
// written (Days days, Bytes bytes, Dropped what the cap left out);
// Waiting: the index was not caught up (Failure is then budget);
// Empty: no complete day to share yet; Failure: the class of a failed
// attempt.
type metricsShare struct {
	Up, Waiting, Empty bool
	Days, Bytes        int
	Dropped            machines.Dropped
	Failure            string
	// IndexLine is the tick's metrics index line when this step caught the
	// index up (the tick then does not catch it up again), "" otherwise.
	IndexLine string
}

// publishOffset spreads the PCs over the first hour of the UTC day: the
// machine id's first byte mod 60, in minutes (0 for an id that does not
// start with a byte).
func publishOffset(machine string) time.Duration {
	if len(machine) < 2 {
		return 0
	}
	b, err := hex.DecodeString(machine[:2])
	if err != nil {
		return 0
	}
	return time.Duration(int(b[0])%60) * time.Minute
}

func utcMidnight(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// metricsDue says whether the daily figures are to be sent: there is no
// readable record, it is dated ahead of now, they went to another server
// or bank, or the last send's UTC date is before today's and now is
// publishOffset past 00:00 UTC. A failure is tried again an hour later
// (at once when its time is ahead of now); a PC that waits for its index
// (no failed_at) is tried at every tick.
func metricsDue(last machines.Published, ok bool, target, machine string, now time.Time) bool {
	if !ok || last.At.After(now) || last.FailedAt.After(now) {
		return true
	}
	if !last.FailedAt.IsZero() && now.Sub(last.FailedAt) < metricsRetryAfter {
		return false // also after a change of server: a failing publish waits its hour
	}
	if !last.At.IsZero() && last.Server != target {
		return true
	}
	if !last.At.IsZero() && !utcMidnight(last.At).Before(utcMidnight(now)) {
		return false
	}
	return now.Sub(utcMidnight(now)) >= publishOffset(machine)
}

// publishMetrics sends this PC's daily figures once a UTC day (the tick's
// sharing step, after the shared counts, with feedback and share_ledger
// on, on a client). It catches the metrics index up first and goes no
// further unless that finished (Waiting), computes the days, fits the
// document under the write cap and writes it, then saves the record. A
// failure is a class in the result and in the record (failed_at, and
// failed_since until the next success), never an error: err is only a
// record that could not be saved.
func publishMetrics(ctx context.Context, cl *hindsight.Client, d paths.Dirs, home string, cfg config.Config, machine string, now time.Time) (metricsShare, error) {
	if !cfg.Feedback.Enabled || !cfg.Feedback.ShareLedger || machine == "" || cfg.ServerOnly() {
		return metricsShare{}, nil
	}
	dir, target := d.MetricsDir(), machines.TargetKey(shareTarget(d, cfg)) // the record keeps a hash, never the address and bank
	rec, ok := machines.LoadPublished(dir)
	if !metricsDue(rec, ok, target, machine, now) {
		return metricsShare{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, metricsShareBudget)
	defer cancel()

	line := "" // the tick's metrics index line, once this step caught the index up

	// fail records a failed attempt: failed_at moves, failed_since stays.
	// A record that never sent anything also names the target, so that a
	// change of server is told from a failure.
	fail := func(class string, wait bool) (metricsShare, error) {
		if !ok {
			rec = machines.Published{}
		}
		if rec.At.IsZero() {
			rec.Server = target
		}
		if !wait {
			rec.FailedAt = now
		}
		if rec.FailedSince.IsZero() || rec.FailedSince.After(now) {
			rec.FailedSince = now
		}
		rec.Failure = class
		return metricsShare{Waiting: wait, Failure: class, IndexLine: line}, machines.SavePublished(dir, rec)
	}

	o := indexOptions(d, home, nil)
	o.Now = tickIndexClock
	ix, err := index.Open(ctx, o)
	if err != nil {
		return fail(failCompute, false)
	}
	defer func() { _ = ix.Close() }()
	side := ""
	if ix.Side() {
		side = " (" + filepath.Base(ix.Path()) + ": a newer backsight's index is kept)"
	}
	fr, cerr := publishCatchUp(ctx, ix, tickIndexBudget)
	line = tickIndexText(fr) + side
	if cerr != nil {
		line = indexStepError(cerr) + side
	} else {
		_ = ix.Checkpoint(ctx)
	}
	if cerr != nil || fr.Partial || fr.Yielded {
		return fail(failBudget, true)
	}
	days, err := dailyFigures(ctx, metrics.Source{Index: ix, Config: cfg, Configured: true, Now: func() time.Time { return now }}, now)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return fail(failBudget, false)
		}
		return fail(failCompute, false)
	}
	if len(days) == 0 {
		return metricsShare{Empty: true, IndexLine: line}, nil
	}

	first := rec.FirstSeen
	if !ok || first == "" {
		first = now.UTC().Format(time.DateOnly)
	}
	version := buildinfo.Version
	if !machines.ValidVersion(version) {
		version = "other"
	}
	doc := machines.Doc{Machine: machine, At: now.UTC(), OS: machines.OS(runtime.GOOS), FirstSeen: first, BinaryVersion: version,
		Until: now.UTC().AddDate(0, 0, -1).Format(time.DateOnly), Days: days}
	doc, dropped, err := machines.Fit(doc, metricsWriteMax)
	if err != nil {
		return fail(failTooLarge, false)
	}
	// The document is checked with the reader's own rules before it leaves:
	// one a reader would refuse is not sent. A version figure above the day's
	// is the likely cause, so the breakdown goes first; if it still fails,
	// nothing is written.
	name := machines.DocPrefix + machine
	meta, err := doc.Marshal()
	if err == nil {
		if _, perr := machines.Parse(name, meta, now); perr != nil {
			doc.Days = withoutVersions(doc.Days)
			meta, err = doc.Marshal()
			if err == nil {
				_, err = machines.Parse(name, meta, now)
			}
		}
	}
	if err != nil {
		return fail(failCompute, false)
	}
	content := fmt.Sprintf("daily figures of machine %s: %d days", machine, len(doc.Days))
	if err := retain.WriteControlDocument(ctx, cl, cfg.Server.Bank, name, content, map[string]string{machines.MetaKey: meta}); err != nil {
		return fail(failServer, false)
	}
	s := metricsShare{Up: true, Days: len(doc.Days), Bytes: len(meta), Dropped: dropped, IndexLine: line}
	return s, machines.SavePublished(dir, machines.Published{At: now, Server: target, Until: doc.Until, FirstSeen: first, Bytes: len(meta)})
}

// withoutVersions is days with every version breakdown removed (a copy).
func withoutVersions(days []machines.Day) []machines.Day {
	out := make([]machines.Day, len(days))
	for i, d := range days {
		d.Versions = nil
		out[i] = d
	}
	return out
}

// applyMetrics puts the publish's result in the tick's: err is the
// record that could not be saved (a fixed text, never the error's).
func (r *tickResult) applyMetrics(s metricsShare, err error) {
	r.MetricsUp, r.MetricsDays, r.MetricsBytes = s.Up, s.Days, s.Bytes
	r.MetricsWaiting, r.MetricsEmpty, r.MetricsFailure = s.Waiting, s.Empty, s.Failure
	r.metricsDropped, r.metricsIndex = s.Dropped, s.IndexLine
	if err != nil {
		r.MetricsError = "the publish record could not be written"
	}
}

// metricsText is the tick line's part for the daily figures, "" when the
// step did nothing.
func (r tickResult) metricsText() string {
	switch {
	case r.MetricsUp:
		msg := fmt.Sprintf("daily figures sent (%d days, %d bytes", r.MetricsDays, r.MetricsBytes)
		if n := r.metricsDropped.VersionDays; n > 0 {
			msg += fmt.Sprintf("; version breakdown of %d oldest day(s) left out to fit", n)
		}
		if n := r.metricsDropped.Days; n > 0 {
			msg += fmt.Sprintf("; %d oldest day(s) left out to fit", n)
		}
		return msg + ")"
	case r.MetricsWaiting:
		return "daily figures: waiting for the index"
	case r.MetricsEmpty:
		return "daily figures: nothing to share yet"
	case r.MetricsFailure != "":
		return "daily figures not shared (" + r.MetricsFailure + "): the next tick retries"
	case r.MetricsError != "":
		return "daily figures: " + r.MetricsError
	}
	return ""
}

// metricsRefreshEvery: the other PCs' documents are fetched when the cache
// is this old, missing, unreadable or dated ahead of now.
const metricsRefreshEvery = shareRefreshEvery

// The classes of a failed fetch (the job event's metrics_fetch_error);
// never an error's text.
const (
	fetchServer = "server"
	fetchBudget = "budget"
	fetchCache  = "cache"
)

// refreshMetricsDocs fetches the other PCs' daily figures into machines.json,
// when sharing is on and the cache is metricsRefreshEvery old (or dated ahead
// of now), missing or unreadable. The documents come from the tick's one
// listing l. Each is read with machines.Parse at now (untrusted): this PC's
// own is left out, so is one more than machines.DropAfter old, a name listed
// twice keeps its first, at most machines.MaxDocs are kept, the newest by at;
// every refused one is recorded as ignored (its short id, if any, and the
// reason). A failure keeps the cache as it was.
func refreshMetricsDocs(ctx context.Context, cl *hindsight.Client, l *controlListing, d paths.Dirs, cfg config.Config, machine string, now time.Time) (fetched, ignored int, err error) {
	if !cfg.Feedback.Enabled || !cfg.Feedback.ShareLedger || machine == "" {
		return 0, 0, nil
	}
	dir := d.MetricsDir()
	if c, cerr := machines.LoadCache(dir); cerr == nil {
		if age := now.Sub(c.FetchedAt); age >= 0 && age < metricsRefreshEvery {
			return 0, 0, nil
		}
	}
	items, err := l.docs(ctx, cl, cfg.Server.Bank, machines.DocPrefix)
	if err != nil {
		return 0, 0, err
	}
	slices.SortStableFunc(items, func(a, b hindsight.DocumentItem) int { return cmp.Compare(a.ID, b.ID) })
	type kept struct {
		name, raw string
		at        time.Time
	}
	var good []kept
	fresh := machines.Cache{FetchedAt: now, Docs: []machines.CachedDoc{}, Ignored: []machines.Ignored{}}
	seen := map[string]bool{}
	for _, it := range items {
		if it.ID == machines.DocPrefix+machine || seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		raw := it.Metadata[machines.MetaKey]
		doc, perr := machines.Parse(it.ID, raw, now)
		if perr != nil {
			reason := machines.NotValid
			if pe := (*machines.ParseError)(nil); errors.As(perr, &pe) {
				reason = pe.Reason
			}
			if len(fresh.Ignored) < machines.MaxDocs {
				fresh.Ignored = append(fresh.Ignored, machines.Ignored{Machine: machines.Short(it.ID), Reason: reason})
			}
			continue
		}
		if now.Sub(doc.At) > machines.DropAfter {
			continue
		}
		// The canonical form, not the metadata as fetched: unknown fields
		// (which Parse ignores) must not reach the cache, where JSON escaping
		// can multiply them by six.
		canon, merr := doc.Marshal()
		if merr != nil {
			continue
		}
		// min(at, now): a document dated ahead (Parse allows 24 h) must not
		// outrank the PCs that really sent lately.
		at := doc.At
		if at.After(now) {
			at = now
		}
		good = append(good, kept{it.ID, canon, at})
	}
	slices.SortStableFunc(good, func(a, b kept) int { // newest first
		return b.at.Compare(a.at)
	})
	good = good[:min(len(good), machines.MaxDocs)]
	good, over := withinCacheBudget(good, func(g kept) machines.CachedDoc { return machines.CachedDoc{Name: g.name, Raw: g.raw} }, cacheDocsBudget)
	for _, g := range over {
		if len(fresh.Ignored) < machines.MaxDocs {
			fresh.Ignored = append(fresh.Ignored, machines.Ignored{Machine: machines.Short(g.name), Reason: machines.TooLarge})
		}
	}
	slices.SortFunc(good, func(a, b kept) int { return cmp.Compare(a.name, b.name) })
	for _, g := range good {
		fresh.Docs = append(fresh.Docs, machines.CachedDoc{Name: g.name, Raw: g.raw})
	}
	if err := machines.SaveCache(dir, fresh); err != nil {
		return 0, 0, errCacheWrite
	}
	return len(fresh.Docs), len(fresh.Ignored), nil
}

// cacheDocsBudget is what the cached documents may take once JSON-escaped,
// MaxCacheBytes less room for the rest of machines.json.
const cacheDocsBudget = machines.MaxCacheBytes - 1<<20

// withinCacheBudget keeps the leading items whose escaped JSON, summed, stays
// within budget; the others (in order) are returned as over. A running total,
// so a document that would not fit does not stop a later, smaller one.
func withinCacheBudget[T any](items []T, doc func(T) machines.CachedDoc, budget int) (kept, over []T) {
	total := 0
	for _, it := range items {
		b, err := json.Marshal(doc(it))
		if err != nil || total+len(b)+1 > budget {
			over = append(over, it)
			continue
		}
		total += len(b) + 1
		kept = append(kept, it)
	}
	return kept, over
}

var errCacheWrite = errors.New("the cache could not be written")

// fetchMetrics is the tick's fetch of the other PCs' daily figures, within
// metricsShareBudget; the failure is a class, never an error's text.
func fetchMetrics(ctx context.Context, cl *hindsight.Client, l *controlListing, d paths.Dirs, cfg config.Config, machine string, now time.Time) (fetched, ignored int, class string) {
	// Its own budget, apart from the publish's: a tick that publishes and
	// fetches takes at most 2 x metricsShareBudget (60 s) for the daily
	// figures, on top of the shared counts' own shareTickBudget. Acceptable:
	// it is a background tick, and a miss only delays the next attempt. A
	// listing the shared counts already cached as failed is reported as budget
	// here even if this budget was not spent.
	ctx, cancel := context.WithTimeout(ctx, metricsShareBudget)
	defer cancel()
	fetched, ignored, err := refreshMetricsDocs(ctx, cl, l, d, cfg, machine, now)
	switch {
	case err == nil:
		return fetched, ignored, ""
	case errors.Is(err, errCacheWrite):
		return 0, 0, fetchCache
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		return 0, 0, fetchBudget
	}
	return 0, 0, fetchServer
}

// applyFetch puts the fetch's result in the tick's.
func (r *tickResult) applyFetch(fetched, ignored int, class string) {
	r.MetricsFetched, r.MetricsIgnored, r.MetricsFetchError = fetched, ignored, class
}

// fetchText is the tick line's part for the other PCs' figures, "" when the
// fetch did nothing.
func (r tickResult) fetchText() string {
	switch {
	case r.MetricsFetchError != "":
		return "other PCs' daily figures not fetched (" + r.MetricsFetchError + "): the next tick retries"
	case r.MetricsFetched > 0 || r.MetricsIgnored > 0:
		msg := fmt.Sprintf("daily figures of %d other PC(s) fetched", r.MetricsFetched)
		if r.MetricsIgnored > 0 {
			msg += fmt.Sprintf(", %d document(s) ignored", r.MetricsIgnored)
		}
		return msg
	}
	return ""
}
