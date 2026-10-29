package retain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/redact"
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
// (decision 0073, "never lose" over "never store twice").

// PendingReplace is a part this PC sent that replaces its document (its
// segment 1), not confirmed on the server yet.
type PendingReplace struct {
	Document string `json:"document"`
	// Parts: the records of each segment this PC sent to Document since
	// that replace, first and last: an append applied before the other
	// PC's segment 1 is replaced with it.
	Parts []RecordRange `json:"parts"`
	// Epoch: the bank's restore epoch at the send; -1 unknown. A restore
	// since makes the reconcile the judge of what the server lost.
	Epoch  int       `json:"epoch"`
	SentAt time.Time `json:"sent_at"`
	// SeenAt: when a run first found this PC's records there, the queue
	// clear. A second look, replaceConfirmGap later, confirms: another
	// PC that read the document absent before this part was applied may
	// have sent its segment 1 just after the first look.
	SeenAt time.Time `json:"seen_at,omitempty"`
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

func (q *queuedRetains) names(doc string) bool { return q.err != nil || q.all || q.docs[doc] }

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
// project's document doc as segment segNo: a segment 1 starts a pending
// replace, a later one joins the pending replace of that document.
func (f *Flusher) noteSent(cur *Cursor, project, doc string, segNo int, first, last string, now time.Time) {
	part := RecordRange{First: first, Last: last}
	if segNo == 1 {
		epoch := -1
		if st, err := state.Load(f.Dirs.StateFile()); err == nil {
			epoch = st.EpochOf(f.Bank)
		}
		if cur.Replaces == nil {
			cur.Replaces = map[string]PendingReplace{}
		}
		cur.Replaces[project] = PendingReplace{Document: doc, Parts: []RecordRange{part}, Epoch: epoch, SentAt: now}
		return
	}
	if p, ok := cur.Replaces[project]; ok && p.Document == doc {
		p.Parts = append(p.Parts, part)
		cur.Replaces[project] = p
	}
}

// checkReplaces confirms or repairs the replacing parts session sid's
// cursor holds, before the flush. Only the tick, the detached flush and
// backfill flush: no hook reads the server. A part is looked at once
// replaceCheckAfter passed and the bank's queue names no retain of its
// document:
//   - tombstoned (a purge) or sent under another restore epoch: dropped,
//     never written again;
//   - this PC's records there: seen, then confirmed at a second look;
//   - some missing: sent again (resendReplaced), then dropped;
//   - absent, or holding no text to compare: left pending, dropped after
//     PendingLimit (never recreated);
//   - unreadable: the run stops, nothing is sent, a later run retries.
//
// done: the flush ends here with res.
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
	var recs []transcript.Record
	projects := make([]string, 0, len(cur.Replaces))
	for p := range cur.Replaces {
		projects = append(projects, p)
	}
	slices.Sort(projects)
	for _, p := range projects {
		pr := cur.Replaces[p]
		switch {
		case pr.Epoch >= 0 && pr.Epoch != epoch, slices.ContainsFunc(tombs, func(t state.Tombstone) bool { return t.Document == pr.Document }):
			delete(cur.Replaces, p)
			changed = true
			continue
		case now.Sub(pr.SentAt) < replaceCheckAfter, f.replaceReads >= maxReplaceReads, f.queuedRetains(ctx).names(pr.Document):
			continue
		}
		f.replaceReads++
		doc, err := f.Client.GetDocument(ctx, f.Bank, pr.Document)
		var apiErr *hindsight.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
			if now.Sub(pr.SentAt) >= PendingLimit {
				delete(cur.Replaces, p)
				changed = true
			}
			continue
		case err != nil:
			res.Stop, res.NeedsFix = true, needsFix(err)
			res.FailedDocument = pr.Document
			res.Status, res.Detail = Retry, errSummary(err)
			return res, true
		}
		if recs == nil {
			if recs, err = readRecords(path, now); err != nil {
				continue
			}
		}
		missing, judged := f.missingFrom(pr, doc, recs)
		switch {
		case !judged:
			if now.Sub(pr.SentAt) >= PendingLimit {
				delete(cur.Replaces, p)
				changed = true
			}
		case len(missing) > 0:
			changed = false // resendReplaced saves the cursor
			return f.resendReplaced(ctx, res, cur, sid, p, pr.Document, doc.Metadata, missing, now), true
		case pr.SeenAt.IsZero():
			pr.SeenAt = now
			cur.Replaces[p] = pr
			changed = true
		case now.Sub(pr.SeenAt) >= replaceConfirmGap:
			delete(cur.Replaces, p)
			changed = true
		}
	}
	return res, false
}

