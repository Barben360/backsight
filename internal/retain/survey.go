package retain

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/transcript"
)

// Candidate is one transcript as backfill sees it.
type Candidate struct {
	Path    string    `json:"path"`
	Session string    `json:"session"`
	Project string    `json:"project"`
	Start   time.Time `json:"start"`
	Chars   int64     `json:"chars"`  // unsent turn text
	Notes   int       `json:"notes"`  // explicit saves to replay
	NoteEst []int64   `json:"-"`      // characters of each save, for the estimate
	Status  string    `json:"status"` // send | notes (sent, explicit saves to replay) | sent | opted_out | empty
	Reason  string    `json:"reason,omitempty"`
}

// Survey reads a transcript and says what backfill would send from it.
func (f *Flusher) Survey(path string) (Candidate, []transcript.Record) {
	sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	c := Candidate{Path: path, Session: sid}
	fh, err := os.Open(path)
	if err != nil {
		c.Status, c.Reason = "empty", err.Error()
		return c, nil
	}
	recs, _, _ := transcript.Read(fh, 0)
	_ = fh.Close()
	if len(recs) == 0 {
		c.Status = "empty"
		return c, nil
	}
	c.Start = recs[0].Timestamp
	c.Project = f.Resolver.Project(recs[0].CWD)
	pending := recs
	replayed := false
	if cur, ok, _ := LoadCursor(f.Dirs.CursorsDir(), sid); ok && !cur.Backfill {
		replayed = cur.NotesReplayed
		pending = nil
		for _, r := range recs {
			if r.Start >= cur.Offset {
				pending = append(pending, r)
			}
		}
	}
	for _, r := range pending {
		for _, t := range r.Turns {
			c.Chars += int64(len(t.Content))
		}
	}
	if paused, why := f.paused(sid); paused {
		c.Status, c.Reason = "opted_out", why
		return c, recs
	}
	if !replayed {
		for _, n := range f.Notes(sid, recs) {
			c.NoteEst = append(c.NoteEst, int64(len(n.Content)))
		}
		c.Notes = len(c.NoteEst)
	}
	switch out, why := f.Resolver.OptedOut(recs[0].CWD, f.Exclude); {
	case out && c.Notes == 0:
		c.Status, c.Reason = "opted_out", why
	case len(pending) == 0 && c.Notes > 0:
		c.Status = "notes"
	case len(pending) == 0:
		c.Status = "sent"
	default:
		c.Status = "send"
	}
	return c, recs
}

// Notes are the explicit saves of a session that may be replayed: none for
// a paused session, none from records in opted-out directories.
func (f *Flusher) Notes(sid string, recs []transcript.Record) []Note {
	paused, _, spans := f.pauseState(sid)
	if paused {
		return nil
	}
	if len(spans) > 0 {
		var kept []transcript.Record
		for _, r := range recs {
			if !session.InSpans(spans, r.Timestamp) {
				kept = append(kept, r)
			}
		}
		recs = kept
	}
	return NotesFromRecords(f.Bank, sid, recs, func(cwd string) (string, bool) {
		if out, _ := f.Resolver.OptedOut(cwd, f.Exclude); out {
			return "", false
		}
		return f.Resolver.Project(cwd), true
	})
}
