package retain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

// The look for recreated purged documents (backsight issue 5). Hindsight
// 1.1.0 recreates a deleted document on append, holding only the new
// segment. A client unaware of tombstones (an older binary on another
// machine, a rollback to an older release, any other writer) appending to
// a purged session's id therefore brings that id back with whatever it
// sends next. A current binary purges it again, as forget --purge did;
// the turns that older binary sent after the purge go again to the live
// document (resync, lostToPurged), so none is lost.

// guardOverlap: how far before the newest document seen the next look
// starts. A document's created_at is its transaction's start (PostgreSQL
// NOW()), so a slow retain can commit a document older than one already
// seen.
const guardOverlap = time.Hour

// clockMargin: a creation time further than this past this machine's
// clock is taken for a wrong clock (a server host's clock jump, a volume
// from another host), never for the window's start: it would pin every
// later look in the future.
const clockMargin = 24 * time.Hour

// directLookups: up to this many tombstoned documents are read one by one
// with no other request (a purged document is a short 404).
const directLookups = 64

// documentsPerPage is the page of hindsight's document listing.
const documentsPerPage = 200

// queuePages: how many pages of 100 operations, per status, the look reads
// to know which documents an append is queued for.
const queuePages = 5

// perLook: the most documents one look purges again, each under its own
// hold of the maintenance lock; the rest wait for the next look.
const perLook = 10

// repeatMax purges again of one document within repeatWindow, unattended:
// past that, an older client keeps writing to it, and purging it every
// look would only cost consolidation and briefing pages each time.
const (
	repeatMax    = 3
	repeatWindow = 24 * time.Hour
)

// LookRecordEvery: a look that changed nothing records its time only
// when the last record is this old. With a tick every 15 minutes, the
// recorded time is then never more than 45 minutes old while the ticks
// run (doctor's staleness is twice this).
const LookRecordEvery = 30 * time.Minute

// Lookup says where FindRecreated looks.
type Lookup struct {
	// Since: zero looks for every tombstoned document; else only among the
	// documents created since then (the server's clock).
	Since time.Time
	// Recheck: documents found before, read again whatever their age.
	Recheck []string
	// Documents: the bank's document count when the caller has it, -1
	// when not (a full look then asks for it when it needs it).
	Documents int
	// NotAfter: creation times past it are left out of Scan.Newest.
	NotAfter time.Time
}

// Scan is what FindRecreated found.
type Scan struct {
	// Found: one tombstone per tombstoned document the server holds, by id.
	Found []state.Tombstone
	// Checked: the tombstoned documents compared; Full: each was looked
	// for, not only among the documents created since.
	Checked int
	Full    bool
	// Requests: the reads made.
	Requests int
	// Newest: the newest creation time among the documents listed, in the
	// server's clock (zero when none was listed).
	Newest time.Time
}

// tombstonedDocs is one tombstone per document of bank, by id; a
// tombstone of another bank, or of none, is left out.
func tombstonedDocs(bank string, tombs []state.Tombstone) map[string]state.Tombstone {
	out := map[string]state.Tombstone{}
	for _, t := range tombs {
		if _, ok := out[t.Document]; !ok && t.Document != "" && state.SameBank(t.Bank, bank) {
			out[t.Document] = t
		}
	}
	return out
}

func notFound(err error) bool {
	var apiErr *hindsight.APIError
	return errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound
}

