package retain

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/transcript"
)

// A segment 1 replaces its document (decision 0050): that is what stores
// once a session two PCs send at the same time (dedup-concurrent). But two
// PCs that both read a document absent and both send its segment 1 before
// the server applies either keep only the later one: the other PC's
// records there are gone (M6 block 4 review, I-1). Hindsight has no
// compare-and-set, so this is repaired after the fact, by the PC that
// lost: it remembers each replacing part it sent (Cursor.Replaces), and a
// later run, once the bank's queue no longer names the document, reads it.
// Its records there: confirmed. Some missing: they go again as the next
// segment, numbered after the server's, and the split is recorded
// (decision 0073, "never lose" over "never store twice"). A mark never
// holds the session's sends, and ends PendingLimit after its send.

// PendingReplace is a part this PC sent that replaces its document (its
// segment 1), not confirmed on the server yet.
type PendingReplace struct {
	Document string `json:"document"`
	// Parts: the records of each segment this PC sent to Document since
	// that replace, first and last: an append applied before the other
	// PC's segment 1 is replaced with it.
	Parts []RecordRange `json:"parts"`
	// Epoch: the bank's restore epoch at the send, or at the last look
	// since a restore; -1 unknown. A restore since starts the looks over
	// against the restored document: the reconcile judges only cursors
	// sent after the backup, and a race before it is in the backup.
	Epoch  int       `json:"epoch"`
	SentAt time.Time `json:"sent_at"`
	// LastAt: when the last part was sent. A record whose time is after it
	// (or missing) was given the send's time (clampTime), which a later
	// read cannot give again: it is matched without its time.
	LastAt time.Time `json:"last_at"`
	// SeenAt: when a run first found this PC's records there, the queue
	// clear. A second look, replaceConfirmGap later, confirms: another
	// PC that read the document absent before this part was applied may
	// have sent its segment 1 just after the first look.
	SeenAt time.Time `json:"seen_at,omitempty"`
}

// Unconfirmed is a replacing part given up PendingLimit after its send,
// unconfirmed: its document and why it could not be confirmed.
type Unconfirmed struct {
	Document string `json:"document"`
	Reason   string `json:"reason"`
}

// RecordRange is the first and last record of one part sent; the part is
// every record of the transcript between them.
type RecordRange struct {
	First string `json:"first"`
	Last  string `json:"last"`
}

const (
	// replaceCheckAfter: a replacing part is first looked at this long
	// after its send (the server's queue takes time; no read in the run
	// that sent it).
	replaceCheckAfter = 5 * time.Minute
	// replaceConfirmGap: the second look that confirms is at least this
	// long after the first (PendingReplace.SeenAt).
	replaceConfirmGap = 5 * time.Minute
)

// maxReplaceReads bounds the documents one run (one Flusher: a tick, a
// detached flush, a backfill) reads to confirm or repair replacing parts;
// the rest wait for the next run.
var maxReplaceReads = 50

// queuedRetains is the bank's queue as a run first read it: the
// documents a pending or processing retain names; all when one names no
// single document or the queue is longer than read; err when unread.
type queuedRetains struct {
	docs map[string]bool
	all  bool
	err  error
}

// queuedRetains reads the bank's queue once per run.
func (f *Flusher) queuedRetains(ctx context.Context) *queuedRetains {
	if f.queue != nil {
		return f.queue
	}
	q := &queuedRetains{docs: map[string]bool{}}
	f.queue = q
	for _, status := range []string{"pending", "processing"} {
		ops, complete, err := f.Client.QueuedOperationsOf(ctx, f.Bank, status, queuePages)
		switch {
		case err != nil:
			q.err = err
			return q
		case !complete:
			q.all = true
			return q
		}
		for _, op := range ops {
			switch {
			case !strings.Contains(op.Type, "retain"):
			case op.DocumentID == "":
				q.all = true
			default:
				q.docs[op.DocumentID] = true
			}
		}
	}
	return q
}

