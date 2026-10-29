package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
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
	files, _ := filepath.Glob(filepath.Join(dir, "journal-*.jsonl"))
	slices.Sort(files) // journal-YYYY-MM sorts by month
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
		c.Detail = "the tick reads its last two months, a rebuild all of it"
		c.Fix = "move the month files older than two months out of " + dir + ": the counts already settled stay; backsight feedback rebuild would then drop what they counted"
	}
	return c
}

// checkFeedbackLedger: the ledger is of this binary's schema and settled
// within a day of the newest journal event (the ticks settle).
func checkFeedbackLedger(d paths.Dirs, enabled bool) Check {
	c := Check{ID: "feedback-ledger", Status: OK}
	if !enabled {
		c.Status, c.Summary = Info, feedbackOff
		return c
	}
	dir := d.FeedbackDir()
	l, err := feedback.LoadLedger(dir)
	if err != nil {
		c.Status, c.Summary = Error, "ledger unreadable: "+err.Error()
		c.Detail = "the tick settles nothing while it cannot be read (it is never guessed)"
		c.Fix = "backsight feedback rebuild recomputes it from the journal"
		return c
	}
	first, last, ok := newestEvents(dir)
	switch {
	case !ok:
		c.Summary = "nothing journaled yet"
		return c
	case l.Watermark.IsZero():
		// Never settled: behind since the journal began.
		if last.Sub(first) > feedbackLagWarn {
			c.Status, c.Summary = Warn, "never settled, though the journal goes back to "+first.UTC().Format("2006-01-02 15:04")+" UTC"
			c.Fix = "backsight jobs tick settles it (its output says why if it cannot); the jobs check says whether the scheduler runs it"
			return c
		}
		c.Summary = "not settled yet: the journal began less than a day ago"
		return c
	case last.Sub(l.Watermark) > feedbackLagWarn:
		c.Status = Warn
		c.Summary = fmt.Sprintf("settled up to %s UTC, %s before the newest journal event: the ticks are not settling", l.Watermark.UTC().Format("2006-01-02 15:04"),
			last.Sub(l.Watermark).Round(time.Hour))
		c.Fix = "backsight jobs tick settles it (its output says why if it cannot); the jobs check says whether the scheduler runs it"
		return c
	}
	c.Summary = fmt.Sprintf("%d memories, settled up to %s UTC", l.Counted(), l.Watermark.UTC().Format("2006-01-02 15:04"))
	return c
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
