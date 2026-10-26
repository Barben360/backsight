package retain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/transcript"
)

// Timing of settled retain (decision 0031).
const (
	SettleAfter   = 30 * time.Minute
	LongOpenAfter = 2 * time.Hour
	lockStaleness = 10 * time.Minute
)

// Statuses of one flush.
const (
	Sent        = "sent"
	Nothing     = "nothing"     // no new record
	Waiting     = "waiting"     // not settled yet
	OptedOut    = "opted_out"   // advanced without sending
	Retry       = "retry"       // server busy or unreachable: next tick
	Quarantined = "quarantined" // refused by the server or unknown position
	Locked      = "locked"      // another flusher holds the session
	Maintenance = "maintenance" // maintenance lock held
	Backfill    = "backfill"    // started before retain_since: backfill's job
	Held        = "held"        // quarantined earlier, waiting for retry-quarantine
)

// refusesDocument says the server rejected this document itself (bad
// request, too large, unprocessable): quarantine it. Every other failure
// (auth, redirect, missing bank, conflict, rate limit, server error,
// network) is about the server or setup: stop the run and retry later.
func refusesDocument(err error) bool {
	var apiErr *hindsight.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Status {
	case 400, 413, 422:
		return true
	}
	return false
}

// Flusher sends settled sessions of one machine to one bank.
type Flusher struct {
	Client   *hindsight.Client
	Bank     string
	Dirs     paths.Dirs
	Resolver *project.Resolver
	Exclude  []string
	// Headless says the records of an entrypoint are never retained
	// (privacy.skip_entrypoints, decision 0071); nil keeps every record.
	Headless    func(entrypoint string) bool
	MachineID   string
	Version     string
	Now         func() time.Time
	RetainSince time.Time // zero: no boundary
	Index       *SentIndex
	// Hold: the bank's restore epoch could not be reconciled (Reconcile):
	// nothing is sent until a later run reconciles (decision 0044).
	Hold string
	// Outbox takes the subagent reports a reconcile rebuilds; zero: the
	// notes outbox of Dirs (NotesOutbox).
	Outbox Outbox
	// NoReports: components.write is off, so SubagentStop keeps no
	// report and a reconcile rebuilds none.
	NoReports bool
	// ReportProject names a rebuilt report's project from its directory as
	// SubagentStop does; nil: Resolver.Project.
	ReportProject func(cwd string) string
	// Attribution records each main turn's path-rule answer beside the
	// applied one, and decides rebuilt reports' projects as SubagentStop
	// does (decision 0072); a zero Dir records nothing.
	Attribution Attributor
}

// Options change the timing rules for explicit flushes.
type Options struct {
	Force       bool // send every pending record, settled or not
	IgnoreSince bool // the session was named explicitly
}

