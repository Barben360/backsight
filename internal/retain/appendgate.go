package retain

import (
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/transcript"
)

// AppendGate holds back an append to a conversation document the server
// already has until enough text waits, or long enough (decision 0083 C1):
// Hindsight re-chunks a whole document on append and extracts again every
// chunk whose text changed, so each small append recreates the tail
// chunk's facts and the observations drawn from them. The first send of a
// document, a resync and a forced flush (SessionEnd, retain flush) are
// never held.
type AppendGate struct {
	MinChars int           // retain.append_min_chars; 0: no gate
	MaxWait  time.Duration // retain.append_max_wait; 0: no gate
}

// Holds says an append of chars rendered characters, to documents last
// sent at lastSent, waits at now. A last send unknown (zero) or later than
// now (the clock was set back since) never holds: the wait could not be
// bounded by MaxWait, and the send that goes records a time before now.
func (g AppendGate) Holds(chars int, lastSent, now time.Time) bool {
	return g.MinChars > 0 && g.MaxWait > 0 && chars < g.MinChars &&
		!lastSent.IsZero() && !lastSent.After(now) && now.Sub(lastSent) < g.MaxWait
}

// Rule is the gate as a waiting reason: "waiting for 30,000 characters or 6 h".
func (g AppendGate) Rule() string {
	return "waiting for " + grouped(g.MinChars) + " characters or " + hours(g.MaxWait)
}

// hours is d as "6 h" when it is whole hours, else Go's form ("1h30m").
func hours(d time.Duration) string {
	if d > 0 && d%time.Hour == 0 {
		return strconv.Itoa(int(d/time.Hour)) + " h"
	}
	return minutes(d)
}

// minutes is d in Go's form without its zero seconds ("1h30m", "45m").
func minutes(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	return s
}

// grouped is n with a comma every three digits.
func grouped(n int) string {
	s := strconv.Itoa(n)
	var b strings.Builder
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 && s[i-1] != '-' {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// RenderedChars is the length of the segment content recs' turns make
// (BuildSegment, without segment 1's REF-ID line): what an append sends.
func RenderedChars(recs []transcript.Record) int {
	n := 0
	for _, r := range recs {
		for _, t := range r.Turns {
			if n > 0 {
				n++ // the newline between two lines
			}
			n += len(encodeLine(turnLine(t)))
		}
	}
	return n
}

// settledCount is how many of recs the settle rule lets go at now
// (decision 0031): all of them once the last is SettleAfter old, else the
// leading ones at least LongOpenAfter old.
func settledCount(recs []transcript.Record, now time.Time) int {
	if len(recs) == 0 || now.Sub(recs[len(recs)-1].Timestamp) >= SettleAfter {
		return len(recs)
	}
	n := 0
	for n < len(recs) && now.Sub(recs[n].Timestamp) >= LongOpenAfter {
		n++
	}
	return n
}

// sentAt is when this PC last sent project p's document: its sent_ats
// entry, or for a cursor an older build wrote, the session's last send.
func (c Cursor) sentAt(p string) time.Time {
	if t, ok := c.SentAts[p]; ok {
		return t
	}
	return c.SentAt
}

// appendHeld says the append gate holds send (the records the settle rule
// lets go, read from a valid cursor cur) back, with the waiting reason.
// It holds only appends: a record bound for a document this cursor never
// wrote (a new project's, or a purge's successor id) sends them all, as
// before. Records held (sent already) or kept out send nothing and count
// for nothing; the last send is the oldest of the documents written to.
func (f *Flusher) appendHeld(cur Cursor, sid string, send []transcript.Record, attrs map[int64]attr, tombs []state.Tombstone, now time.Time) (string, bool) {
	if f.Append.MinChars <= 0 || f.Append.MaxWait <= 0 {
		return "", false
	}
	var out []transcript.Record
	var last time.Time
	for _, r := range send {
		k := f.runKeyOf(sid, r, attrs)
		if k.held || k.optOut {
			continue
		}
		if cur.Segments[k.project] == 0 || cur.docOf(sid, k.project) != LiveDocumentID(sid, cur.Home, k.project, tombs) {
			return "", false // a first send to this document
		}
		at := cur.sentAt(k.project)
		if at.IsZero() {
			return "", false // never known: as before the gate
		}
		if last.IsZero() || at.Before(last) {
			last = at
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return "", false
	}
	chars := RenderedChars(out)
	if !f.Append.Holds(chars, last, now) {
		return "", false
	}
	return f.Append.Rule() + " (" + grouped(chars) + " characters pending, last sent " + minutes(now.Sub(last).Round(time.Minute)) + " ago)", true
}
