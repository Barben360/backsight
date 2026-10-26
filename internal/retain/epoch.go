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
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/transcript"
)

// EpochDocument holds the bank's restore epoch, in its control bank:
// `restore` writes it, every client reads it before flushing (decision
// 0044).
const EpochDocument = "backsight:epoch"

// Epoch is the bank's restore epoch; 0 when it was never restored.
type Epoch struct {
	Epoch           int       `json:"epoch"`
	RestoredAt      time.Time `json:"restored_at"`
	BackupCreatedAt time.Time `json:"backup_created_at"`
	// History: the last epochs written, oldest first, this one included.
	History []EpochEntry `json:"history,omitempty"`
}

// EpochEntry is one restore of the epoch history.
type EpochEntry struct {
	Epoch           int       `json:"epoch"`
	BackupCreatedAt time.Time `json:"backup_created_at"`
}

// historyMax: how many restores the epoch document remembers. A client
// further behind than that compares every cursor sent since its own last
// reconcile (decision 0044, block 1 review).
const historyMax = 10

// EpochUnreadable is an epoch document the server holds but whose metadata
// does not parse: the server answered, the document is wrong.
type EpochUnreadable struct{ Reason string }

func (e *EpochUnreadable) Error() string { return EpochDocument + ": " + e.Reason }

// ReadEpoch reads the restore epoch of bank (the memory bank) from its
// control bank. No control bank or no document is epoch 0 (every bank
// before its first restore); any other failure, or metadata that does not
// parse (*EpochUnreadable), is an error: the caller must not act on a guess.
// A document written before the history existed has a history of itself.
func ReadEpoch(ctx context.Context, cl *hindsight.Client, bank string) (Epoch, error) {
	var ep Epoch
	doc, err := cl.GetDocument(ctx, ControlBank(bank), EpochDocument)
	var apiErr *hindsight.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
		return ep, nil
	case err != nil:
		return ep, errors.New(errSummary(err))
	}
	return EpochFromMetadata(doc.Metadata)
}

// EpochFromMetadata reads the epoch document's metadata (what ReadEpoch
// gets through the API, or restore reads from the database when the API is
// down). Metadata that does not parse is *EpochUnreadable.
func EpochFromMetadata(meta map[string]string) (Epoch, error) {
	var ep Epoch
	var err error
	if ep.Epoch, err = strconv.Atoi(meta["epoch"]); err != nil || ep.Epoch < 0 {
		return ep, &EpochUnreadable{fmt.Sprintf("epoch %q is not a number", meta["epoch"])}
	}
	for key, t := range map[string]*time.Time{"restored_at": &ep.RestoredAt, "backup_created_at": &ep.BackupCreatedAt} {
		if *t, err = time.Parse(time.RFC3339, meta[key]); err != nil {
			return ep, &EpochUnreadable{fmt.Sprintf("%s %q is not an RFC 3339 time", key, meta[key])}
		}
	}
	raw, ok := meta["history"]
	if !ok {
		ep.History = []EpochEntry{{Epoch: ep.Epoch, BackupCreatedAt: ep.BackupCreatedAt}}
		return ep, nil
	}
	if err := json.Unmarshal([]byte(raw), &ep.History); err != nil {
		return ep, &EpochUnreadable{"history is not a JSON list of epochs: " + err.Error()}
	}
	for _, h := range ep.History {
		if h.Epoch < 0 || h.BackupCreatedAt.IsZero() {
			return ep, &EpochUnreadable{fmt.Sprintf("history entry %+v is incomplete", h)}
		}
	}
	slices.SortFunc(ep.History, func(a, b EpochEntry) int { return a.Epoch - b.Epoch })
	return ep, nil
}

