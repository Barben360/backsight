package retain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/transcript"
)

// TombstonesDocument holds the bank's purge tombstones in its control bank,
// beside EpochDocument: every client and a reinstall learn there what
// `forget --purge` deleted, so none sends it again (M5 review rules).
const TombstonesDocument = "backsight:tombstones"

// TombstonesUnreadable is a tombstones document the server holds whose
// metadata does not parse.
type TombstonesUnreadable struct{ Reason string }

func (e *TombstonesUnreadable) Error() string { return TombstonesDocument + ": " + e.Reason }

// TombstoneOf is the tombstone of document doc of bank, from its metadata
// read just before the purge deletes it. A conversation document names its
// session and project in its id and metadata; one without a project in its
// metadata was written before per-project documents and held every
// project's records up to its last_uuid (Legacy).
func TombstoneOf(bank, doc string, meta map[string]string, at time.Time) state.Tombstone {
	t := state.Tombstone{Bank: bank, Document: doc, Session: meta["session_id"], Project: meta["project"], LastUUID: meta["last_uuid"],
		PurgedAt: at.UTC().Truncate(time.Second)}
	switch {
	case strings.HasPrefix(doc, "conversation:"):
		sid, project, scoped := strings.Cut(strings.TrimPrefix(BaseDocumentID(doc), "conversation:"), "@")
		if t.Session == "" {
			t.Session = sid
		}
		_, layout := meta["project"]
		switch {
		case layout:
			t.Legacy = meta["legacy_last_uuid"]
		case scoped:
			t.Project = project
		default:
			t.Legacy = t.LastUUID
		}
	case strings.HasPrefix(doc, "subagent:") && t.Session == "":
		t.Session, _, _ = strings.Cut(strings.TrimPrefix(doc, "subagent:"), ":")
	}
	return t
}

// ContentHash is a note's text as a tombstone keeps it (ContentSHA256).
func ContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ReadTombstones reads the purge tombstones of bank (the memory bank) from
// its control bank: none when the bank or the document does not exist;
// *TombstonesUnreadable when its metadata does not parse; any other
// failure is an error.
func ReadTombstones(ctx context.Context, cl *hindsight.Client, bank string) ([]state.Tombstone, error) {
	doc, err := cl.GetDocument(ctx, ControlBank(bank), TombstonesDocument)
	var apiErr *hindsight.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
		return nil, nil
	case err != nil:
		return nil, errors.New(errSummary(err))
	}
	return TombstonesFromMetadata(bank, doc.Metadata)
}

// TombstonesFromMetadata reads the tombstones document's metadata (through
// the API, or from the database while the API is down); each tombstone
// gets bank.
func TombstonesFromMetadata(bank string, meta map[string]string) ([]state.Tombstone, error) {
	var ts []state.Tombstone
	if err := json.Unmarshal([]byte(meta["tombstones"]), &ts); err != nil {
		return nil, &TombstonesUnreadable{"tombstones is not a JSON list: " + err.Error()}
	}
	for i := range ts {
		if ts[i].Document == "" {
			return nil, &TombstonesUnreadable{fmt.Sprintf("tombstone %d names no document", i)}
		}
		ts[i].Bank = bank
	}
	return ts, nil
}

// WriteTombstones adds ts to the tombstones of bank in its control bank,
// synchronously, and returns how many the document holds now. The control
// bank is set up first, with its ownership check (QuietBank): a bank that
// is not backsight's is left alone (*ForeignBank). A broken document is
// replaced.
func WriteTombstones(ctx context.Context, cl *hindsight.Client, bank string, ts []state.Tombstone) (int, error) {
	return WriteTombstonesAs(ctx, cl, bank, ts, false)
}

// WriteTombstonesAs is WriteTombstones; adopt: a control bank it creates
// is adopted (AdoptedControlMarker).
func WriteTombstonesAs(ctx context.Context, cl *hindsight.Client, bank string, ts []state.Tombstone, adopt bool) (int, error) {
	cur, err := ReadTombstones(ctx, cl, bank)
	var unreadable *TombstonesUnreadable
	switch {
	case errors.As(err, &unreadable):
		cur = nil
	case err != nil:
		return 0, err
	}
	all := state.State{Tombstones: cur}
	all.AddTombstones(ts...)
	slices.SortStableFunc(all.Tombstones, func(a, b state.Tombstone) int { return a.PurgedAt.Compare(b.PurgedAt) })
	raw, err := json.Marshal(all.Tombstones)
	if err != nil {
		return 0, err
	}
	content := fmt.Sprintf("purge tombstones of bank %s: %d document(s) deleted by forget --purge", bank, len(all.Tombstones))
	if err := writeControlDocument(ctx, cl, bank, TombstonesDocument, content, map[string]string{"tombstones": string(raw)}, adopt); err != nil {
		return 0, err
	}
	return len(all.Tombstones), nil
}

