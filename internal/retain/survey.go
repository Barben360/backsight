package retain

import (
	"os"
	"slices"
	"time"

	"github.com/Barben360/backsight/internal/state"
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
	h := f.harnessFor(path)
	sid := h.SessionOf(path)
	c := Candidate{Path: path, Session: sid}
	fh, err := h.Open(path, 0)
	if err != nil {
		c.Status, c.Reason = "empty", err.Error()
		return c, nil
	}
	recs, _, _ := h.ReadConversation(fh, 0)
	_ = fh.Close()
	if len(recs) == 0 {
		c.Status = "empty"
		return c, nil
	}
	c.Start = recs[0].Timestamp
	c.Project = f.Resolver.Project(recs[0].CWD)
	pending := recs
	replayed := false
	if cur, ok, _ := LoadCursor(f.Dirs.CursorsDir(), sid); ok && !cur.Backfill && !f.NoCursors {
		replayed = cur.NotesReplayed
		pending = nil
		for _, r := range recs {
			if r.Start >= cur.Offset {
				pending = append(pending, r)
			}
		}
	}
	// What a purge deleted is never sent again: no cost, nothing to send.
	if tombs, err := f.tombstones(); err == nil && len(tombs) > 0 {
		gone := f.purgedRecords(sid, recs, f.attribute(sid, recs, attr{}), tombs)
		pending = slices.DeleteFunc(slices.Clone(pending), func(r transcript.Record) bool { return gone[r.UUID] })
	}
	for _, r := range pending {
		if f.headless(r.Entrypoint) {
			continue // never sent: no cost
		}
		for _, t := range r.Turns {
			c.Chars += int64(len(t.Content))
		}
	}
	if paused, why, spans := f.pauseState(sid); wholePause(paused, spans) {
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
	case f.headless(recs[0].Entrypoint) && c.Notes == 0:
		c.Status, c.Reason = "opted_out", "headless session (entrypoint "+recs[0].Entrypoint+"): privacy.skip_entrypoints"
	case len(pending) == 0 && c.Notes > 0:
		c.Status = "notes"
	case len(pending) == 0:
		c.Status = "sent"
	default:
		c.Status = "send"
	}
	return c, recs
}

// Why a session has not left this PC yet (Waiter.Reason).
const (
	WaitNotSettled  = "not_settled" // still being written: it settles SettleAfter after its last turn
	WaitDue         = "due"         // settled: the next tick or retain flush sends it
	WaitAppend      = "append_gate" // an append held until enough text or time (AppendGate, Waiter.Detail)
	WaitQuarantined = "quarantined" // set aside after the server refused it: retain retry-quarantine
)

// Waiter is one session that still has records to send.
type Waiter struct {
	Session    string        `json:"session"`
	Transcript string        `json:"transcript"`
	Project    string        `json:"project,omitempty"`
	Reason     string        `json:"reason"`
	Idle       time.Duration `json:"idle_ns,omitempty"` // since its last turn; 0 when quarantined
	// Detail: for WaitAppend, the gate's rule and where the session is
	// ("waiting for 30,000 characters or 6 h (5,120 characters pending, ...)").
	Detail string `json:"detail,omitempty"`
}

// appendWaits says the append gate holds back what the flush would send
// now of recs (the whole transcript at file, size bytes) past cur, by the
// flush's own rules: never for a cursor the flush rebuilds (cursorStale),
// and on the records the settle rule lets go. Approximate in one point:
// the records' times are not clamped to the file's modification time, so a
// turn with no time or a future one (a clock moved back) may read
// differently than in the flush.
func (f *Flusher) appendWaits(cur Cursor, sid, file string, size int64, recs []transcript.Record) (string, bool) {
	tombs, err := f.tombstones()
	if err != nil {
		return "", false
	}
	if stale, err := f.cursorStale(cur, sid, file, size, tombs); err != nil || stale {
		return "", false // the flush resyncs: never held
	}
	var pending []transcript.Record
	for _, r := range recs {
		if r.Start >= cur.Offset {
			pending = append(pending, r)
		}
	}
	now := f.Now()
	send := pending[:settledCount(pending, now)]
	if len(send) == 0 {
		return "", false
	}
	attrs := f.attribute(sid, pending, attr{project: cur.Project, namedOut: cur.OptOut, namedOutDir: cur.OptOutDir})
	return f.appendHeld(cur, sid, send, attrs, tombs, now)
}

