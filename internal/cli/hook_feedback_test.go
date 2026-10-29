package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/session"
)

// monthsBack is the 15th of the month back months before this one, UTC.
func monthsBack(back int) time.Time {
	m := time.Now().UTC()
	return time.Date(m.Year(), m.Month()-time.Month(back), 15, 12, 0, 0, 0, time.UTC)
}

// journalFile is the month file of dir's journal that holds t.
func journalFile(dir string, t time.Time) string {
	return filepath.Join(dir, "journal-"+t.Format("2006-01")+".jsonl")
}

// unreadableMonth puts a month file nothing can read (a directory) in dir.
func unreadableMonth(t *testing.T, dir string, month time.Time) {
	t.Helper()
	if err := os.MkdirAll(journalFile(dir, month), 0o700); err != nil {
		t.Fatal(err)
	}
}

func showIDs(shown []feedback.Show) []string {
	var ids []string
	for _, s := range shown {
		ids = append(ids, s.ID)
	}
	return ids
}

// A session's shows are read from the month its journal began, which a
// resume keeps: an unreadable month file from before no longer sends the
// lookup to the session state's seen ids, and a show of the month the
// session began, before this one, is still found. A session whose start
// is unknown reads the last 62 days, as before.
func TestShownInSessionReadsFromSessionStart(t *testing.T) {
	home := hookHome(t)
	d := pathsOf(home)
	dir := d.FeedbackDir()
	unreadableMonth(t, dir, monthsBack(2))
	show := func(sess, id string, at time.Time) {
		t.Helper()
		e := feedback.Event{Kind: "shows", TS: at, Path: "search", Items: []feedback.Item{{ID: id, Rank: 1}}}
		if err := feedback.NewJournal(dir, feedback.Common{Session: sess}).Append(e); err != nil {
			t.Fatal(err)
		}
		_ = sessions(home).Update(sess, func(st *session.State) error { st.Seen = []string{"seen-only"}; return nil })
	}

	// Began this month.
	fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": home})
	show("s1", "m-this", time.Time{})
	if got := showIDs(shownInSession(d, "s1")); !slices.Contains(got, "m-this") || slices.Contains(got, "seen-only") {
		t.Errorf("began this month: %v", got)
	}

	// Began last month, resumed now.
	if err := feedback.SetTurn(dir, "s2", feedback.Turn{ID: "p-1", N: 1, Since: filepath.Base(journalFile(dir, monthsBack(1)))}); err != nil {
		t.Fatal(err)
	}
	show("s2", "m-last", monthsBack(1))
	fire(t, "session-start", map[string]any{"session_id": "s2", "source": "resume", "cwd": home})
	fire(t, "user-prompt-submit", map[string]any{"session_id": "s2", "prompt_id": "p-2", "prompt": "next", "cwd": home})
	if got := showIDs(shownInSession(d, "s2")); !slices.Contains(got, "m-last") || slices.Contains(got, "seen-only") {
		t.Errorf("began last month, resumed: %v", got)
	}

	// No pointer: the last 62 days, and the seen ids when they cannot be read.
	show("s3", "m-unknown", time.Time{})
	if got := showIDs(shownInSession(d, "s3")); !slices.Contains(got, "seen-only") {
		t.Errorf("start unknown: %v", got)
	}
}

// The retirement cap counts the session's retirements from the month it
// began: an unreadable month file from before hides none of them.
func TestCapCountsFromSessionStart(t *testing.T) {
	home := hookHome(t)
	d := pathsOf(home)
	dir := d.FeedbackDir()
	unreadableMonth(t, dir, monthsBack(1))
	fire(t, "session-start", map[string]any{"session_id": "s1", "source": "startup", "cwd": home})
	j := feedback.NewJournal(dir, feedback.Common{Session: "s1"})
	for _, e := range []feedback.Event{
		{Kind: feedback.KindRetire, IDs: []string{"m1", "m2"}, Verdict: "wrong"},
		{Kind: feedback.KindHide, IDs: []string{"o1"}, Verdict: "outdated"},
	} {
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if got := retirementsInSession(d, "s1"); got != 3 {
		t.Errorf("%d retirement(s) counted, want 3", got)
	}
}