// syncTombstones makes this client's tombstones and the control bank's
// the same: the control bank's are added to state.json (another client's
// purge, or the only copy after a reinstall), and the ones it lacks (a
// restore brought back an older control bank, or a purge could not write
// there) are written back. A document that cannot be read stops the run:
// sending without it could resend what another client purged. Writing back
// is retried by the next run when it fails: this client holds them anyway.
func (f *Flusher) syncTombstones(ctx context.Context) error {
	remote, err := ReadTombstones(ctx, f.Client, f.Bank)
	var unreadable *TombstonesUnreadable
	if err != nil && !errors.As(err, &unreadable) {
		return fmt.Errorf("cannot read the purge tombstones: %w", err)
	}
	st, err := state.Load(f.Dirs.StateFile())
	if err != nil {
		return err
	}
	local := st.TombstonesOf(f.Bank)
	if probe := (state.State{Tombstones: local}); probe.AddTombstones(remote...) > 0 {
		if err := state.Update(f.Dirs, "purge tombstones", func(s *state.State) error {
			s.AddTombstones(remote...)
			local = s.TombstonesOf(f.Bank)
			return nil
		}); err != nil {
			return err
		}
	}
	have := state.State{Tombstones: remote}
	if unreadable == nil && have.AddTombstones(local...) == 0 {
		return nil
	}
	_, _ = WriteTombstones(ctx, f.Client, f.Bank, local)
	return nil
}

// purge is what tombstones say a purge removed of one session, by index
// in its whole transcript.
type purge struct {
	at  map[string]int // project → the last record of it deleted
	all int            // every project's records up to there (a legacy document); -1: none
}

func (p purge) holds(i int, project string) bool {
	at, ok := p.at[project]
	return i <= p.all || (ok && i <= at)
}

// purgedOf reads the tombstones of session sid's conversation documents
// against its whole transcript recs. A tombstone whose last record recs do
// not hold (a transcript rewritten since) covers every record of its
// project: what it deleted can no longer be told apart, and the purge was
// meant to keep it off the server.
func purgedOf(sid string, recs []transcript.Record, tombs []state.Tombstone) purge {
	p := purge{at: map[string]int{}, all: -1}
	upTo := func(uuid string) int {
		if i := indexOf(recs, uuid); i >= 0 {
			return i
		}
		return len(recs) - 1
	}
	home := "conversation:" + sid
	for _, t := range tombs {
		project, scoped := "", false
		// A successor id (LiveDocumentID) is its base's: the same
		// session's records of the same project.
		switch doc := BaseDocumentID(t.Document); {
		case doc == home:
		case strings.HasPrefix(doc, home+"@"):
			project, scoped = strings.TrimPrefix(doc, home+"@"), true
		default:
			continue
		}
		if t.Legacy != "" {
			p.all = max(p.all, upTo(t.Legacy))
			if t.Legacy == t.LastUUID {
				continue
			}
		}
		if !scoped {
			project = t.Project // the home document: its own project
		}
		if at, ok := p.at[project]; !ok || upTo(t.LastUUID) > at {
			p.at[project] = upTo(t.LastUUID)
		}
	}
	return p
}

// tombstones are this client's tombstones of the bank.
func (f *Flusher) tombstones() ([]state.Tombstone, error) {
	st, err := state.Load(f.Dirs.StateFile())
	if err != nil {
		return nil, err
	}
	return st.TombstonesOf(f.Bank), nil
}

// lostToPurged are the projects of cursor cur (session sid) whose
// document is a tombstoned id, each with when that id was purged (the
// latest purge of it). Their last sends went to a document a purge again
// deletes: either a binary unaware of tombstones wrote them after the
// purge (a rollback, or this machine before it learned another's purge),
// or this binary sent them before the purge and their async retain was
// still queued. Only the records written after the purge are the first
// kind (lostAfter). A cursor from before per-project documents sent every
// project to conversation:<sid> (key "").
func lostToPurged(sid string, cur Cursor, tombs []state.Tombstone) map[string]time.Time {
	purged := map[string]time.Time{}
	for _, t := range tombs {
		if at, ok := purged[t.Document]; !ok || t.PurgedAt.After(at) {
			purged[t.Document] = t.PurgedAt
		}
	}
	out := map[string]time.Time{}
	if cur.Segments == nil {
		if at, ok := purged[DocumentID(sid, "", "")]; ok && cur.Bank != "" {
			out[""] = at
		}
		return out
	}
	for p := range cur.Segments {
		if at, ok := purged[cur.docOf(sid, p)]; ok {
			out[p] = at
		}
	}
	return out
}

// lostAfter says whether record r of project p was lost to a purge again
// of what a binary unaware of tombstones wrote after the purge: its
// project's last sends went to a purged id (lost), and it was written
// after that purge. A record written before it, or with no time, stays
// held: the user purged the session as it stood, and a retain still queued
// at that time may have carried it.
func lostAfter(lost map[string]time.Time, p string, r transcript.Record) (inLost, after bool) {
	at, ok := lost[p]
	if !ok {
		at, ok = lost[""]
	}
	if !ok {
		return false, false
	}
	return true, !r.Timestamp.IsZero() && !at.IsZero() && r.Timestamp.After(at)
}

// homeOfPurged is the project of session sid's purged home document, ""
// when none was purged: without a cursor, it names the home a resync would
// otherwise take from that document.
func homeOfPurged(sid string, tombs []state.Tombstone) string {
	for _, t := range tombs {
		if BaseDocumentID(t.Document) == "conversation:"+sid && t.Project != "" {
			return t.Project
		}
	}
	return ""
}

// SyncTombstones makes this client's tombstones and the control bank's
// the same, as every reconcile does first (syncTombstones).
func (f *Flusher) SyncTombstones(ctx context.Context) error { return f.syncTombstones(ctx) }
