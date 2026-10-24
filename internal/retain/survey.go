package retain

import (
	"os"
	"path/filepath"
	"strings"
	"time"

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
	Status  string    `json:"status"` // send | sent | opted_out | empty
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
	if cur, ok, _ := LoadCursor(f.Dirs.CursorsDir(), sid); ok && !cur.Backfill {
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
	c.Notes = len(NotesFromRecords(f.Bank, sid, c.Project, recs))
	switch out, why := f.Resolver.OptedOut(recs[0].CWD, f.Exclude); {
	case out:
		c.Status, c.Reason = "opted_out", why
	case len(pending) == 0:
		c.Status = "sent"
	default:
		c.Status = "send"
	}
	return c, recs
}
