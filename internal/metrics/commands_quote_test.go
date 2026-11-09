package metrics

import (
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// Hostile names and ids, as a folder name or a server can give them, and
// each as one word of the shell this PC's commands are pasted in.
const (
	injectName = "R&D $(touch /tmp/pwned)"
	quoteName  = "Bob's notes"
	hostileID  = "m'1$(id)" // an id holds no space
)

func word(t *testing.T, s string) string {
	t.Helper()
	posix := map[string]string{injectName: `'R&D $(touch /tmp/pwned)'`, quoteName: `'Bob'\''s notes'`, hostileID: `'m'\''1$(id)'`}
	ps := map[string]string{injectName: `'R&D $(touch /tmp/pwned)'`, quoteName: `'Bob''s notes'`, hostileID: `'m''1$(id)'`}
	if runtime.GOOS == "windows" {
		return ps[s]
	}
	return posix[s]
}

// TestCommandsQuoteNamesAndIDs: every copyable command the views build
// (the project page's backfill, its memories' read and forget, the review
// lists, the memory page) holds a project name or a memory id as one quoted
// word, never bare: a folder named R&D $(touch …) does not run touch when
// the command is pasted.
func TestCommandsQuoteNamesAndIDs(t *testing.T) {
	x := newFx(t, fxNow)
	for _, name := range []string{injectName, quoteName} {
		x.prompts(name, "p", fxNow.Add(-2*time.Hour), 1, nil)
	}
	x.shows(hoursAgo(1), "s1", "t1", "", injectName, "prompt", injectName, hostileID)
	x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{
		hostileID: {Shown: 12, Credits: 3, CreditsAt: fxNow, LastShown: ago(time.Hour)},
	}})
	for _, name := range []string{injectName, quoteName} {
		p := x.run("projects", projectQuery(x, name)).Tables.Project
		if want := "backsight retain backfill --project " + word(t, name) + " --plan"; p.Command != want {
			t.Errorf("%s: command %q, want %q", name, p.Command, want)
		}
	}
	p := x.run("projects", projectQuery(x, injectName)).Tables.Project
	if len(p.TopUsed) != 1 || p.TopUsed[0].Command != "backsight read "+word(t, hostileID) {
		t.Errorf("top used = %+v", p.TopUsed)
	}

	q := x.query(7)
	q.ID = hostileID
	m := x.run("memories", q).Tables.Memory
	w := word(t, hostileID)
	if want := []string{"backsight read " + w, "backsight forget " + w, "backsight forget --restore " + w}; !slices.Equal(m.Commands, want) {
		t.Errorf("memory page commands = %q, want %q", m.Commands, want)
	}

	x = newFx(t, fxNow)
	x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{
		hostileID:  {Shown: 12},
		"m-hidden": {Shown: 3, Hidden: &feedback.Hidden{Since: ago(10 * day)}},
	}})
	x.shows(hoursAgo(1), "s1", "t1", "", "alpha", "prompt", "alpha", hostileID)
	rv := x.run("bank", x.query(7)).Tables.Review
	if len(rv.NeverUsed) != 1 || rv.NeverUsed[0].Command != "backsight forget "+w {
		t.Errorf("never used = %+v", rv.NeverUsed)
	}
	if len(rv.Hidden) != 1 || rv.Hidden[0].Command != "backsight forget --restore m-hidden" {
		t.Errorf("hidden = %+v, a plain id stays bare", rv.Hidden)
	}
}