// Waiting lists the sessions of files that live retain still has records
// of to send, settled or not, or has set aside, with why, from the
// transcripts and cursors alone (no server call). Backfill's sessions
// (started before retain_since) are not listed; a transcript its cursor
// reached costs a stat and the cursor.
func (f *Flusher) Waiting(files []string) []Waiter {
	var out []Waiter
	for _, file := range files {
		fi, err := os.Stat(file)
		if err != nil {
			continue
		}
		h := f.harnessFor(file)
		sid := h.SessionOf(file)
		cur, ok, err := LoadCursor(f.Dirs.CursorsDir(), sid)
		size := fi.Size()
		if ok && !isClaude(h) {
			if size, err = h.Size(file); err != nil {
				continue
			}
		}
		switch {
		case err != nil:
			continue
		case ok && cur.Quarantined:
			out = append(out, Waiter{Session: sid, Transcript: file, Reason: WaitQuarantined})
			continue
		case ok && (cur.Backfill || cur.Offset >= size):
			continue
		case !ok && !f.RetainSince.IsZero() && fi.ModTime().Before(f.RetainSince):
			continue // last written before retain_since: backfill's
		}
		c, recs := f.Survey(file)
		if c.Status != "send" || (!ok && !f.RetainSince.IsZero() && recs[0].Timestamp.Before(f.RetainSince)) {
			continue
		}
		w := Waiter{Session: sid, Transcript: file, Project: c.Project, Reason: WaitDue}
		if f.Now != nil {
			w.Idle = max(0, f.Now().Sub(recs[len(recs)-1].Timestamp))
			if w.Idle < SettleAfter {
				w.Reason = WaitNotSettled
			}
			if ok {
				if detail, held := f.appendWaits(cur, sid, file, fi.Size(), recs); held {
					w.Reason, w.Detail = WaitAppend, detail
				}
			}
		}
		out = append(out, w)
	}
	return out
}

// CaughtUp says live retain has nothing of the transcript at file left to
// send: its cursor, readable and not set aside, reached the end of the file
// (Waiting skips it the same way). A transcript with no cursor, or one that
// cannot be read, is not caught up.
func (f *Flusher) CaughtUp(file string) bool {
	size, err := f.harness().Size(file)
	if err != nil {
		return false
	}
	cur, ok, err := LoadCursor(f.Dirs.CursorsDir(), f.harness().SessionOf(file))
	return err == nil && ok && !cur.Quarantined && cur.Offset >= size
}

// Unsent counts the sessions Waiting lists that a send can still move:
// settled or not, not the quarantined ones (counted apart).
func (f *Flusher) Unsent(files []string) int { return len(f.UnsentSessions(files)) }

// UnsentSessions are the ids of the sessions Unsent counts, sorted; never
// nil (none is an empty list).
func (f *Flusher) UnsentSessions(files []string) []string {
	out := []string{}
	for _, w := range f.Waiting(files) {
		if w.Reason != WaitQuarantined {
			out = append(out, w.Session)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Notes are the explicit saves of a session that may be replayed, recs
// being the whole transcript: none from records kept out (a pause, an
// opted-out directory, or a directory that inherits a kept-out project),
// and a save without a project gets its record's project by the same rule
// as the session's text (attribute), not its directory's basename. None
// either from records a purge deleted, nor a save whose note was purged
// (its document, or the same text stored live under another id); with
// tombstones that cannot be read, none at
// all: a purge must not come back through a replay.
func (f *Flusher) Notes(sid string, recs []transcript.Record) []Note {
	tombs, err := f.tombstones()
	if err != nil {
		return nil
	}
	attrs := f.attribute(sid, recs, attr{})
	gone := f.purgedRecords(sid, recs, attrs, tombs)
	notes := NotesFromRecords(f.Bank, sid, recs, f.Attribution.Transverse, func(r transcript.Record) (string, bool) {
		a := attrs[r.End]
		return a.project, !a.out && !gone[r.UUID]
	})
	return slices.DeleteFunc(notes, func(n Note) bool {
		h := ContentHash(n.Content)
		return slices.ContainsFunc(tombs, func(t state.Tombstone) bool { return t.Document == n.DocumentID || t.ContentSHA256 == h })
	})
}

// purgedRecords are the uuids of recs (a whole transcript) that a purge
// deleted from the server.
func (f *Flusher) purgedRecords(sid string, recs []transcript.Record, attrs map[int64]attr, tombs []state.Tombstone) map[string]bool {
	p := purgedOf(sid, recs, tombs)
	gone := map[string]bool{}
	for i, r := range recs {
		if p.holds(i, attrs[r.End].project) {
			gone[r.UUID] = true
		}
	}
	return gone
}