// FindRecreated looks for the documents of bank that tombs name and that
// the server holds. With lk.Since zero, every tombstoned document is
// looked for: read one by one, or compared with the bank's whole listing
// when that takes fewer requests. Otherwise only the documents created
// since then are compared, in one listing, and lk.Recheck is read one by
// one. Only exact ids match: a successor (<id>#2) is a document of its
// own, found only when a tombstone names it. A bank the server does not
// have holds nothing.
func FindRecreated(ctx context.Context, cl *hindsight.Client, bank string, tombs []state.Tombstone, lk Lookup) (Scan, error) {
	want := tombstonedDocs(bank, tombs)
	sc := Scan{Checked: len(want)}
	if len(want) == 0 {
		return sc, nil
	}
	found := map[string]bool{}
	get := func(id string) error {
		sc.Requests++
		_, err := cl.GetDocument(ctx, bank, id)
		switch {
		case notFound(err):
			return nil
		case err != nil:
			return fmt.Errorf("bank %s, document %s: %w", bank, id, err)
		}
		found[id] = true
		return nil
	}
	match := func(items []hindsight.DocumentItem) {
		sc.Requests += len(items)/documentsPerPage + 1
		for _, it := range items {
			if _, ok := want[it.ID]; ok {
				found[it.ID] = true
			}
			if it.CreatedAt.After(sc.Newest) && (lk.NotAfter.IsZero() || !it.CreatedAt.After(lk.NotAfter)) {
				sc.Newest = it.CreatedAt
			}
		}
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if lk.Since.IsZero() {
		sc.Full = true
		direct := len(want) <= directLookups
		if !direct {
			n := lk.Documents
			if n < 0 {
				sc.Requests++
				var err error
				switch n, _, err = cl.NewestDocument(ctx, bank); {
				case notFound(err):
					return sc, nil
				case err != nil:
					return sc, fmt.Errorf("bank %s, counting its documents: %w", bank, err)
				}
			}
			direct = len(want) <= (n+documentsPerPage-1)/documentsPerPage
		}
		if direct {
			for _, id := range ids {
				if err := get(id); err != nil {
					return sc, err
				}
			}
		} else {
			items, err := cl.ListDocuments(ctx, bank)
			switch {
			case notFound(err):
				return sc, nil
			case err != nil:
				return sc, fmt.Errorf("bank %s, listing its documents: %w", bank, err)
			}
			match(items)
		}
	} else {
		items, err := cl.DocumentsCreatedSince(ctx, bank, lk.Since)
		switch {
		case notFound(err):
			return sc, nil
		case err != nil:
			return sc, fmt.Errorf("bank %s, listing its new documents: %w", bank, err)
		}
		match(items)
		for _, id := range lk.Recheck {
			if _, ok := want[id]; ok && !found[id] {
				if err := get(id); err != nil {
					return sc, err
				}
			}
		}
	}
	for _, id := range ids {
		if found[id] {
			sc.Found = append(sc.Found, want[id])
		}
	}
	return sc, nil
}

// GuardOptions say how a look runs.
type GuardOptions struct {
	// Full: look for every tombstoned document (jobs maintain, retain
	// purge-again); else only among the documents created since the
	// newest one seen (jobs tick), plus the ones waiting.
	Full bool
	// Hold: find and record, purge nothing; the reason goes with each
	// document found, as one the user must act on (the claude-code
	// provider, or a server this machine does not run, without consent).
	Hold string
	// Plan: find only, and write nothing (retain purge-again --plan).
	Plan bool
	// Manual: the user asked (retain purge-again): no cap on repeats.
	Manual bool
	// Remote: tombstones to add to state's, in memory only (a plan reads
	// the control bank's without writing them).
	Remote []state.Tombstone
}

// Guarded is what a look did.
type Guarded struct {
	Full     bool     `json:"full"`
	Checked  int      `json:"tombstones_checked"`
	Requests int      `json:"requests"`
	Purged   Repurged `json:"purged_again"`
	// Waiting: found, not purged again (yet), and why.
	Waiting []state.Recreated `json:"waiting,omitempty"`
}

// GuardPurges looks for tombstoned documents of the bank that the server
// holds again and purges them again, as forget --purge does (PurgeFound).
// A full look (opt.Full) compares every tombstone; the others list only
// the documents created since the newest one seen (less guardOverlap),
// one request whatever the tombstones' number, and read again the
// documents still waiting. The first one, with no newest seen yet, asks
// the server for it first.
//
// It never runs under the maintenance lock or a restore or update marker
// (an error, nothing done). A document an append is queued or running for
// waits: purging under it would let it recreate the document, with facts
// the purge never saw. So does each found document when a retain names no
// single document, or the queue is too long to read. Each purge takes the
// maintenance lock, as forget --purge does (a stale one is broken), one
// document at a time, at most perLook per look. Unattended, one document
// is purged again at most repeatMax times in repeatWindow: past that, an
// older client keeps writing to it, and it waits for the user. What waits
// is kept in state, read again by the next look, and shown by doctor. A
// failed read changes nothing in state.
func (f *Flusher) GuardPurges(ctx context.Context, opt GuardOptions) (Guarded, error) {
	var g Guarded
	if err := f.restoreHold(); err != nil {
		return g, err
	}
	if state.MaintenanceHeld(f.Dirs.MaintenanceLock()) {
		return g, errors.New("maintenance in progress")
	}
	st, err := state.Load(f.Dirs.StateFile())
	if err != nil {
		return g, err
	}
	all := state.State{Tombstones: st.TombstonesOf(f.Bank)}
	all.AddTombstones(opt.Remote...)
	tombs := all.TombstonesOf(f.Bank)
	if len(tombstonedDocs(f.Bank, tombs)) == 0 {
		return g, nil
	}
	prev := state.PurgeGuard{Bank: f.Bank}
	if st.PurgeGuard != nil && state.SameBank(st.PurgeGuard.Bank, f.Bank) {
		prev = *st.PurgeGuard
	}
	at := f.Now().UTC()
	notAfter := at.Add(clockMargin)
	sane := func(t time.Time) bool { return !t.IsZero() && !t.After(notAfter) }
	lk := Lookup{Documents: -1, NotAfter: notAfter}
	for _, w := range prev.Waiting {
		lk.Recheck = append(lk.Recheck, w.Document)
	}
	newest := prev.Newest
	if opt.Full || !sane(newest) {
		// The newest document first: one created during the look falls in
		// the next one's window.
		n, t, err := f.Client.NewestDocument(ctx, f.Bank)
		g.Requests++
		if err != nil && !notFound(err) {
			return g, fmt.Errorf("bank %s, its newest document: %w", f.Bank, err)
		}
		lk.Documents, newest = n, t
	}
	switch {
	case opt.Full:
	case sane(newest):
		lk.Since = newest.Add(-guardOverlap)
	case newest.IsZero():
		// The bank holds no document: only the waiting ones can be back.
		lk.Since = notAfter
	default:
		// Its newest document is dated past this machine's clock: the last
		// day, by this machine's clock, until it is behind.
		lk.Since = at.Add(-clockMargin)
	}
	sc, err := FindRecreated(ctx, f.Client, f.Bank, tombs, lk)
	g.Full, g.Checked, g.Requests = sc.Full, sc.Checked, g.Requests+sc.Requests
	if err != nil {
		return g, err
	}
	next := prev
	next.CheckedAt = at
	switch {
	case opt.Full:
		// A full look sets the newest again, never only forward: a wrong
		// date seen once must not pin the window.
		next.Newest, next.FullAt = time.Time{}, at
		for _, t := range []time.Time{newest, sc.Newest} {
			if sane(t) && t.After(next.Newest) {
				next.Newest = t
			}
		}
	default:
		if !sane(next.Newest) {
			next.Newest = time.Time{}
		}
		for _, t := range []time.Time{newest, sc.Newest} {
			if sane(t) && t.After(next.Newest) {
				next.Newest = t
			}
		}
	}
	// The repeats of the last day.
	// The repeats within the last day, a sliding window.
	next.Repeats = nil
	for _, r := range prev.Repeats {
		r.At = slices.DeleteFunc(slices.Clone(r.At), func(t time.Time) bool { return at.Sub(t) >= repeatWindow })
		if len(r.At) > 0 {
			next.Repeats = append(next.Repeats, r)
		}
	}
	repeat := func(doc string) *state.Repurge {
		for i := range next.Repeats {
			if next.Repeats[i].Document == doc {
				return &next.Repeats[i]
			}
		}
		return nil
	}
	type hold struct {
		why  string
		user bool
	}
	waiting := map[string]hold{}
	var purge []state.Tombstone
	if len(sc.Found) > 0 {
		var why map[string]string
		switch {
		case opt.Hold != "":
			why = map[string]string{"": opt.Hold}
		case opt.Plan:
			why = map[string]string{"": "a plan purges nothing"}
		default:
			why, err = queuedAppends(ctx, f.Client, f.Bank, sc.Found)
			if err != nil {
				why = map[string]string{"": "cannot read the bank's queue: " + errSummary(err)}
			}
		}
		for _, t := range sc.Found {
			r := repeat(t.Document)
			switch w := why[""] + why[t.Document]; {
			case opt.Hold != "":
				waiting[t.Document] = hold{w, true}
			case w != "":
				waiting[t.Document] = hold{w, false}
			case !opt.Manual && r != nil && len(r.At) >= repeatMax:
				waiting[t.Document] = hold{fmt.Sprintf("an older client keeps writing to it (purged again %d times since %s UTC): update backsight on every machine that writes to this bank, then run backsight retain purge-again",
					len(r.At), r.At[0].Format("2006-01-02 15:04")), true}
			case len(purge) >= perLook:
				waiting[t.Document] = hold{fmt.Sprintf("at most %d documents are purged again per look: the next one does it", perLook), false}
			default:
				purge = append(purge, t)
			}
		}
	}
	for _, t := range purge {
		l, _, err := state.Acquire(f.Dirs.MaintenanceLock(), "purge again (recreated purged documents)")
		if err != nil {
			waiting[t.Document] = hold{"maintenance in progress: " + errSummary(err), false}
			continue
		}
		done, err := PurgeFound(ctx, f.Client, f.Bank, []state.Tombstone{t})
		_ = l.Release()
		g.Purged.Observations += done.Observations
		g.Purged.Pages = append(g.Purged.Pages, done.Pages...)
		if err != nil {
			waiting[t.Document] = hold{errSummary(err), false}
			continue
		}
		g.Purged.Documents = append(g.Purged.Documents, t.Document)
		if r := repeat(t.Document); r != nil {
			r.At = append(r.At, at)
		} else {
			next.Repeats = append(next.Repeats, state.Repurge{Document: t.Document, At: []time.Time{at}})
		}
	}
	sort.Strings(g.Purged.Pages)
	g.Purged.Pages = compactStrings(g.Purged.Pages)
	next.Waiting = nil
	for _, t := range sc.Found {
		if w, ok := waiting[t.Document]; ok {
			next.Waiting = append(next.Waiting, state.Recreated{Document: t.Document, Session: t.Session, Project: t.Project, Why: w.why, NeedsUser: w.user})
		}
	}
	g.Waiting = next.Waiting
	if len(g.Purged.Documents) > 0 {
		next.Purged, next.PurgedAt = g.Purged.Documents, at
	}
	if opt.Plan {
		return g, nil
	}
	// Nothing but the time of the look changed, and that was recorded
	// within LookRecordEvery: state.json is left alone (it is written under
	// a lock other commands wait on).
	quiet, was := next, prev
	quiet.CheckedAt, was.CheckedAt = time.Time{}, time.Time{}
	if reflect.DeepEqual(quiet, was) && at.Sub(prev.CheckedAt) < LookRecordEvery {
		return g, nil
	}
	return g, state.Update(f.Dirs, "purge guard", func(s *state.State) error {
		s.PurgeGuard = &next
		return nil
	})
}

// LookSince is where an incremental look at bank, given the last one
// recorded in st, would start: zero when none was recorded (no newest
// seen, or another bank's); recheck are the documents found before and
// still waiting.
func LookSince(st state.State, bank string, at time.Time) (since time.Time, recheck []string) {
	prev := st.PurgeGuard
	if prev == nil || !state.SameBank(prev.Bank, bank) {
		return time.Time{}, nil
	}
	for _, w := range prev.Waiting {
		recheck = append(recheck, w.Document)
	}
	if prev.Newest.IsZero() || prev.Newest.After(at.Add(clockMargin)) {
		return time.Time{}, recheck
	}
	return prev.Newest.Add(-guardOverlap), recheck
}

// queuedAppends says, for each of found, why it must wait for the bank's
// queue: a retain queued or running names it; under "" (every one): a
// retain names no single document (a batch of several), or the queue is
// longer than the look reads.
func queuedAppends(ctx context.Context, cl *hindsight.Client, bank string, found []state.Tombstone) (map[string]string, error) {
	out := map[string]string{}
	for _, status := range []string{"pending", "processing"} {
		ops, complete, err := cl.QueuedOperationsOf(ctx, bank, status, queuePages)
		if err != nil {
			return nil, err
		}
		if !complete {
			return map[string]string{"": fmt.Sprintf("the bank has more than %d %s operations: purged again once fewer are queued", len(ops), status)}, nil
		}
		for _, op := range ops {
			if !strings.Contains(op.Type, "retain") {
				continue
			}
			if op.DocumentID == "" {
				return map[string]string{"": fmt.Sprintf("a retain of several documents is %s (operation %s): purged again once it ran", status, op.ID)}, nil
			}
			if slices.ContainsFunc(found, func(t state.Tombstone) bool { return t.Document == op.DocumentID }) {
				out[op.DocumentID] = fmt.Sprintf("an append to it is %s (operation %s): purged again once it ran", status, op.ID)
			}
		}
	}
	return out, nil
}
