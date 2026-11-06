package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
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
// attempt. Fetched, Ignored and FetchError are the fetch of the other
// PCs' documents (not part of the publish).
type metricsShare struct {
	Up, Waiting, Empty bool
	Days, Bytes        int
	Dropped            machines.Dropped
	Failure            string
	Fetched, Ignored   int
	FetchError         string
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
	dir, target := d.MetricsDir(), shareTarget(d, cfg)
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
	meta, err := doc.Marshal()
	if err != nil {
		return fail(failCompute, false)
	}
	content := fmt.Sprintf("daily figures of machine %s: %d days", machine, len(doc.Days))
	if err := retain.WriteControlDocument(ctx, cl, cfg.Server.Bank, machines.DocPrefix+machine, content, map[string]string{machines.MetaKey: meta}); err != nil {
		return fail(failServer, false)
	}
	s := metricsShare{Up: true, Days: len(doc.Days), Bytes: len(meta), Dropped: dropped, IndexLine: line}
	return s, machines.SavePublished(dir, machines.Published{At: now, Server: target, Until: doc.Until, FirstSeen: first, Bytes: len(meta)})
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