// noteSent records, in cur, the part [first, last] this PC just sent to
// project's document doc as segment segNo; prev is the record the cursor
// was at before it. A segment 1 starts a pending replace; a later one
// joins the pending replace of that document, as one range with the part
// before when it follows it in the transcript (prev is that part's last):
// a long send in chunks is one range.
func (f *Flusher) noteSent(cur *Cursor, project, doc string, segNo int, prev, first, last string, now time.Time) {
	part := RecordRange{First: first, Last: last}
	if segNo == 1 {
		epoch := -1
		if st, err := state.Load(f.Dirs.StateFile()); err == nil {
			epoch = st.EpochOf(f.Bank)
		}
		if cur.Replaces == nil {
			cur.Replaces = map[string]PendingReplace{}
		}
		cur.Replaces[project] = PendingReplace{Document: doc, Parts: []RecordRange{part}, Epoch: epoch, SentAt: now, LastAt: now}
		return
	}
	p, ok := cur.Replaces[project]
	if !ok || p.Document != doc {
		return
	}
	if n := len(p.Parts); n > 0 && p.Parts[n-1].Last == prev {
		p.Parts[n-1].Last = last
	} else {
		p.Parts = append(p.Parts, part)
	}
	p.LastAt = now
	cur.Replaces[project] = p
}

// A look at one pending replace.
type look int

const (
	lookNot     look = iota // could not look: why says it
	lookSpent               // not looked at: this run's reads are spent
	lookAbsent              // the document is not on the server
	lookUnknown             // nothing to compare (no original_text)
	lookPresent             // this PC's records are there
	lookMissing             // some are not
)

// checkReplaces confirms or repairs the replacing parts session sid's
// cursor holds, before the flush. Only the tick, the detached flush and
// backfill flush: no hook reads the server. A part is dropped, never
// written again, when its document is tombstoned (a purge). A restore
// since its send (the bank's epoch changed) starts its looks over against
// the restored document. It is looked at from
// replaceCheckAfter after its send (lookAt):
//   - this PC's records there: seen, then confirmed at a second look;
//   - some missing: sent again (resendReplaced), then dropped; the flush
//     ends there;
//   - anything else (no look possible, absent, nothing to compare): left
//     pending; the session's flush goes on as usual.
//
// PendingLimit after its send, a part ends at its next look whatever it
// gives: found, it is confirmed, missing, sent again, else given up and
// reported in res.Unconfirmed (never quarantined: the session's sends go
// on). A part this run has no read left for is not looked at, so it stays
// pending, even past PendingLimit.
//
// done: the flush ends here with the result returned; otherwise the
// result goes on to the flush.
func (f *Flusher) checkReplaces(ctx context.Context, res Result, sid, path string) (Result, bool) {
	cursorDir := f.Dirs.CursorsDir()
	cur, ok, err := LoadCursor(cursorDir, sid)
	if err != nil || !ok || len(cur.Replaces) == 0 || cur.Quarantined || !state.SameBank(cur.Bank, f.Bank) {
		return res, false
	}
	st, err := state.Load(f.Dirs.StateFile())
	if err != nil {
		return res, false
	}
	epoch, tombs, now := st.EpochOf(f.Bank), st.TombstonesOf(f.Bank), f.Now()
	changed := false
	defer func() {
		if changed {
			_ = SaveCursor(cursorDir, sid, cur)
		}
	}()
	drop := func(p string) { delete(cur.Replaces, p); changed = true }
	tr := &transcriptOnce{path: path, now: now}
	projects := make([]string, 0, len(cur.Replaces))
	for p := range cur.Replaces {
		projects = append(projects, p)
	}
	slices.Sort(projects)
	for _, p := range projects {
		pr := cur.Replaces[p]
		expired := now.Sub(pr.SentAt) >= PendingLimit
		switch {
		case slices.ContainsFunc(tombs, func(t state.Tombstone) bool { return t.Document == pr.Document }):
			drop(p)
			continue
		case pr.Epoch >= 0 && pr.Epoch != epoch:
			// A restore since: a cursor sent after its backup was the
			// reconcile's to judge (dropSession removes it, the mark with
			// it); this one predates the backup, which may hold the other
			// PC's segment 1 alone. Looked at again, twice, against the
			// restored document.
			pr.Epoch, pr.SeenAt = epoch, time.Time{}
			cur.Replaces[p] = pr
			changed = true
		}
		if !expired && now.Sub(pr.SentAt) < replaceCheckAfter {
			continue
		}
		got, why, doc, missing := f.lookAt(ctx, pr, tr)
		switch {
		case got == lookSpent:
			// Left pending even past PendingLimit: a mark is never given
			// up unlooked; the next run reads it (the backlog drains at
			// maxReplaceReads a run).
		case got == lookMissing:
			changed = false // resendReplaced saves the cursor
			return f.resendReplaced(ctx, res, cur, sid, p, pr.Document, doc.Metadata, missing, now), true
		case got == lookPresent && (expired || !pr.SeenAt.IsZero() && now.Sub(pr.SeenAt) >= replaceConfirmGap):
			drop(p)
		case got == lookPresent && pr.SeenAt.IsZero():
			pr.SeenAt = now
			cur.Replaces[p] = pr
			changed = true
		case got != lookPresent && expired:
			drop(p)
			res.Unconfirmed = append(res.Unconfirmed, Unconfirmed{Document: pr.Document, Reason: why})
		}
	}
	return res, false
}

