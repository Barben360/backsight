package retain

import (
	"context"
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
}

// ReadEpoch reads the restore epoch of bank (the memory bank) from its
// control bank. No control bank or no document is epoch 0 (every bank
// before its first restore); any other failure, or metadata that does not
// parse, is an error: the caller must not act on a guess.
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
	if ep.Epoch, err = strconv.Atoi(doc.Metadata["epoch"]); err != nil || ep.Epoch < 0 {
		return ep, fmt.Errorf("%s: epoch %q is not a number", EpochDocument, doc.Metadata["epoch"])
	}
	for key, t := range map[string]*time.Time{"restored_at": &ep.RestoredAt, "backup_created_at": &ep.BackupCreatedAt} {
		if *t, err = time.Parse(time.RFC3339, doc.Metadata[key]); err != nil {
			return ep, fmt.Errorf("%s: %s %q is not an RFC 3339 time", EpochDocument, key, doc.Metadata[key])
		}
	}
	return ep, nil
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
	var backfill []string
	if rec.To > rec.From { // a lower epoch is another server's history: nothing to undo
		if rec.Resend, backfill, err = f.dropLost(ctx, ep.BackupCreatedAt.Add(-epochSkew), st.Backfill != nil); err != nil {
			return rec, err
		}
	}
	return rec, state.Update(f.Dirs, "restore epoch reconcile", func(s *state.State) error {
		s.RestoreEpoch = &state.RestoreEpoch{Bank: f.Bank, Epoch: rec.To, ReconciledAt: f.Now().UTC()}
		if b := s.Backfill; b != nil {
			for _, p := range backfill {
				if !slices.Contains(b.Sessions, p) {
					b.Sessions = append(b.Sessions, p)
					b.Total++
				}
			}
		}
		return nil
	})
}

// dropLost undoes, for each session of this bank sent after since, what
// the server no longer holds. backfill lists the transcripts of lost
// sessions that started before retain_since: live retain leaves those to
// backfill when queued (a backfill queue exists); without one, the user
// sent them by hand (`retain flush --session`) and the next live flush
// sends them again whole.
func (f *Flusher) dropLost(ctx context.Context, since time.Time, queued bool) (resend, backfill []string, err error) {
	files, err := filepath.Glob(filepath.Join(f.Dirs.CursorsDir(), "*.json"))
	if err != nil {
		return nil, nil, err
	}
	for _, p := range files {
		sid := strings.TrimSuffix(filepath.Base(p), ".json")
		cur, ok, err := LoadCursor(f.Dirs.CursorsDir(), sid)
		if err != nil || !ok || cur.Bank != f.Bank || !cur.SentAt.After(since) {
			continue // an unreadable cursor, or another bank's, resyncs anyway
		}
		lost, early, err := f.dropSession(ctx, sid, cur, queued)
		if err != nil {
			return nil, nil, fmt.Errorf("session %s: %w", sid, err)
		}
		if lost {
			resend = append(resend, sid)
		}
		if early {
			backfill = append(backfill, cur.Transcript)
		}
	}
	return resend, backfill, nil
}

// dropSession compares one session's cursor with the server's document,
// under the session's lock. When the server holds less, it removes the
// sent-index entries of the records this session sent after the server's
// last_uuid (all of them when the document is gone), then the cursor.
// early: the document is gone and the session started before retain_since;
// unless queued for backfill, it is marked to be resent whole, past
// retain_since, before its cursor goes.
func (f *Flusher) dropSession(ctx context.Context, sid string, cur Cursor, queued bool) (lost, early bool, err error) {
	unlock, err := f.lockSession(sid)
	if err != nil {
		return false, false, err
	}
	defer unlock()
	last := ""
	doc, err := f.Client.GetDocument(ctx, f.Bank, cur.Document)
	var apiErr *hindsight.APIError
	switch {
	case err == nil:
		if last = doc.Metadata["last_uuid"]; last == cur.LastUUID {
			return false, false, nil
		}
	case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
	default:
		return false, false, errors.New(errSummary(err))
	}
	fh, err := os.Open(cur.Transcript)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil // nothing left to resend from
	}
	if err != nil {
		return false, false, err
	}
	recs, _, err := transcript.Read(fh, 0)
	_ = fh.Close()
	if err != nil {
		return false, false, err
	}
	// Records up to the server's last_uuid stay sent. A last_uuid the
	// transcript lacks leaves the index alone: the resync quarantines.
	from := 0
	if last != "" {
		if from = indexOf(recs, last) + 1; from == 0 {
			from = len(recs)
		}
	}
	var keys []string
	for _, r := range recs[from:] {
		// Only this session's own sends: a fork's copied records stay
		// marked under the parent that sent them.
		if k := sessionKey(f.Bank, sid, r.UUID); f.Index.Has(k) {
			keys = append(keys, r.UUID, k)
		}
	}
	if err := f.Index.Remove(keys...); err != nil {
		return false, false, err
	}
	early = last == "" && len(recs) > 0 && !f.RetainSince.IsZero() && recs[0].Timestamp.Before(f.RetainSince)
	if early && !queued {
		if err := fsutil.WriteAtomic(pendingPath(f.Dirs.CursorsDir(), sid), []byte(resendMark), 0o600); err != nil {
			return false, false, err
		}
	}
	p, _ := cursorPath(f.Dirs.CursorsDir(), sid)
	if err := os.Remove(p); err != nil {
		return false, false, err
	}
	return true, early && queued, nil
}

// controlConfig keeps Hindsight's work on a control bank to the least
// 1.1.0 allows (checked 2026-10-25): chunks are stored as they are, with no
// LLM call, and no observation is consolidated; one unit per write remains.
var controlConfig = map[string]any{"retain_extraction_mode": "chunks", "enable_observations": false}

// WriteEpoch stores e as the restore epoch of bank (the memory bank) in its
// control bank, synchronously: readers see it as soon as it returns
// (restore writes it once the restored server passed its checks). The
// control bank is set up first, every time: a retain into a missing bank
// creates it with the default settings, an LLM extraction.
func WriteEpoch(ctx context.Context, cl *hindsight.Client, bank string, e Epoch) error {
	cb := ControlBank(bank)
	if err := cl.CreateBank(ctx, cb, "backsight control", "backsight's control records for bank "+bank+" (the restore epoch); no memory."); err != nil {
		return err
	}
	if err := cl.PatchBankConfig(ctx, cb, controlConfig); err != nil {
		return err
	}
	at, from := e.RestoredAt.UTC().Format(time.RFC3339), e.BackupCreatedAt.UTC().Format(time.RFC3339)
	return cl.RetainNow(ctx, cb, []hindsight.RetainItem{{
		Content:    fmt.Sprintf("restore epoch %d (restored at %s from a backup of %s)", e.Epoch, at, from),
		DocumentID: EpochDocument,
		Tags:       []string{},
		Metadata:   map[string]string{"epoch": strconv.Itoa(e.Epoch), "restored_at": at, "backup_created_at": from},
	}})
}

// ControlBank is the bank holding bank's control records (the restore
// epoch), apart from the memories: whatever Hindsight makes of them never
// reaches recall, doctor's counts or bank apply, which read bank alone.
func ControlBank(bank string) string { return bank + "-control" }