// since is when the cursors to compare start, for a client on epoch from
// that last reconciled at reconciledAt: the earliest backup of the epochs
// above from (minus the clock skew), or, for a client older than the
// history (all historyMax entries above it, so older ones were cut), its
// own last reconcile.
func (ep Epoch) since(from int, reconciledAt time.Time) time.Time {
	hist := ep.History
	if len(hist) == 0 {
		hist = []EpochEntry{{Epoch: ep.Epoch, BackupCreatedAt: ep.BackupCreatedAt}}
	}
	if len(hist) >= historyMax && hist[0].Epoch > from {
		return reconciledAt
	}
	earliest := ep.BackupCreatedAt
	for _, h := range hist {
		if h.Epoch > from && h.BackupCreatedAt.Before(earliest) {
			earliest = h.BackupCreatedAt
		}
	}
	return earliest.Add(-epochSkew)
}

// Reconciled is what Reconcile did.
type Reconciled struct {
	From int `json:"from"`
	To   int `json:"to"`
	// Resend: sessions the server holds less of than their cursor said;
	// the cursor is gone, so the next flush resumes them from the server.
	Resend []string `json:"resend,omitempty"`
}

// epochSkew widens "sent after the backup": the backup's time comes from
// the server's clock, the cursors' from this machine's. A cursor taken in
// by mistake costs one document read; one left out would stay lost.
const epochSkew = time.Hour

// Reconcile brings the cursors to the bank's restore epoch (decision 0044).
// On a higher epoch, each session sent after the backup was taken, of
// which the server now holds less, loses its cursor and the sent-index
// entries of what the server lacks; then the epoch is recorded in state.
// Anything that stops it (an unreadable epoch, maintenance, a session
// being flushed) drops nothing more and sets Hold: the next run retries.
func (f *Flusher) Reconcile(ctx context.Context) (Reconciled, error) {
	rec, err := f.reconcile(ctx)
	f.Hold = ""
	if err != nil {
		f.Hold = "restore epoch not reconciled: " + err.Error()
	}
	return rec, err
}

func (f *Flusher) reconcile(ctx context.Context) (Reconciled, error) {
	var rec Reconciled
	if err := f.restoreHold(); err != nil {
		return rec, err
	}
	if state.MaintenanceHeld(f.Dirs.MaintenanceLock()) {
		return rec, errors.New("maintenance in progress")
	}
	st, err := state.Load(f.Dirs.StateFile())
	if err != nil {
		return rec, err
	}
	rec.From = st.EpochOf(f.Bank)
	ep, err := ReadEpoch(ctx, f.Client, f.Bank)
	if err != nil {
		return rec, err
	}
	if rec.To = ep.Epoch; rec.To == rec.From {
		return rec, nil
	}
	l, _, err := state.Acquire(filepath.Join(f.Dirs.LocksDir(), "epoch.lock"), "restore epoch reconcile")
	if err != nil {
		return rec, err
	}
	defer func() { _ = l.Release() }()
	if st, err = state.Load(f.Dirs.StateFile()); err != nil {
		return rec, err
	}
	if rec.From = st.EpochOf(f.Bank); rec.To == rec.From {
		return rec, nil // another process reconciled meanwhile
	}
	if rec.To > rec.From { // a lower epoch is another server's history: nothing to undo
		// A restored queue lags the documents' metadata: a document read
		// now could say less than the server will hold, and resending
		// from it would send twice.
		n, err := f.Client.QueuedOperations(ctx, f.Bank)
		if err != nil {
			return rec, errors.New("cannot read the bank's queue: " + errSummary(err))
		}
		if n > 0 {
			return rec, fmt.Errorf("the bank has %d queued operation(s); retried once they are done", n)
		}
		reconciledAt := time.Time{}
		if st.RestoreEpoch != nil && st.RestoreEpoch.Bank == f.Bank {
			reconciledAt = st.RestoreEpoch.ReconciledAt
		}
		if rec.Resend, err = f.dropLost(ctx, ep.since(rec.From, reconciledAt), st.Backfill != nil); err != nil {
			return rec, err
		}
	}
	return rec, state.Update(f.Dirs, "restore epoch reconcile", func(s *state.State) error {
		s.RestoreEpoch = &state.RestoreEpoch{Bank: f.Bank, Epoch: rec.To, ReconciledAt: f.Now().UTC()}
		return nil
	})
}