// Result is what one flush did.
type Result struct {
	Session    string `json:"session"`
	Transcript string `json:"transcript"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	Records    int    `json:"records,omitempty"`
	Segment    int    `json:"segment,omitempty"`
	Chars      int    `json:"chars,omitempty"`
	Project    string `json:"project,omitempty"`
	// Documents are the documents this flush wrote a segment to, in order;
	// FailedDocument is the one whose send failed (retried or refused).
	Documents      []string `json:"documents,omitempty"`
	FailedDocument string   `json:"failed_document,omitempty"`
	// Unknown counts the records of types Claude Code is not known to write
	// (transcript.Known) that this flush moved the cursor past: each is
	// counted once, however often the file is read.
	Unknown map[string]int `json:"unknown,omitempty"`
	read    transcript.Stats
	// Stop tells the caller the server or setup is failing: later sessions
	// would fail the same way, so the run ends here.
	Stop bool `json:"-"`
	// NeedsFix: the server refused backsight itself (credentials, redirect,
	// missing bank); retrying cannot help until the user fixes it.
	NeedsFix bool `json:"-"`
	// Turns are the main turns this flush recorded (Flusher.Attribution).
	Turns []TurnEntry `json:"-"`
}

// FlushFile sends what is settled and unsent in one transcript.
func (f *Flusher) FlushFile(ctx context.Context, path string, opt Options) (res Result) {
	sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	res = Result{Session: sid, Transcript: path}
	fail := func(status, detail string) Result { res.Status, res.Detail = status, detail; return res }

	if state.MaintenanceHeld(f.Dirs.MaintenanceLock()) {
		return fail(Maintenance, "maintenance in progress")
	}
	if err := f.restoreHold(); err != nil {
		res.Stop = true // every session waits for the restore to finish
		return fail(Retry, err.Error())
	}
	if f.Hold != "" {
		res.Stop = true // every session waits for the same reconcile
		return fail(Retry, f.Hold)
	}
	unlock, err := f.lockSession(sid)
	if err != nil {
		return fail(Locked, err.Error())
	}
	defer unlock()
	// Under the session's lock, whatever the flush did: the path rule's
	// shadow answers are local and never wait for the server.
	defer func() { res.Turns = f.recordTurns(sid, path, opt.Force) }()

	cursorDir := f.Dirs.CursorsDir()
	cur, haveCursor, err := LoadCursor(cursorDir, sid)
	if err != nil {
		return fail(Retry, err.Error())
	}
	if cur.Quarantined {
		return fail(Held, "quarantined earlier: backsight retain retry-quarantine")
	}
	if haveCursor && cur.Backfill && !opt.IgnoreSince {
		return fail(Backfill, "started before live retain began; backfill sends it")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fail(Retry, err.Error())
	}
	// A cursor without Segments predates per-project documents: its
	// Segment counts every project's runs, all sent to conversation:<sid>.
	resync := !haveCursor || cur.Bank != f.Bank || cur.Backfill || cur.Segments == nil || fi.Size() < cur.Offset
	if haveCursor {
		fp, err := Fingerprint(path, cur.Offset)
		if err != nil {
			return fail(Retry, err.Error())
		}
		resync = resync || cur.Fingerprint != fp
	}
	tombs, err := f.tombstones()
	if err != nil {
		return fail(Retry, err.Error())
	}
	// A purge deleted a document this cursor appends to: its segments
	// start again in the successor id, which the resync reads.
	for p := range cur.Segments {
		resync = resync || cur.docOf(sid, p) != LiveDocumentID(sid, cur.Home, p, tombs)
	}
	from := cur.Offset
	if resync {
		from = 0
	}
	fh, err := os.Open(path)
	if err != nil {
		return fail(Retry, err.Error())
	}
	if _, err := fh.Seek(from, 0); err != nil {
		_ = fh.Close()
		return fail(Retry, err.Error())
	}
	recs, st, err := transcript.Read(fh, from)
	_ = fh.Close()
	if err != nil {
		return fail(Retry, err.Error())
	}
	res.read = st
	now := f.Now()
	for i := range recs {
		recs[i].Timestamp = clampTime(recs[i].Timestamp, fi.ModTime(), now)
		for j := range recs[i].Turns {
			recs[i].Turns[j].Timestamp = recs[i].Timestamp
		}
	}

	// Read before the cursor is rebuilt: the projects whose last sends
	// went to a purged id (lostToPurged).
	var lostTo map[string]time.Time
	if haveCursor {
		lostTo = lostToPurged(sid, cur, tombs)
	}
	if resync {
		// A cursor in the per-project layout keeps its home: the documents
		// already written are named after it. Only a session with no such
		// cursor names its home from the records.
		home := ""
		if haveCursor && cur.Segments != nil {
			home = cur.Home
		}
		cur = Cursor{Transcript: path, Bank: f.Bank, Home: home, Segments: map[string]int{}, LastUUIDs: map[string]string{}}
		if len(recs) == 0 {
			return fail(Nothing, "")
		}
	}
	if cur.Segments == nil {
		cur.Segments = map[string]int{}
	}
	if cur.LastUUIDs == nil {
		cur.LastUUIDs = map[string]string{}
	}
	attrs := f.attribute(sid, recs, attr{project: cur.Project, namedOut: cur.OptOut, namedOutDir: cur.OptOutDir})
	if resync {
		var done bool
		if recs, res, done = f.resync(ctx, res, &cur, sid, recs, attrs, opt, now, lostTo); done {
			return res
		}
	}

	if len(recs) == 0 {
		cur.Offset = max(cur.Offset, st.Consumed)
		return f.save(res, cur, Nothing, "")
	}
	send := recs
	if !opt.Force && now.Sub(recs[len(recs)-1].Timestamp) < SettleAfter {
		n := 0
		for n < len(recs) && now.Sub(recs[n].Timestamp) >= LongOpenAfter {
			n++
		}
		if n == 0 {
			return f.save(res, cur, Waiting, "last turn "+now.Sub(recs[len(recs)-1].Timestamp).Round(time.Minute).String()+" ago")
		}
		send = recs[:n]
	}
	return f.sendRuns(ctx, res, cur, sid, send, attrs, tombs, now)
}

// resync rebuilds the cursor from the server's documents (decision 0038):
// it returns the records left to send, from the first one the server lacks,
// or done with the flush's result. Positions are per document: a record is
// held when its own project's document holds it (its last_uuid is at or
// after it), or when conversation:<sid> still holds every project up to
// its legacy position. The records left that this session already sent are
// marked held in the sent index; sendRuns skips them, in this flush and in
// the next one should this one stop half way.
//
// lostTo are the projects whose last sends, as the cursor read before
// this resync says, went to a purged id (lostToPurged): this session's own
// records there written after the purge go again; the earlier ones stay
// held (lostAfter).
func (f *Flusher) resync(ctx context.Context, res Result, cur *Cursor, sid string, recs []transcript.Record, attrs map[int64]attr, opt Options, now time.Time, lostTo map[string]time.Time) ([]transcript.Record, Result, bool) {
	retry := func(detail string) ([]transcript.Record, Result, bool) {
		res.Status, res.Detail = Retry, detail
		return nil, res, true
	}
	// sent: already out, from this machine. The fork guard (a bare uuid
	// in the index) does not count for the records a restore lost of
	// this session's own sends (its .dropped mark), unless sent again.
	lostOwn := f.loadDropped(sid)
	cur.Generation = max(cur.Generation, lostOwn.Generation, f.generation(sid))
	own := func(u string) bool { return f.Index.Has(sessionKey(f.Bank, sid, u)) }
	sent := func(u string) bool { return f.Index.Has(u) && (!lostOwn.Records[u] || own(u)) }
	tombs, err := f.tombstones()
	if err != nil {
		return retry(err.Error())
	}
	known := map[string]serverDoc{}
	if cur.Home == "" {
		// No cursor to keep the home from: conversation:<sid> says which
		// project it was sent for, which the directories may no longer say
		// (a linked worktree removed since), or its tombstone does once
		// purged; else the records name it.
		base, err := f.readDocument(ctx, DocumentID(sid, "", ""))
		if err != nil {
			res.Stop, res.NeedsFix = true, needsFix(err)
			return retry(errSummary(err))
		}
		if cur.Home = base.named; cur.Home == "" && !base.found {
			cur.Home = homeOfPurged(sid, tombs)
		}
		if cur.Home == "" {
			cur.Home = homeOf(recs, attrs)
		}
		known[base.id] = base
	}
	cur.Document = LiveDocumentID(sid, cur.Home, cur.Home, tombs)
	docs, err := f.readDocuments(ctx, sid, cur.Home, projectsOf(cur.Home, recs, attrs), known, tombs)
	if err != nil {
		res.Stop, res.NeedsFix = true, needsFix(err)
		return retry(errSummary(err))
	}
	pos, bad := serverPosition(recs, docs, cur.Document)
	if bad != "" {
		return nil, f.quarantine(res, cur, bad), true
	}
	// What a purge deleted counts as sent: held like what the server holds,
	// though no segment of it is left to append to.
	pos.purged = purgedOf(sid, recs, tombs)
	for p, seg := range pos.segments {
		cur.Segments[p], cur.LastUUIDs[p] = seg, pos.last[p]
		cur.setDoc(sid, p, LiveDocumentID(sid, cur.Home, p, tombs))
	}
	cur.Legacy = pos.legacyUUID
	// Released from quarantine after a first segment the server never
	// stored: that document is resent whole, from its segment 1.
	resend := f.pendingMark(sid) == resendMark
	start := -1 // the first record to send
	var mark, unmark, resent []string
	lostIn := func(r transcript.Record, p string) bool { in, _ := lostAfter(lostTo, p, r); return in }
	for i, r := range recs {
		a, key := attrs[r.End], sessionKey(f.Bank, sid, r.UUID)
		_, found := pos.at[a.project]
		held := false
		switch {
		case pos.holds(i, a.project):
			held = true
			// Sent from another machine, or the index was lost: marked as
			// this session's own. Not a record whose bare uuid is in the
			// index: a fork's copy of its parent's records, or a record
			// kept out, which a restore must never resend as this session's.
			if !own(r.UUID) && !a.out && !sent(r.UUID) {
				mark = append(mark, key)
			}
		case start < 0 && sent(r.UUID) && !own(r.UUID):
			held = true // a fork copies its parent's records (decision 0038)
		case a.out:
			// never sent: sendRuns keeps it out again
		case own(r.UUID) && lostIn(r, a.project):
			// Sent to the purged id, which is purged again (or will be).
			// Written after the purge: an older binary's turn, it goes again
			// to the live document. Written before: the purge was meant to
			// cover it (its retain was still queued), it stays held.
			if _, after := lostAfter(lostTo, a.project, r); after {
				unmark, resent = append(unmark, key), append(resent, r.UUID)
			} else {
				held = true
			}
		case own(r.UUID) && found:
			// Sent, but missing from the server's metadata (async retain
			// still running, or a crash before the cursor was saved):
			// never send it twice.
			held = true
		case own(r.UUID) && resend:
			unmark, resent = append(unmark, key), append(resent, r.UUID)
		case own(r.UUID):
			// This session's own record went out, but its document is not
			// readable yet (its first async retain is still running): a new
			// segment 1 would replace it (issue 101). After PendingLimit
			// the retain is taken as lost.
			if f.waitedTooLong(sid, now) {
				return nil, f.quarantine(res, cur, "the server never stored the first segment of "+LiveDocumentID(sid, cur.Home, a.project, tombs)+"; retain retry-quarantine resends the session"), true
			}
			return retry("the server has not stored this session's earlier segment yet; retried later")
		}
		if !held && start < 0 {
			start = i
		}
	}
	if err := f.Index.Add(mark...); err != nil {
		return retry(err.Error())
	}
	// The records resent lose their session key; the .dropped mark keeps
	// them this session's own to resend (as after a restore), should this
	// resend be refused or stopped: without it, their bare uuid alone would
	// read as a fork's copy, and they would never go out.
	if len(resent) > 0 {
		lost := dropped{Records: map[string]bool{}}
		for _, u := range resent {
			lost.Records[u] = true
		}
		if err := f.addDropped(sid, lost); err != nil {
			return retry(err.Error())
		}
	}
	if err := f.Index.Remove(unmark...); err != nil {
		return retry(err.Error())
	}
	if len(unmark) == 0 {
		// No document lacks a record this session sent: a pending first
		// segment landed, or a resend mark has nothing left to resend. A
		// stale mark would later resend a merely pending document as
		// segment 1 (replacing it), or quarantine it at once.
		_ = os.Remove(pendingPath(f.Dirs.CursorsDir(), sid))
	}
	if start < 0 {
		start = len(recs)
	}
	if start > 0 {
		last := recs[start-1]
		cur.advance(last, attrs[last.End])
	}
	recs = recs[start:]
	if !pos.found && !resend && len(recs) > 0 && !opt.IgnoreSince && !lostOwn.PastSince && !f.RetainSince.IsZero() && recs[0].Timestamp.Before(f.RetainSince) {
		cur.Backfill = true
		return nil, f.save(res, *cur, Backfill, "started before live retain began; backfill sends it"), true
	}
	return recs, res, false
}

// advance moves the cursor past r, whose attribution a carries to the next
// record.
func (c *Cursor) advance(r transcript.Record, a attr) {
	c.Offset, c.LastUUID, c.LastTurnAt = r.End, r.UUID, r.Timestamp
	c.Project, c.OptOut, c.OptOutDir = a.project, a.namedOut, a.namedOutDir
}

// runKey groups consecutive records that go out together: same project
// (so same document) and same opt-out answer.
type runKey struct {
	held    bool // already on the server, sent by this session
	optOut  bool
	reason  string
	project string
}

// sendRuns sends records in runs of one project, each to its project's
// document, skipping held runs (sent already: the sent index's session key)
// and opted-out ones (attribute). It stops at the first failure, with the
// cursor after the last run done; the cursor is saved after every segment.
func (f *Flusher) sendRuns(ctx context.Context, res Result, cur Cursor, sid string, send []transcript.Record, attrs map[int64]attr, tombs []state.Tombstone, now time.Time) Result {
	key := func(r transcript.Record) runKey {
		a := attrs[r.End]
		switch {
		case f.Index.Has(sessionKey(f.Bank, sid, r.UUID)):
			return runKey{held: true}
		case a.out:
			return runKey{optOut: true, reason: a.why}
		case f.headless(r.Entrypoint):
			return runKey{optOut: true, reason: "headless session (entrypoint " + r.Entrypoint + "): privacy.skip_entrypoints"}
		}
		return runKey{project: a.project}
	}
	status, detail := Nothing, ""
	for len(send) > 0 {
		k := key(send[0])
		n := 1
		for n < len(send) && key(send[n]) == k {
			n++
		}
		run := send[:n]
		send = send[n:]
		last := run[len(run)-1]
		switch {
		case k.held:
			cur.advance(last, attrs[last.End])
			continue
		case k.optOut:
			// Handled for good: a fork's copy of these records skips them
			// with the ones already sent (the fork guard's leading run),
			// instead of sending them or stopping its run there.
			handled := make([]string, 0, len(run))
			for _, r := range run {
				handled = append(handled, r.UUID)
			}
			_ = f.Index.Add(handled...)
			cur.advance(last, attrs[last.End])
			if status != Sent {
				status, detail = OptedOut, k.reason
			}
			continue
		}
		for _, part := range chunk(run) {
			var done bool
			if res, cur, status, detail, done = f.sendPart(ctx, res, cur, sid, part, k.project, status, detail, tombs, now); done {
				return res
			}
			f.checkpoint(res.Session, &cur)
		}
	}
	return f.save(res, cur, status, detail)
}

func (f *Flusher) headless(entrypoint string) bool {
	return f.Headless != nil && f.Headless(entrypoint)
}

// sendPart sends one segment to project's document (never one a purge
// deleted: LiveDocumentID); done reports that the flush ended (error).
func (f *Flusher) sendPart(ctx context.Context, res Result, cur Cursor, sid string, run []transcript.Record, project, status, detail string, tombs []state.Tombstone, now time.Time) (Result, Cursor, string, string, bool) {
	last := run[len(run)-1]
	doc := LiveDocumentID(sid, cur.Home, project, tombs)
	segNo := cur.Segments[project] + 1
	if cur.Segments[project] > 0 && cur.docOf(sid, project) != doc {
		segNo = 1 // the segments counted were of a purged document
	}
	seg := BuildSegment(f.Bank, sid, doc, run, segNo, project, f.MachineID, f.Version)
	if cur.Generation > 0 {
		seg.OperationID = uuid5(nsBacksight, seg.OperationID+"\n"+strconv.Itoa(cur.Generation))
	}
	if doc == cur.Document && cur.Legacy != "" {
		// carried forward: a later resync still knows that this document
		// holds every project's records up to there
		seg.Item.Metadata["legacy_last_uuid"] = cur.Legacy
	}
	if err := f.Client.Retain(ctx, f.Bank, []hindsight.RetainItem{seg.Item}, seg.OperationID); err != nil {
		res.FailedDocument = doc
		if refusesDocument(err) {
			return f.quarantine(res, &cur, errSummary(err)), cur, status, detail, true
		}
		res.Stop, res.NeedsFix = true, needsFix(err)
		if status == Sent {
			return f.save(res, cur, Sent, "partly sent; the rest is retried: "+errSummary(err)), cur, status, detail, true
		}
		return f.save(res, cur, Retry, errSummary(err)), cur, status, detail, true
	}
	uuids := make([]string, 0, 2*len(run))
	for _, r := range run {
		uuids = append(uuids, r.UUID, sessionKey(f.Bank, sid, r.UUID)) // fork guard, then "sent in this session"
	}
	if err := f.Index.Add(uuids...); err != nil {
		detail = "sent, but the fork-guard index was not updated: " + err.Error()
	}
	cur.advance(last, attr{project: project})
	cur.SentAt = now
	cur.Segments[project], cur.LastUUIDs[project] = segNo, last.UUID
	cur.setDoc(sid, project, doc)
	cur.Records += len(run)
	res.Records += len(run)
	if !slices.Contains(res.Documents, doc) {
		res.Documents = append(res.Documents, doc)
	}
	res.Segment, res.Chars, res.Project = segNo, res.Chars+seg.Chars, project
	return res, cur, Sent, detail, false
}

// checkpoint saves the cursor between two segments: a crash later in the
// flush must not resend what went out, with other chunks and operation
// ids. A cursor that cannot be saved is removed: the next flush then
// resyncs from the server and the sent index instead of trusting it.
func (f *Flusher) checkpoint(sid string, cur *Cursor) {
	if fp, err := Fingerprint(cur.Transcript, cur.Offset); err == nil {
		cur.Fingerprint = fp
	}
	cur.Segment = cur.Segments[cur.Home]
	if err := SaveCursor(f.Dirs.CursorsDir(), sid, *cur); err != nil {
		if p, perr := cursorPath(f.Dirs.CursorsDir(), sid); perr == nil {
			_ = os.Remove(p)
		}
	}
}

func (f *Flusher) save(res Result, cur Cursor, status, detail string) Result {
	res.Status, res.Detail = status, detail
	if u := res.read.UnknownBefore(cur.Offset); len(u) > 0 {
		res.Unknown = u
	}
	if status == Sent {
		_ = os.Remove(pendingPath(f.Dirs.CursorsDir(), res.Session)) // the document exists now
		f.pruneDropped(res.Session)
	}
	if fp, err := Fingerprint(cur.Transcript, cur.Offset); err == nil {
		cur.Fingerprint = fp
	}
	if cur.Segments != nil {
		cur.Segment = cur.Segments[cur.Home] // what an older build appends to
	}
	if err := SaveCursor(f.Dirs.CursorsDir(), res.Session, cur); err != nil && status == Sent {
		// A stale cursor would resend; without one the next flush resyncs
		// from the server and the sent index.
		if p, perr := cursorPath(f.Dirs.CursorsDir(), res.Session); perr == nil {
			_ = os.Remove(p)
		}
		res.Detail = "sent, but the cursor was not saved: " + err.Error()
	}
	return res
}

// clampTime gives a missing or future time (a clock moved back) the file's
// modification time, itself never later than now.
func clampTime(t, mtime, now time.Time) time.Time {
	if mtime.After(now) {
		mtime = now
	}
	if t.IsZero() || t.After(now) {
		return mtime
	}
	return t
}

// Segment caps (a large backlog goes out as several appends).
var (
	maxSegmentRecords = 400
	maxSegmentChars   = 900_000
)

// chunk splits records into segments under the caps.
func chunk(recs []transcript.Record) [][]transcript.Record {
	var out [][]transcript.Record
	start, chars := 0, 0
	for i, r := range recs {
		n := 0
		for _, t := range r.Turns {
			n += len(t.Content)
		}
		if i > start && (i-start >= maxSegmentRecords || chars+n > maxSegmentChars) {
			out = append(out, recs[start:i])
			start, chars = i, 0
		}
		chars += n
	}
	return append(out, recs[start:])
}

func (f *Flusher) quarantine(res Result, cur *Cursor, why string) Result {
	cur.Quarantined = true
	entry, _ := json.Marshal(map[string]string{"ts": f.Now().UTC().Format(time.RFC3339), "session": res.Session, "reason": why})
	p := filepath.Join(f.Dirs.QuarantineDir(), res.Session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
		if fh, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
			_, _ = fh.Write(append(entry, '\n'))
			_ = fh.Close()
		}
	}
	return f.save(res, *cur, Quarantined, why)
}

// pauses tells, per record, whether a pause keeps it out. A record is
// judged by its own session's pause as well as the file's: a fork or a
// resumed session copies its parent's records, which keep the parent's
// session id, and a stretch the parent paused must stay out of the copy.
func (f *Flusher) pauses(fileSID string) func(transcript.Record) (bool, string) {
	type state struct {
		whole bool
		why   string
		spans []session.Span
	}
	cache := map[string]state{}
	get := func(sid string) state {
		if st, ok := cache[sid]; ok {
			return st
		}
		paused, why, spans := f.pauseState(sid)
		st := state{wholePause(paused, spans), why, spans}
		cache[sid] = st
		return st
	}
	return func(r transcript.Record) (bool, string) {
		sids := []string{fileSID}
		if r.SessionID != "" && r.SessionID != fileSID && session.ValidID(r.SessionID) {
			sids = append(sids, r.SessionID)
		}
		for _, sid := range sids {
			st := get(sid)
			switch {
			case st.whole:
				return true, st.why
			case session.InSpans(st.spans, r.Timestamp):
				return true, "typed while the session was paused"
			}
		}
		return false, ""
	}
}

// wholePause: a paused session with no open stretch (a state written before
// stretches existed, or unreadable) keeps every record out. With an open
// stretch, only the records typed after the pause are out: the turns before
// it are sent as usual.
func wholePause(paused bool, spans []session.Span) bool {
	return paused && (len(spans) == 0 || !spans[len(spans)-1].To.IsZero())
}

// pauseState is the pause flag and the paused stretches of the session:
// records typed during a stretch stay out even after a resume.
func (f *Flusher) pauseState(sid string) (bool, string, []session.Span) {
	var s struct {
		Paused bool           `json:"paused"`
		Spans  []session.Span `json:"paused_spans"`
	}
	raw, err := os.ReadFile(filepath.Join(f.Dirs.SessionsDir(), sid+".json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, "", nil
	case err != nil || json.Unmarshal(raw, &s) != nil:
		return true, "session state unreadable: treated as paused", nil
	}
	return s.Paused, "session paused", s.Spans
}

// PendingLimit: how long a flush waits for a first segment the server has
// not stored yet before quarantining the session.
const PendingLimit = 24 * time.Hour

// resendMark in <sid>.pending: the session was released from quarantine
// and is resent whole.
const resendMark = "resend"

func pendingPath(cursorDir, sid string) string { return filepath.Join(cursorDir, sid+".pending") }

func (f *Flusher) pendingMark(sid string) string {
	raw, _ := os.ReadFile(pendingPath(f.Dirs.CursorsDir(), sid))
	return strings.TrimSpace(string(raw))
}

// waitedTooLong records when the flush first found sid's first segment
// pending and says whether that is longer ago than PendingLimit.
func (f *Flusher) waitedTooLong(sid string, now time.Time) bool {
	if mark := f.pendingMark(sid); mark != "" {
		since, err := time.Parse(time.RFC3339, mark)
		return err == nil && now.Sub(since) >= PendingLimit
	}
	_ = fsutil.WriteAtomic(pendingPath(f.Dirs.CursorsDir(), sid), []byte(now.UTC().Format(time.RFC3339)), 0o600)
	return false
}

// lockSession takes the per-session lock, taking over one whose owner died
// or that is older than lockStaleness (decision 0050).
func (f *Flusher) lockSession(sid string) (func(), error) {
	p := filepath.Join(f.Dirs.LocksDir(), "session-"+sid+".lock")
	l, err := state.TryAcquire(p, "retain flush")
	var held *state.HeldError
	if errors.As(err, &held) {
		if !held.Stale && f.Now().Sub(held.Owner.Since) > lockStaleness && !held.Owner.Since.IsZero() {
			held.Stale = true
		}
		if held.Stale && state.BreakStale(held) == nil {
			l, err = state.TryAcquire(p, "retain flush")
		}
	}
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}

func indexOf(recs []transcript.Record, uuid string) int {
	for i, r := range recs {
		if r.UUID == uuid {
			return i
		}
	}
	return -1
}

// errSummary describes a server error without its body: a refusal can echo
// the submitted transcript text.
// needsFix: errors only the user can clear (decision 0050's stop cases that
// are not transient).
func needsFix(err error) bool {
	var apiErr *hindsight.APIError
	return errors.As(err, &apiErr) && (apiErr.Kind == hindsight.Unauthorized || apiErr.Kind == hindsight.Redirected || apiErr.Kind == hindsight.NotFound)
}

func errSummary(err error) string {
	var apiErr *hindsight.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("%s %s: HTTP %d (%s)", apiErr.Method, apiErr.Path, apiErr.Status, apiErr.Kind)
	}
	return err.Error()
}
