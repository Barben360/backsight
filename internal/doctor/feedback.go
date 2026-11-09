package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/feedback/ledgerdb"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/stats"
	"github.com/Barben360/backsight/internal/telemetry"
)

const (
	// feedbackJournalWarn: above it in total the journal is worth a look.
	feedbackJournalWarn = 200 << 20
	// feedbackLagWarn: a ledger this far behind the journal is not being
	// settled by the ticks.
	feedbackLagWarn = 24 * time.Hour
)

const feedbackOff = "feedback off ([feedback] enabled = false): nothing is journaled or settled"

// journalFiles is dir's journal month files, oldest first.
func journalFiles(dir string) []string {
	names, _ := feedback.MonthFiles(dir) // journal-YYYY-MM sorts by month
	files := make([]string, len(names))
	for i, n := range names {
		files[i] = filepath.Join(dir, n)
	}
	return files
}

// checkFeedbackJournal: the hooks can append to the newest month file
// (opened for append, nothing written), and the journal stays small.
func checkFeedbackJournal(d paths.Dirs, enabled bool) Check {
	c := Check{ID: "feedback-journal", Status: OK}
	if !enabled {
		c.Status, c.Summary = Info, feedbackOff
		return c
	}
	dir := d.FeedbackDir()
	files := journalFiles(dir)
	if len(files) == 0 {
		c.Summary = "no journal yet: nothing shown to Claude since it was enabled"
		return c
	}
	var total int64
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			total += fi.Size()
		}
	}
	newest := files[len(files)-1]
	f, err := os.OpenFile(newest, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
			err = pe.Err // the file is named already
		}
		c.Status, c.Summary = Error, "cannot append to "+filepath.Base(newest)+": "+err.Error()
		c.Detail = "every memory shown and every credit is lost until it can be written, and the counts stop changing (hooks and recall go on)"
		c.Fix = "make " + dir + " and its files writable by this user, and check free space there"
		return c
	}
	_ = f.Close()
	mb := float64(total) / (1 << 20)
	c.Summary = fmt.Sprintf("%d month file(s), %.1f MB", len(files), mb)
	if total > feedbackJournalWarn {
		c.Status, c.Summary = Warn, fmt.Sprintf("the journal holds %.0f MB in %d month file(s), above %d MB", mb, len(files), feedbackJournalWarn>>20)
		c.Detail = "the month files are the history the counts are rebuilt from: moving or deleting one drops the counts it held at the next jobs tick"
		c.Fix = "keep the month files in " + dir + "; remove the oldest only if losing the counts they hold is acceptable"
	}
	return c
}

// ledgerFallbackWarn: this many fallbacks of the ledger's readers, or
// failed folds and exports, in a day are worth a look.
const ledgerFallbackWarn = 3

// ledgerFallbackReasons are the feedback_error reasons doctor counts: a
// reader that could not read ledger.db (ledger_missing aside: the
// database not built yet, which the check says itself) or ledger.json
// either, a tick that could not fold or export.
var ledgerFallbackReasons = []string{telemetry.FeedbackReasonLedgerBusy, telemetry.FeedbackReasonLedgerUnreadable, telemetry.FeedbackReasonLedgerSchema,
	telemetry.FeedbackReasonLedgerJSON, telemetry.FeedbackReasonLedgerExport, telemetry.FeedbackReasonLedgerFold}

const (
	ledgerTickFix    = "backsight jobs tick settles it (its output says why if it cannot); the jobs check says whether the scheduler runs it"
	ledgerRebuildFix = "backsight feedback rebuild recomputes it from the journal"
)

// checkFeedbackLedger judges the ledger database from f, its facts (err:
// they could not be read): built (a missing one is fine until a recent
// tick could not build it), the file of this binary's schema, folded
// within a day of the newest journal event, ledger.json exported, and
// its readers not falling back day after day (the telemetry of the last
// 24 h). The first problem is the summary and fix; the others go in the
// detail.
func checkFeedbackLedger(d paths.Dirs, enabled bool, f ledgerdb.LedgerFacts, err error, now time.Time) Check {
	c := Check{ID: "feedback-ledger", Status: OK}
	if !enabled {
		c.Status, c.Summary = Info, feedbackOff
		return c
	}
	dir := d.FeedbackDir()
	type problem struct{ summary, detail, fix string }
	var problems []problem
	add := func(summary, detail, fix string) { problems = append(problems, problem{summary, detail, fix}) }
	_, last, journaled := newestEvents(dir)
	failures := ledgerFailures(d, now)
	switch {
	case err != nil:
		add("cannot read the ledger database's state: "+err.Error(), "", ledgerRebuildFix)
	case f.Newer:
		add("a newer backsight migrated ledger.db: this one keeps its counts in "+f.File+" beside it", "recall reads ledger.json, which this backsight exports from "+f.File,
			"run the newer backsight again (backsight update); jobs maintain then removes "+f.File)
	}
	if err == nil {
		switch {
		// A damaged file is one; a folder a tick could not build in, one
		// once a tick tried.
		case f.OpenError == "corrupt" || f.OpenError == "notadb":
			add(fmt.Sprintf("%s in %s is damaged (%s)", f.File, dir, f.OpenError),
				"the ticks fold nothing new into it until it is replaced; recall reads the last copy of the counts, ledger.json", ledgerRebuildFix)
		case f.OpenError != "" && (f.Built || tickRecent(d, now)):
			fix := "make " + dir + " and its files writable by this user, and check free space there"
			add(fmt.Sprintf("%s cannot be written in %s (%s)", f.File, dir, f.OpenError),
				"the ticks fold nothing new into it until it can; recall reads the last copy of the counts, ledger.json", fix)
		case !f.Built && journaled:
			c.Summary = "not built yet: it builds at the next jobs tick"
			if f.UpdateInProgress {
				c.Summary = "not built yet: it builds at the first jobs tick after the update in progress"
			}
		case f.Built && journaled && last.Sub(f.FoldedAt) > feedbackLagWarn:
			add(fmt.Sprintf("folded up to %s UTC, %s before the newest journal event: the ticks are not folding", f.FoldedAt.UTC().Format("2006-01-02 15:04"),
				last.Sub(f.FoldedAt).Round(time.Hour)), "", ledgerTickFix)
		}
		if f.Built && f.ExportBehind && failures[telemetry.FeedbackReasonLedgerExport] >= 2 {
			add("ledger.json, the copy of the counts recall falls back to, was not written at the last ticks", "the database itself is up to date",
				"backsight jobs tick writes it (its output says why if it cannot)")
		}
	}
	if reason, n := mostFrequent(failures); n >= ledgerFallbackWarn {
		add(fmt.Sprintf("the feedback ledger failed %d times in 24 h, mostly %s", n, reason),
			"a read that fails falls back to ledger.json, then to no learned ranking; a fold or export that fails leaves the counts behind", ledgerRebuildFix)
	}
	if len(problems) > 0 {
		p := problems[0]
		c.Status, c.Summary, c.Detail, c.Fix = Warn, p.summary, p.detail, p.fix
		for _, q := range problems[1:] {
			c.Detail = strings.TrimPrefix(c.Detail+"; also: "+q.summary, "; ")
		}
		return c
	}
	if c.Summary != "" {
		return c
	}
	if !f.Built {
		c.Summary = "nothing journaled yet"
		return c
	}
	c.Summary = fmt.Sprintf("%d memories, settled up to %s UTC", f.Memories, f.Watermark.UTC().Format("2006-01-02 15:04"))
	return c
}