// readRecords reads every record of a transcript, their times clamped as
// a flush does.
func readRecords(path string, now time.Time) ([]transcript.Record, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := fh.Stat()
	if err != nil {
		_ = fh.Close()
		return nil, err
	}
	recs, _, err := transcript.Read(fh, 0)
	_ = fh.Close()
	if err != nil {
		return nil, err
	}
	for i := range recs {
		recs[i].Timestamp = clampTime(recs[i].Timestamp, fi.ModTime(), now)
		for j := range recs[i].Turns {
			recs[i].Turns[j].Timestamp = recs[i].Timestamp
		}
	}
	return recs, nil
}

// missingFrom lists the records of pr's parts the server's document d
// lacks, in order. judged is false when d gives nothing to compare.
//
// When pr is a lone segment 1 and d's metadata names it (this machine,
// segment 1, its last record), this PC's replace is the last part the
// server applied: nothing is missing. Otherwise each record is looked for
// in d's original_text, in order: every turn's text, as sent (redacted),
// either as it is or JSON-escaped (the content sent is JSON lines). A
// record whose every turn's text is found after the previous record found
// is there; a short turn repeated elsewhere may read as there.
func (f *Flusher) missingFrom(pr PendingReplace, d hindsight.Document, recs []transcript.Record) ([]transcript.Record, bool) {
	m := d.Metadata
	if len(pr.Parts) == 1 && f.MachineID != "" && m["machine_id"] == f.MachineID && m["segment"] == "1" && m["last_uuid"] == pr.Parts[0].Last {
		return nil, true
	}
	if d.OriginalText == "" {
		return nil, false
	}
	var missing []transcript.Record
	at := 0
	for _, part := range pr.Parts {
		i, j := indexOf(recs, part.First), indexOf(recs, part.Last)
		if i < 0 || j < i {
			continue // no longer in the transcript: nothing to send again
		}
		for _, r := range recs[i : j+1] {
			if next, ok := findRecord(d.OriginalText, at, r); ok {
				at = next
			} else {
				missing = append(missing, r)
			}
		}
	}
	return missing, true
}

// findRecord looks for each of r's turns in text from offset at, in
// order, and returns the offset after the last one found.
func findRecord(text string, at int, r transcript.Record) (int, bool) {
	for _, t := range r.Turns {
		red, _ := redact.Text(t.Content)
		if red == "" {
			continue
		}
		best, n := -1, 0
		for _, form := range []string{red, jsonEscaped(red)} {
			if k := strings.Index(text[at:], form); k >= 0 && (best < 0 || k < best) {
				best, n = k, len(form)
			}
		}
		if best < 0 {
			return at, false
		}
		at += best + n
	}
	return at, true
}

// jsonEscaped is s as BuildSegment's encoder writes it inside a JSON
// string, without the quotes.
func jsonEscaped(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out := strings.TrimSuffix(b.String(), "\n")
	return out[1 : len(out)-1]
}

// resendReplaced sends missing, records of project's document doc another
// PC's segment 1 replaced, as the next segments after the server's (meta:
// the document's metadata as read), and records the split. The pending
// replace goes once they are all sent. Like every send, it goes only to
// the server this run began with (StartedOver).
func (f *Flusher) resendReplaced(ctx context.Context, res Result, cur Cursor, sid, project, doc string, meta map[string]string, missing []transcript.Record, now time.Time) Result {
	base, err := strconv.Atoi(meta["segment"])
	if err != nil || base < 1 {
		res.Status, res.Detail = Retry, "the server's document "+doc+" has no valid segment number; this PC's records it lacks are sent once it has"
		return res
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
