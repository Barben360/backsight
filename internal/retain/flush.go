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
}

// FlushFile sends what is settled and unsent in one transcript.
func (f *Flusher) FlushFile(ctx context.Context, path string, opt Options) Result {
	sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	res := Result{Session: sid, Transcript: path}
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
	// A cursor without Home predates per-project documents: its Segment
	// counts every project's runs, all sent to the home document.
	resync := !haveCursor || cur.Bank != f.Bank || cur.Backfill || cur.Home == "" || fi.Size() < cur.Offset
	if haveCursor {
		fp, err := Fingerprint(path, cur.Offset)
		if err != nil {
			return fail(Retry, err.Error())
		}
		resync = resync || cur.Fingerprint != fp
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

	if resync {
		cur = Cursor{Transcript: path, Bank: f.Bank}
		if len(recs) == 0 {
			return fail(Nothing, "")
		}
	}
	if cur.Segments == nil {
		cur.Segments = map[string]int{}
	}
	projs := f.attribute(recs, cur.Project)
	if resync {
		cur.Home = homeOf(recs, projs)
		cur.Document = DocumentID(sid, cur.Home, cur.Home)
		// sent: already out, from this machine. The fork guard (a bare uuid
		// in the index) does not count for the records a restore lost of
		// this session's own sends (its .dropped mark), unless sent again.
		lostOwn := f.loadDropped(sid)
		sent := func(u string) bool {
			return f.Index.Has(u) && (!lostOwn.Records[u] || f.Index.Has(sessionKey(f.Bank, sid, u)))
		}
		docs, err := f.readDocuments(ctx, sid, cur.Home, projectsOf(cur.Home, recs, projs))
		if err != nil {
			res.Stop, res.NeedsFix = true, needsFix(err)
			return fail(Retry, errSummary(err))
		}
		// The session resumes after the furthest record any of its
		// documents holds: records go out in file order, so the server holds
		// every earlier one (in its own document) or the restore lost it too.
		at, unknown, ok := furthest(recs, docs)
		if !ok {
			return f.quarantine(res, &cur, "the server holds this session up to record "+unknown+", which this transcript does not contain")
		}
		found := map[string]bool{}
		for _, d := range docs {
			if !d.found {
				continue
			}
			seg, err := strconv.Atoi(d.segment)
			if err != nil || seg < 1 {
				return f.quarantine(res, &cur, "the server's document "+d.id+" has no valid segment number; sending would replace it")
			}
			cur.Segments[d.project], found[d.project] = seg, true
		}
		if at >= 0 {
			cur.Offset, cur.LastUUID, cur.Project = recs[at].End, recs[at].UUID, projs[recs[at].End]
			recs = recs[at+1:]
		}
		// Released from quarantine after a first segment the server never
		// stored: that document is resent whole, from segment 1.
		resend := f.pendingMark(sid) == resendMark
		// Records this machine already sent may be missing from the
		// server's metadata (async retain still running, or a crash before
		// the cursor was saved): never send them twice.
		skip := 0
		for skip < len(recs) && sent(recs[skip].UUID) {
			r := recs[skip]
			if !found[projs[r.End]] {
				if resend {
					break
				}
				if f.Index.Has(sessionKey(f.Bank, sid, r.UUID)) {
					// This session's own record went out, but its document is
					// not readable yet (its first async retain is still
					// running): a new segment 1 would replace it (issue 101).
					// After PendingLimit the retain is taken as lost.
					if f.waitedTooLong(sid, now) {
						return f.quarantine(res, &cur, "the server never stored the first segment of "+DocumentID(sid, cur.Home, projs[r.End])+"; retain retry-quarantine resends the session")
					}
					return fail(Retry, "the server has not stored this session's earlier segment yet; retried later")
				}
				// else a fork copies its parent's records (decision 0038)
			}
			skip++
		}
		if skip > 0 {
			last := recs[skip-1]
			cur.Offset, cur.LastUUID, cur.Project = last.End, last.UUID, projs[last.End]
			recs = recs[skip:]
		}
		if at < 0 && !resend && len(recs) > 0 && !opt.IgnoreSince && !lostOwn.PastSince && !f.RetainSince.IsZero() && recs[0].Timestamp.Before(f.RetainSince) {
			cur.Backfill = true
			return f.save(res, cur, Backfill, "started before live retain began; backfill sends it")
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
	return f.sendRuns(ctx, res, cur, sid, send, projs, now)
}

// attribute names the project of each record of recs, keyed by its End
// offset (unique in a file); prev is the project of the record before
// recs[0], "" at the file's start. This is the one place the project rule
// lives (decision 0071, C2a): a record whose directory has a strong name
// (Resolver.Named: transverse path, .backsight.toml project, git
// repository) takes it; any other keeps the previous record's project, so
// a cd into ~/.npm-global/... or /tmp does not start a project. Only a
// session's first directory (prev == "") may name one by its basename. A
// record with no directory keeps prev too: it is never sent (OptedOut).
// The rule reads the transcript alone, never pauses or opt-outs, so a
// resync from offset 0 names every record as the incremental flushes did.
func (f *Flusher) attribute(recs []transcript.Record, prev string) map[int64]string {
	type name struct {
		name   string
		strong bool
	}
	names := map[string]name{}
	out := make(map[int64]string, len(recs))
	for _, r := range recs {
		if strings.TrimSpace(r.CWD) != "" {
			n, ok := names[r.CWD]
			if !ok {
				n.name, n.strong = f.Resolver.Named(r.CWD)
				names[r.CWD] = n
			}
			if n.strong || prev == "" {
				prev = n.name
			}
		}
		out[r.End] = prev
	}
	return out
}

// homeOf is the session's home project: its first named record's.
func homeOf(recs []transcript.Record, projs map[int64]string) string {
	for _, r := range recs {
		if p := projs[r.End]; p != "" {
			return p
		}
	}
	return ""
}

// projectsOf lists home, then every other project of recs once, in order:
// the documents a resync reads.
func projectsOf(home string, recs []transcript.Record, projs map[int64]string) []string {
	out := []string{home}
	seen := map[string]bool{home: true}
	for _, r := range recs {
		if p := projs[r.End]; p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// serverDoc is what the server holds of one of a session's documents.
type serverDoc struct {
	project, id string
	found       bool
	lastUUID    string
	segment     string
}

// readDocuments reads the session's document of each project; a missing
// one is not an error, any other failure is.
func (f *Flusher) readDocuments(ctx context.Context, sid, home string, projects []string) ([]serverDoc, error) {
	out := make([]serverDoc, 0, len(projects))
	for _, p := range projects {
		d := serverDoc{project: p, id: DocumentID(sid, home, p)}
		doc, err := f.Client.GetDocument(ctx, f.Bank, d.id)
		var apiErr *hindsight.APIError
		switch {
		case err == nil:
			d.found, d.lastUUID, d.segment = true, doc.Metadata["last_uuid"], doc.Metadata["segment"]
		case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
		default:
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// furthest is the index in recs of the furthest last_uuid the documents
// hold, -1 when none holds any. ok is false, with that last_uuid, when a
// document holds a record recs lack: the position is unknown.
func furthest(recs []transcript.Record, docs []serverDoc) (at int, unknown string, ok bool) {
	at = -1
	for _, d := range docs {
		if !d.found {
			continue
		}
		i := indexOf(recs, d.lastUUID)
		if i < 0 {
			return -1, d.lastUUID, false
		}
		at = max(at, i)
	}
	return at, "", true
}

// runKey groups consecutive records that go out together: same project
// (so same document) and same opt-out answer for their working directory.
type runKey struct {
	optOut  bool
	reason  string
	project string
}

// sendRuns sends records in runs of one project, each to its project's
// document, skipping opted-out runs (a session can cd into an excluded path
// mid-way; opt-outs are still judged per record directory). projs names
// each record's project (attribute). It stops at the first failure, with
// the cursor after the last run done.
func (f *Flusher) sendRuns(ctx context.Context, res Result, cur Cursor, sid string, send []transcript.Record, projs map[int64]string, now time.Time) Result {
	pauses := f.pauses(sid)
	optOuts := map[string]runKey{}
	key := func(r transcript.Record) runKey {
		if out, why := pauses(r); out {
			return runKey{optOut: true, reason: why}
		}
		if f.headless(r.Entrypoint) {
			return runKey{optOut: true, reason: "headless session (entrypoint " + r.Entrypoint + "): privacy.skip_entrypoints"}
		}
		k, ok := optOuts[r.CWD]
		if !ok {
			if out, reason := f.Resolver.OptedOut(r.CWD, f.Exclude); out {
				k.optOut, k.reason = true, reason
			}
			optOuts[r.CWD] = k
		}
		if !k.optOut {
			k.project = projs[r.End]
		}
		return k
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
		if k.optOut {
			// Handled for good: a fork's copy of these records skips them
			// with the ones already sent (the fork guard's leading run),
			// instead of sending them or stopping its run there.
			handled := make([]string, 0, len(run))
			for _, r := range run {
				handled = append(handled, r.UUID)
			}
			_ = f.Index.Add(handled...)
			cur.Offset, cur.LastUUID, cur.LastTurnAt, cur.Project = last.End, last.UUID, last.Timestamp, projs[last.End]
			if status != Sent {
				status, detail = OptedOut, k.reason
			}
			continue
		}
		for _, part := range chunk(run) {
			var done bool
			if res, cur, status, detail, done = f.sendPart(ctx, res, cur, sid, part, k.project, status, detail, now); done {
				return res
			}
		}
	}
	return f.save(res, cur, status, detail)
}

func (f *Flusher) headless(entrypoint string) bool {
	return f.Headless != nil && f.Headless(entrypoint)
}

// sendPart sends one segment to project's document; done reports that the
// flush ended (error).
func (f *Flusher) sendPart(ctx context.Context, res Result, cur Cursor, sid string, run []transcript.Record, project, status, detail string, now time.Time) (Result, Cursor, string, string, bool) {
	last := run[len(run)-1]
	doc := DocumentID(sid, cur.Home, project)
	segNo := cur.Segments[project] + 1
	seg := BuildSegment(f.Bank, sid, doc, run, segNo, project, f.MachineID, f.Version)
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
	cur.Offset, cur.LastUUID, cur.LastTurnAt, cur.SentAt, cur.Project = last.End, last.UUID, last.Timestamp, now, project
	cur.Segments[project] = segNo
	cur.Records += len(run)
	res.Records += len(run)
	if !slices.Contains(res.Documents, doc) {
		res.Documents = append(res.Documents, doc)
	}
	res.Segment, res.Chars, res.Project = segNo, res.Chars+seg.Chars, project
	return res, cur, Sent, detail, false
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