// ledgerFailures counts the last 24 h's feedback_error events of
// ledgerFallbackReasons by reason (none when the telemetry cannot be
// read).
func ledgerFailures(d paths.Dirs, now time.Time) map[string]int {
	out := map[string]int{}
	ev, err := stats.Load(d.TelemetryDir(), now.Add(-24*time.Hour), now.Add(time.Minute))
	if err != nil {
		return out
	}
	for _, e := range ev {
		if e.Event == "feedback_error" && slices.Contains(ledgerFallbackReasons, e.Reason) {
			out[e.Reason]++
		}
	}
	return out
}

// mostFrequent is the reason counted most (the first by name of those
// tied) and the total of every reason.
func mostFrequent(counts map[string]int) (reason string, total int) {
	for _, r := range slices.Sorted(maps.Keys(counts)) {
		total += counts[r]
		if counts[r] > counts[reason] {
			reason = r
		}
	}
	return reason, total
}

// newestEvents is the oldest and newest event time of the newest month
// file holding an event; false when the journal holds none.
func newestEvents(dir string) (first, last time.Time, ok bool) {
	files := journalFiles(dir)
	for i := len(files) - 1; i >= 0; i-- {
		events, _, _ := feedback.ReadFrom(files[i], 0)
		for _, e := range events {
			if !ok || e.TS.Before(first) {
				first = e.TS
			}
			if !ok || e.TS.After(last) {
				last = e.TS
			}
			ok = true
		}
		if ok {
			return first, last, true
		}
	}
	return first, last, false
}

// sharedCacheWarn: a cache of the other machines' counts this old, while
// the server answers, is no longer being fetched by the ticks.
const sharedCacheWarn = 24 * time.Hour

// checkFeedbackShared: how many other machines' counts the local cache
// holds and how old it is (jobs tick fetches it every 6 hours).
func checkFeedbackShared(d paths.Dirs, enabled, share bool, now time.Time) Check {
	c := Check{ID: "feedback-shared", Status: OK}
	switch {
	case !enabled:
		c.Status, c.Summary = Info, feedbackOff
		return c
	case !share:
		c.Status, c.Summary = Info, "sharing off ([feedback] share_ledger = false): this machine's counts are neither sent to the server nor read from the other machines"
		return c
	}
	fix := "backsight jobs tick fetches it (its output says why if it cannot); the jobs check says whether the scheduler runs it"
	r := state.LoadReach(d)
	answers := !r.LastOK.IsZero() && now.Sub(r.LastOK) <= sharedCacheWarn
	cache, err := feedback.LoadSharedCache(d.FeedbackDir())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Every send is followed by a fetch: a send a day old with no
		// cache yet, while the server answers, is a fetch that fails.
		c.Summary = "the other machines' counts are not fetched yet"
		if up, ok := feedback.LoadSharedUpload(d.FeedbackDir()); ok && answers && now.Sub(up.At) > sharedCacheWarn {
			c.Status, c.Fix = Warn, fix
			c.Summary = fmt.Sprintf("the other machines' counts were never fetched, though this machine sent its own %s ago", now.Sub(up.At).Round(time.Hour))
		}
		return c
	case err != nil:
		c.Status, c.Summary = Info, "the cache of the other machines' counts is unreadable: "+err.Error()
		if answers {
			c.Status, c.Fix = Warn, fix
		}
		return c
	}
	docs, _ := cache.Parsed()
	age := now.Sub(cache.FetchedAt).Round(time.Hour)
	c.Summary = fmt.Sprintf("counts of %d other machine(s), fetched %s ago", feedback.SharedMachines(docs, "", now), age)
	if answers && now.Sub(cache.FetchedAt) > sharedCacheWarn {
		c.Status, c.Fix = Warn, fix
		c.Detail = "recall here goes on with the counts fetched last"
	}
	return c
}