// transcriptOnce reads a transcript's records once, on first need.
type transcriptOnce struct {
	path string
	now  time.Time
	read bool
	recs []transcript.Record
	// raw: the records' times as read, before clampRecords.
	raw []time.Time
	err error
}

func (t *transcriptOnce) records() ([]transcript.Record, []time.Time, error) {
	if t.read {
		return t.recs, t.raw, t.err
	}
	t.read = true
	fh, err := os.Open(t.path)
	if err != nil {
		t.err = err
		return nil, nil, err
	}
	defer func() { _ = fh.Close() }()
	fi, err := fh.Stat()
	if err != nil {
		t.err = err
		return nil, nil, err
	}
	if t.recs, _, t.err = transcript.Read(fh, 0); t.err != nil {
		return nil, nil, t.err
	}
	t.raw = make([]time.Time, len(t.recs))
	for i, r := range t.recs {
		t.raw[i] = r.Timestamp
	}
	clampRecords(t.recs, fi.ModTime(), t.now)
	return t.recs, t.raw, nil
}

// lookAt reads pr's document, once the bank's queue names no retain that
// may write it, and says whether this PC's records are there (missing:
// those that are not, from the transcript tr). why says what kept it from
// looking or judging. At most maxReplaceReads documents per run.
//
// Cost: every read downloads the whole document, original_text included,
// and a new document is usually read twice (seen, then confirmed): on a
// single-PC install too, after a backfill about two reads per session
// sent, 50 a run. Only the tick, the detached flush and backfill read.
func (f *Flusher) lookAt(ctx context.Context, pr PendingReplace, tr *transcriptOnce) (look, string, hindsight.Document, []transcript.Record) {
	var none hindsight.Document
	if f.replaceReads >= maxReplaceReads {
		return lookSpent, "not looked at: this run's reads were spent", none, nil
	}
	switch q := f.queuedRetains(ctx); {
	case q.err != nil:
		return lookNot, "the bank's queue cannot be read: " + errSummary(q.err), none, nil
	case q.all:
		return lookNot, "the bank's queue holds a retain of several documents, or more than is read", none, nil
	case q.docs[pr.Document]:
		return lookNot, "the bank's queue still holds a retain of it", none, nil
	}
	f.replaceReads++
	doc, err := f.Client.GetDocument(ctx, f.Bank, pr.Document)
	var apiErr *hindsight.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
		return lookAbsent, "not on the server, with no purge or restore to explain it", none, nil
	case err != nil:
		return lookNot, "cannot be read: " + errSummary(err), none, nil
	}
	recs, raw, err := tr.records()
	if err != nil {
		return lookNot, "the transcript cannot be read: " + err.Error(), none, nil
	}
	missing, judged := f.missingFrom(pr, doc, recs, raw)
	switch {
	case !judged:
		return lookUnknown, "no original_text to compare", none, nil
	case len(missing) > 0:
		return lookMissing, "", doc, missing
	}
	return lookPresent, "", doc, nil
}

// missingFrom lists the records of pr's parts the server's document d
// lacks, in order (raw: the records' times as read, before clamping).
// judged is false when d gives nothing to compare.
//
// When pr is a lone segment 1 and d's metadata names it (this machine,
// segment 1, its last record), this PC's replace is the last part the
// server applied: nothing is missing. Otherwise a record is there when
// each of its turns' lines, encoded as BuildSegment encodes them, is in
// d's original_text, which Hindsight keeps byte for byte. A record whose
// time was missing or after pr.LastAt was given its send's time
// (clampTime), unknown here: its lines are matched up to their time. A
// change of the redaction rules between the send and the look makes a
// line differ: sent again, stored twice, never lost.
func (f *Flusher) missingFrom(pr PendingReplace, d hindsight.Document, recs []transcript.Record, raw []time.Time) ([]transcript.Record, bool) {
	m := d.Metadata
	if len(pr.Parts) == 1 && f.MachineID != "" && m["machine_id"] == f.MachineID && m["segment"] == "1" && m["last_uuid"] == pr.Parts[0].Last {
		return nil, true
	}
	if d.OriginalText == "" {
		return nil, false
	}
	lastAt := pr.LastAt
	if lastAt.IsZero() {
		lastAt = pr.SentAt
	}
	var missing []transcript.Record
	for _, part := range pr.Parts {
		i, j := indexOf(recs, part.First), indexOf(recs, part.Last)
		if i < 0 || j < i {
			continue // no longer in the transcript: nothing to send again
		}
		for k := i; k <= j; k++ {
			clamped := raw[k].IsZero() || raw[k].After(lastAt)
			if !holdsRecord(d.OriginalText, recs[k], clamped) {
				missing = append(missing, recs[k])
			}
		}
	}
	return missing, true
}