// restoreHold is the restore-in-progress marker, or update apply's, as an
// error: while one exists nothing is sent, whatever the maintenance lock
// says (its owner may be a restore or an update killed half way).
func (f *Flusher) restoreHold() error {
	switch {
	case state.RestoreIncomplete(f.Dirs):
		return errors.New("a restore is in progress or did not finish (" + f.Dirs.RestoreMarker() + "): run backsight doctor")
	case state.UpdateIncomplete(f.Dirs):
		return errors.New("an update is in progress or did not finish (" + f.Dirs.UpdateMarker() + "): run backsight doctor")
	}
	return nil
}

// dropLost undoes, for each session of this bank sent after since, what
// the server no longer holds. queued: a backfill queue exists, which takes
// back the lost sessions that started before retain_since; without one,
// the user sent them by hand (`retain flush --session`) and the next live
// flush sends them again whole.
func (f *Flusher) dropLost(ctx context.Context, since time.Time, queued bool) (resend []string, err error) {
	files, err := filepath.Glob(filepath.Join(f.Dirs.CursorsDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	for _, p := range files {
		sid := strings.TrimSuffix(filepath.Base(p), ".json")
		cur, ok, err := LoadCursor(f.Dirs.CursorsDir(), sid)
		if err != nil || !ok || cur.Bank != f.Bank || !cur.SentAt.After(since) {
			continue // an unreadable cursor, or another bank's, resyncs anyway
		}
		lost, err := f.dropSession(ctx, sid, cur, queued)
		if err != nil {
			return nil, fmt.Errorf("session %s: %w", sid, err)
		}
		if lost {
			resend = append(resend, sid)
		}
	}
	return resend, nil
}

// dropSession compares what one session sent with the server's documents,
// under the session's lock. When a document no longer holds records this
// session sent to it, it records them as its own to resend (the .dropped
// mark), removes their "sent in this session" index
// entries, queues the session for backfill when it belongs there, and only
// then removes the cursor: a crash in between leaves the cursor, and the
// next run does it again. The bare uuids stay in the index: they are the
// fork guard, so a fork that copied these records still skips them, while
// this session's own resync sends them (it reads its .dropped mark).
func (f *Flusher) dropSession(ctx context.Context, sid string, cur Cursor, queued bool) (lost bool, err error) {
	unlock, err := f.lockSession(sid)
	if err != nil {
		return false, err
	}
	defer unlock()
	// The documents this cursor sent to: the home one and one per other
	// project. A cursor from before per-project documents (no Home, no
	// Segments) sent to conversation:<sid> alone, which DocumentID(sid, "",
	// "") names.
	projects := []string{cur.Home}
	for p := range cur.Segments {
		if p != cur.Home {
			projects = append(projects, p)
		}
	}
	slices.Sort(projects[1:])
	docs, err := f.readDocuments(ctx, sid, cur.Home, projects, nil)
	if err != nil {
		return false, errors.New(errSummary(err))
	}
	fh, err := os.Open(cur.Transcript)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // nothing left to resend from
	}
	if err != nil {
		return false, err
	}
	recs, _, err := transcript.Read(fh, 0)
	_ = fh.Close()
	if err != nil {
		return false, err
	}
	// A record is lost when this session sent it (its session key) and its
	// project's document no longer holds it: positions are per document, as
	// the resync reads them. A position that cannot be trusted leaves the
	// index alone: the resync quarantines.
	attrs := f.attribute(sid, recs, attr{})
	pos, bad := serverPosition(recs, docs, DocumentID(sid, cur.Home, cur.Home))
	mark := dropped{Records: map[string]bool{}}
	earlier := f.loadDropped(sid) // a run stopped half way already dropped these
	var keys []string
	lostAny := bad != ""
	for i, r := range recs {
		p := attrs[r.End].project
		if bad != "" || pos.holds(i, p) {
			continue
		}
		if _, sentTo := cur.Segments[p]; cur.Segments != nil && !sentTo {
			// Named now for a project this cursor never sent to: its
			// directory changed since (a linked worktree removed). The
			// position cannot tell; the resync does, from the server.
			continue
		}
		// Only this session's own sends: a fork's copied records stay
		// marked under the parent that sent them.
		if k := sessionKey(f.Bank, sid, r.UUID); f.Index.Has(k) {
			mark.Records[r.UUID] = true
			keys = append(keys, k)
			lostAny = true
		} else if earlier.Records[r.UUID] {
			lostAny = true
		}
	}
	if !lostAny {
		return false, nil // the server holds every record this session sent
	}
	early := !pos.found && len(recs) > 0 && !f.RetainSince.IsZero() && recs[0].Timestamp.Before(f.RetainSince)
	mark.PastSince = early && !queued // resent by live retain, past retain_since
	if err := f.addDropped(sid, mark); err != nil {
		return false, err
	}
	if err := f.Index.Remove(keys...); err != nil {
		return false, err
	}
	if early && queued {
		if err := state.Update(f.Dirs, "restore epoch reconcile", func(s *state.State) error {
			if b := s.Backfill; b != nil && !slices.Contains(b.Sessions, cur.Transcript) {
				b.Sessions = append(b.Sessions, cur.Transcript)
				b.Total++
			}
			return nil
		}); err != nil {
			return false, err
		}
	}
	p, _ := cursorPath(f.Dirs.CursorsDir(), sid)
	if err := os.Remove(p); err != nil {
		return false, err
	}
	return true, nil
}

// dropped is <sid>.dropped: what a restore lost of this session's own
// sends. Its resync sends these records although their bare uuid is in the
// index (the fork guard, kept for the forks that copied them).
type dropped struct {
	Records map[string]bool `json:"records"`
	// PastSince: the session started before retain_since but no backfill
	// queue takes it back: live retain resends it.
	PastSince bool `json:"past_since,omitempty"`
}

func droppedPath(cursorDir, sid string) string { return filepath.Join(cursorDir, sid+".dropped") }

func (f *Flusher) loadDropped(sid string) dropped {
	d := dropped{Records: map[string]bool{}}
	raw, err := os.ReadFile(droppedPath(f.Dirs.CursorsDir(), sid))
	if err == nil {
		_ = json.Unmarshal(raw, &d)
	}
	if d.Records == nil {
		d.Records = map[string]bool{}
	}
	return d
}

// addDropped merges m into the session's mark: a run stopped half way and
// done again keeps what the first one recorded.
func (f *Flusher) addDropped(sid string, m dropped) error {
	d := f.loadDropped(sid)
	for u := range m.Records {
		d.Records[u] = true
	}
	d.PastSince = d.PastSince || m.PastSince
	if len(d.Records) == 0 && !d.PastSince {
		return nil
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.Dirs.CursorsDir(), 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(droppedPath(f.Dirs.CursorsDir(), sid), raw, 0o600)
}

// pruneDropped forgets the records sent again in this session; the mark
// goes once none is left.
func (f *Flusher) pruneDropped(sid string) {
	p := droppedPath(f.Dirs.CursorsDir(), sid)
	if _, err := os.Stat(p); err != nil {
		return
	}
	d := f.loadDropped(sid)
	for u := range d.Records {
		if f.Index.Has(sessionKey(f.Bank, sid, u)) {
			delete(d.Records, u)
		}
	}
	if len(d.Records) == 0 {
		_ = os.Remove(p)
		return
	}
	d.PastSince = false // the document exists now
	if raw, err := json.Marshal(d); err == nil {
		_ = fsutil.WriteAtomic(p, raw, 0o600)
	}
}

// controlConfig keeps Hindsight's work on a control bank to the least
// 1.1.0 allows (checked 2026-10-25): chunks are stored as they are, with no
// LLM call, and no observation is consolidated; one unit per write remains.
var controlConfig = map[string]any{"retain_extraction_mode": "chunks", "enable_observations": false}

// WriteEpoch stores e as the restore epoch of bank (the memory bank) in its
// control bank, synchronously: readers see it as soon as it returns
// (restore writes it once the restored server passed its checks). The
// history keeps the last historyMax restores: the one on the server now
// (after a restore, the archive's), e.History (the server's before the
// restore, which the caller read) and e itself. The control bank is set up
// first, every time: a retain into a missing bank creates it with the
// default settings, an LLM extraction.
func WriteEpoch(ctx context.Context, cl *hindsight.Client, bank string, e Epoch) error {
	cur, err := ReadEpoch(ctx, cl, bank)
	var unreadable *EpochUnreadable
	switch {
	case errors.As(err, &unreadable):
		cur = Epoch{} // a broken document is replaced, its history with it
	case err != nil:
		return err
	}
	byEpoch := map[int]EpochEntry{}
	for _, h := range append(append(slices.Clone(cur.History), e.History...), EpochEntry{Epoch: e.Epoch, BackupCreatedAt: e.BackupCreatedAt}) {
		if h.Epoch > 0 && !h.BackupCreatedAt.IsZero() {
			byEpoch[h.Epoch] = EpochEntry{Epoch: h.Epoch, BackupCreatedAt: h.BackupCreatedAt.UTC().Truncate(time.Second)}
		}
	}
	hist := make([]EpochEntry, 0, len(byEpoch))
	for _, h := range byEpoch {
		hist = append(hist, h)
	}
	slices.SortFunc(hist, func(a, b EpochEntry) int { return a.Epoch - b.Epoch })
	if len(hist) > historyMax {
		hist = hist[len(hist)-historyMax:]
	}
	rawHist, err := json.Marshal(hist)
	if err != nil {
		return err
	}
	cb := ControlBank(bank)
	if _, err := QuietBank(ctx, cl, cb, ControlMarker, "backsight's control records for bank "+bank+" (the restore epoch); no memory."); err != nil {
		return err
	}
	at, from := e.RestoredAt.UTC().Format(time.RFC3339), e.BackupCreatedAt.UTC().Format(time.RFC3339)
	return cl.RetainNow(ctx, cb, []hindsight.RetainItem{{
		Content:    fmt.Sprintf("restore epoch %d (restored at %s from a backup of %s)", e.Epoch, at, from),
		DocumentID: EpochDocument,
		Tags:       []string{},
		Metadata:   map[string]string{"epoch": strconv.Itoa(e.Epoch), "restored_at": at, "backup_created_at": from, "history": string(rawHist)},
	}})
}

// QuietBank creates bank, or refreshes its profile, with controlConfig:
// what it stores costs no LLM call. The control bank and the docs bank
// (decision 0069) are both set up through it, before anything is retained.
// A bank without m is left as it is (OwnedBank: *ForeignBank); the bank's
// documents before the setup are returned.
func QuietBank(ctx context.Context, cl *hindsight.Client, bank string, m Marker, mission string) ([]hindsight.DocumentItem, error) {
	docs, _, err := OwnedBank(ctx, cl, bank, m)
	if err != nil {
		return nil, err
	}
	if err := cl.CreateBank(ctx, bank, m.Name, mission); err != nil {
		return nil, err
	}
	return docs, cl.PatchBankConfig(ctx, bank, controlConfig)
}

// ControlBank is the bank holding bank's control records (the restore
// epoch), apart from the memories: whatever Hindsight makes of them never
// reaches recall, doctor's counts or bank apply, which read bank alone.
func ControlBank(bank string) string { return bank + "-control" }
