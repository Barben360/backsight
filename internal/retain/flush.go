package retain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
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
	Client      *hindsight.Client
	Bank        string
	Dirs        paths.Dirs
	Resolver    *project.Resolver
	Exclude     []string
	MachineID   string
	Version     string
	Now         func() time.Time
	RetainSince time.Time // zero: no boundary
	Index       *SentIndex
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

	if _, err := os.Stat(f.Dirs.MaintenanceLock()); err == nil {
		return fail(Maintenance, "maintenance in progress")
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
	resync := !haveCursor || cur.Bank != f.Bank || cur.Backfill || fi.Size() < cur.Offset
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
	now := f.Now()
	for i := range recs {
		recs[i].Timestamp = clampTime(recs[i].Timestamp, fi.ModTime(), now)
		for j := range recs[i].Turns {
			recs[i].Turns[j].Timestamp = recs[i].Timestamp
		}
	}

	if resync {
		cur = Cursor{Transcript: path, Document: "conversation:" + sid, Bank: f.Bank}
		if len(recs) == 0 {
			return fail(Nothing, "")
		}
		doc, err := f.Client.GetDocument(ctx, f.Bank, cur.Document)
		var apiErr *hindsight.APIError
		switch {
		case err == nil:
			last := doc.Metadata["last_uuid"]
			i := indexOf(recs, last)
			if i < 0 {
				return f.quarantine(res, &cur, "the server holds this session up to record "+last+", which this transcript does not contain")
			}
			seg, err := strconv.Atoi(doc.Metadata["segment"])
			if err != nil || seg < 1 {
				return f.quarantine(res, &cur, "the server's document has no valid segment number; sending would replace it")
			}
			cur.Segment, cur.LastUUID, cur.Offset = seg, last, recs[i].End
			recs = recs[i+1:]
			// Records this machine already sent may be missing from the
			// server's metadata (async retain still running, or a crash
			// before the cursor was saved): never send them twice.
			skip := 0
			for skip < len(recs) && f.Index.Has(recs[skip].UUID) {
				skip++
			}
			if skip > 0 {
				cur.Offset, cur.LastUUID = recs[skip-1].End, recs[skip-1].UUID
				recs = recs[skip:]
			}
		case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
			skip := 0
			for skip < len(recs) && f.Index.Has(recs[skip].UUID) {
				if f.Index.Has(sessionKey(sid, recs[skip].UUID)) {
					// This session's own records went out, but the document
					// is not readable yet (its async retain is still running):
					// a new segment 1 would replace it (issue 101).
					return fail(Retry, "the server has not stored this session's earlier segment yet; retried later")
				}
				skip++ // a fork copies its parent's records (decision 0038)
			}
			if skip > 0 {
				cur.Offset, cur.LastUUID = recs[skip-1].End, recs[skip-1].UUID
				recs = recs[skip:]
			}
			if len(recs) > 0 && !opt.IgnoreSince && !f.RetainSince.IsZero() && recs[0].Timestamp.Before(f.RetainSince) {
				cur.Backfill = true
				return f.save(res, cur, Backfill, "started before live retain began; backfill sends it")
			}
		default:
			res.Stop, res.NeedsFix = true, needsFix(err)
			return fail(Retry, errSummary(err))
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
	return f.sendRuns(ctx, res, cur, sid, send, now)
}

// runKey groups consecutive records that go out together: same project and
// same opt-out answer for their working directory.
type runKey struct {
	optOut  bool
	reason  string
	project string
}

// sendRuns sends records in runs of one project, skipping opted-out runs
// (a session can cd into an excluded path mid-way). It stops at the first
// failure, with the cursor after the last run done.
func (f *Flusher) sendRuns(ctx context.Context, res Result, cur Cursor, sid string, send []transcript.Record, now time.Time) Result {
	paused, why := f.paused(sid)
	keys := map[string]runKey{}
	key := func(cwd string) runKey {
		if paused {
			return runKey{optOut: true, reason: why}
		}
		if k, ok := keys[cwd]; ok {
			return k
		}
		k := runKey{}
		if out, reason := f.Resolver.OptedOut(cwd, f.Exclude); out {
			k.optOut, k.reason = true, reason
		} else {
			k.project = f.Resolver.Project(cwd)
		}
		keys[cwd] = k
		return k
	}
	status, detail := Nothing, ""
	for len(send) > 0 {
		k := key(send[0].CWD)
		n := 1
		for n < len(send) && key(send[n].CWD) == k {
			n++
		}
		run := send[:n]
		send = send[n:]
		last := run[len(run)-1]
		if k.optOut {
			cur.Offset, cur.LastUUID, cur.LastTurnAt = last.End, last.UUID, last.Timestamp
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

// sendPart sends one segment; done reports that the flush ended (error).
func (f *Flusher) sendPart(ctx context.Context, res Result, cur Cursor, sid string, run []transcript.Record, project, status, detail string, now time.Time) (Result, Cursor, string, string, bool) {
	last := run[len(run)-1]
	segNo := cur.Segment + 1
	seg := BuildSegment(f.Bank, sid, run, segNo, project, f.MachineID, f.Version)
	if err := f.Client.Retain(ctx, f.Bank, []hindsight.RetainItem{seg.Item}, seg.OperationID); err != nil {
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
		uuids = append(uuids, r.UUID, sessionKey(sid, r.UUID)) // fork guard, then "sent in this session"
	}
	if err := f.Index.Add(uuids...); err != nil {
		detail = "sent, but the fork-guard index was not updated: " + err.Error()
	}
	cur.Offset, cur.LastUUID, cur.Segment, cur.LastTurnAt, cur.SentAt = last.End, last.UUID, segNo, last.Timestamp, now
	cur.Records += len(run)
	res.Records += len(run)
	res.Segment, res.Chars, res.Project = segNo, res.Chars+seg.Chars, project
	return res, cur, Sent, detail, false
}

func (f *Flusher) save(res Result, cur Cursor, status, detail string) Result {
	res.Status, res.Detail = status, detail
	if fp, err := Fingerprint(cur.Transcript, cur.Offset); err == nil {
		cur.Fingerprint = fp
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

// paused reads the session's pause flag; an unreadable file counts as
// paused (fail closed).
func (f *Flusher) paused(sid string) (bool, string) {
	var s struct {
		Paused bool `json:"paused"`
	}
	raw, err := os.ReadFile(filepath.Join(f.Dirs.SessionsDir(), sid+".json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, ""
	case err != nil || json.Unmarshal(raw, &s) != nil:
		return true, "session state unreadable: treated as paused"
	}
	return s.Paused, "session paused"
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