// holdsRecord says whether text holds every turn line of r; clamped: up
// to their time only.
func holdsRecord(text string, r transcript.Record, clamped bool) bool {
	for _, t := range r.Turns {
		l := turnLine(t)
		want := encodeLine(l)
		if clamped {
			l.Timestamp = ""
			want = strings.TrimSuffix(encodeLine(l), "}") + `,"timestamp":"`
		}
		if !strings.Contains(text, want) {
			return false
		}
	}
	return true
}

// resendReplaced sends missing, records of project's document doc another
// PC's segment 1 replaced, as the next segments after the server's (meta:
// the document's metadata as read), and records the split. The pending
// replace goes once they are all sent. Like every send, it goes only to
// the server this run began with (StartedOver). A server segment that is
// not a number quarantines the session, as a send would (movedElsewhere,
// serverPosition).
func (f *Flusher) resendReplaced(ctx context.Context, res Result, cur Cursor, sid, project, doc string, meta map[string]string, missing []transcript.Record, now time.Time) Result {
	base, err := strconv.Atoi(meta["segment"])
	if err != nil || base < 1 {
		return f.quarantine(res, &cur, "the server's document "+doc+" has no valid segment number; sending would replace it")
	}
	if err := f.StartedOver(); err != nil {
		res.Stop = true
		res.Status, res.Detail = Retry, err.Error()
		return res
	}
	if cur.Segments == nil {
		cur.Segments = map[string]int{}
	}
	if cur.LastUUIDs == nil {
		cur.LastUUIDs = map[string]string{}
	}
	if cur.SplitBases == nil {
		cur.SplitBases = map[string]SplitBase{}
	}
	detail := ""
	segNo := base
	for _, part := range chunk(missing) {
		segNo++
		seg := f.segment(cur, sid, doc, part, segNo, project)
		// Not the operation id of the part first sent with these records,
		// which the server collapses as a repeat; the same again for a
		// retry of this resend after the same server segment.
		seg.OperationID = uuid5(nsBacksight, seg.OperationID+"\nresent after "+doc+" segment "+strconv.Itoa(base))
		if err := f.Client.Retain(ctx, f.Bank, []hindsight.RetainItem{seg.Item}, seg.OperationID); err != nil {
			res.FailedDocument = doc
			if refusesDocument(err) {
				return f.quarantine(res, &cur, errSummary(err))
			}
			res.Stop, res.NeedsFix = true, needsFix(err)
			status := Retry
			if res.Records > 0 {
				status = Sent
			}
			return f.save(res, cur, status, "the records another PC's first part replaced are partly sent again; the rest is retried: "+errSummary(err))
		}
		if segNo == base+1 {
			// Until the server applies this part, it still reads at the
			// other PC's segment: this PC's own lag (movedElsewhere).
			cur.SplitBases[project] = SplitBase{Segment: base, LastUUID: meta["last_uuid"]}
			if err := RecordSplit(f.Dirs.State, sid, doc, now); err != nil {
				detail = "sent, but the split was not recorded: " + err.Error()
			}
		}
		last := part[len(part)-1]
		cur.Segments[project], cur.LastUUIDs[project] = segNo, last.UUID
		cur.setDoc(sid, project, doc)
		cur.SentAt = now
		cur.Records += len(part)
		res.Records += len(part)
		res.Segment, res.Chars, res.Project = segNo, res.Chars+seg.Chars, project
		if !slices.Contains(res.Documents, doc) {
			res.Documents = append(res.Documents, doc)
		}
		f.checkpoint(sid, &cur)
	}
	delete(cur.Replaces, project)
	if detail == "" {
		detail = "sent again the records another PC's first part replaced in " + doc
	}
	return f.save(res, cur, Sent, detail)
}
