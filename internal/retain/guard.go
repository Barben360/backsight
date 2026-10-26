package retain

import (
	"context"
	"errors"
	"fmt"
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
// sends next. A current binary purges it again, as forget --purge did.

// guardOverlap: how far before the newest document seen the next look
// starts. A document's created_at is its transaction's start (PostgreSQL
// NOW()), so a slow retain can commit a document older than one already
// seen.
const guardOverlap = time.Hour

// guardFullEvery: how often the look reads every tombstone, not only the
// documents created since the last look.
const guardFullEvery = 24 * time.Hour

// directLookups: up to this many tombstoned documents are read one by one
// with no other request (a purged document is a short 404).
const directLookups = 64

// documentsPerPage is the page of hindsight's document listing.
const documentsPerPage = 200

// queuePages: how many pages of 100 operations, per status, the look reads
// to know which documents an append is queued for.
const queuePages = 5

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

// tombstonedDocs is one tombstone per document of bank, by id.
func tombstonedDocs(bank string, tombs []state.Tombstone) map[string]state.Tombstone {
	out := map[string]state.Tombstone{}
	for _, t := range tombs {
		if _, ok := out[t.Document]; !ok && t.Document != "" && (t.Bank == "" || t.Bank == bank) {
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
// the server holds. With since zero, every tombstoned document is looked
// for: read one by one, or compared with the bank's whole listing when
// that takes fewer requests. Otherwise only the documents created since
// then (the server's clock) are compared, in one listing, and recheck
// (documents found before and not purged again yet) is read one by one.
// Only exact ids match: a successor (<id>#2) is a document of its own,
// found only when a tombstone names it. A bank the server does not have
// holds nothing.
func FindRecreated(ctx context.Context, cl *hindsight.Client, bank string, tombs []state.Tombstone, since time.Time, recheck []string) (Scan, error) {
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
			if it.CreatedAt.After(sc.Newest) {
				sc.Newest = it.CreatedAt
			}
		}
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if since.IsZero() {
		sc.Full = true
		direct := len(want) <= directLookups
		if !direct {
			sc.Requests++
			n, _, err := cl.NewestDocument(ctx, bank)
			switch {
			case notFound(err):
				return sc, nil
			case err != nil:
				return sc, fmt.Errorf("bank %s, counting its documents: %w", bank, err)
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
		items, err := cl.DocumentsCreatedSince(ctx, bank, since)
		switch {
		case notFound(err):
			return sc, nil
		case err != nil:
			return sc, fmt.Errorf("bank %s, listing its new documents: %w", bank, err)
		}
		match(items)
		for _, id := range recheck {
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
	// All: look for every tombstoned document, whatever earlier looks saw
	// (retain purge-again).
	All bool
	// Hold: find and record, purge nothing; the reason goes with each
	// document found (the claude-code provider without the user's consent,
	// or a plan).
	Hold string
	// Plan: find only, as Hold, and record nothing (retain purge-again
	// --plan).
	Plan bool
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
// A full look (every tombstone) runs on the first look, when a tombstone
// was learned since the last full one, once a day, and with All; the
// looks between list only the documents created since the newest one
// seen (less guardOverlap), one request whatever the tombstones' number.
//
// It never runs under the maintenance lock or a restore or update marker
// (an error, nothing done). A document an append is queued or running for
// waits: purging under it would let it recreate the document, with facts
// the purge never saw. So does each found document when a retain names no
// single document, or the queue is too long to read. The purge itself
// takes the maintenance lock, as forget --purge does. What waits is kept
// in state, read again by the next look, and shown by doctor.
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
	tombs := st.TombstonesOf(f.Bank)
	want := tombstonedDocs(f.Bank, tombs)
	if len(want) == 0 {
		return g, nil
	}
	prev := state.PurgeGuard{Bank: f.Bank}
	if st.PurgeGuard != nil && st.PurgeGuard.Bank == f.Bank {
		prev = *st.PurgeGuard
	}
	at := f.Now().UTC()
	since, recheck := LookSince(st, f.Bank, len(want), at, opt.All)
	var newest time.Time
	if g.Full = since.IsZero(); g.Full {
		// The newest document first: one created during the look falls in
		// the next one's window.
		_, t, err := f.Client.NewestDocument(ctx, f.Bank)
		if err != nil && !notFound(err) {
			return g, fmt.Errorf("bank %s, its newest document: %w", f.Bank, err)
		}
		newest = t
		g.Requests++
	}
	sc, err := FindRecreated(ctx, f.Client, f.Bank, tombs, since, recheck)
	g.Checked, g.Requests = sc.Checked, g.Requests+sc.Requests
	if err != nil {
		return g, err
	}
	next := prev
	next.CheckedAt = at
	if g.Full {
		next.FullAt, next.Tombstones = at, len(want)
	}
	for _, t := range []time.Time{newest, sc.Newest} {
		if t.After(next.Newest) {
			next.Newest = t
		}
	}
	waiting := map[string]string{}
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
		}
		if err != nil {
			why = map[string]string{"": "cannot read the bank's queue: " + errSummary(err)}
		}
		for _, t := range sc.Found {
			if w := why[""] + why[t.Document]; w != "" {
				waiting[t.Document] = w
			} else {
				purge = append(purge, t)
			}
		}
	}
	if len(purge) > 0 {
		l, err := state.TryAcquire(f.Dirs.MaintenanceLock(), "purge again (recreated purged documents)")
		if err != nil {
			for _, t := range purge {
				waiting[t.Document] = "maintenance in progress: " + errSummary(err)
			}
		} else {
			g.Purged, err = PurgeFound(ctx, f.Client, f.Bank, purge)
			_ = l.Release()
			for _, t := range purge {
				if err != nil && !slices.Contains(g.Purged.Documents, t.Document) {
					waiting[t.Document] = errSummary(err)
				}
			}
		}
	}
	next.Waiting = nil
	for _, t := range sc.Found {
		if w, ok := waiting[t.Document]; ok {
			next.Waiting = append(next.Waiting, state.Recreated{Document: t.Document, Session: t.Session, Project: t.Project, Why: w})
		}
	}
	g.Waiting = next.Waiting
	if len(g.Purged.Documents) > 0 {
		next.Purged, next.PurgedAt = g.Purged.Documents, at
	}
	if opt.Plan {
		return g, nil
	}
	return g, state.Update(f.Dirs, "purge guard", func(s *state.State) error {
		s.PurgeGuard = &next
		return nil
	})
}

// LookSince is where the next look at bank starts, given the last one
// recorded in st: zero for a full look (the first, one with more
// tombstoned documents than the last full look saw, a day after it, or
// all), else an overlap before the newest document seen; recheck are the
// documents found before and still waiting.
func LookSince(st state.State, bank string, tombstoned int, at time.Time, all bool) (since time.Time, recheck []string) {
	prev := st.PurgeGuard
	if prev == nil || prev.Bank != bank {
		return time.Time{}, nil
	}
	for _, w := range prev.Waiting {
		recheck = append(recheck, w.Document)
	}
	if all || prev.Newest.IsZero() || tombstoned > prev.Tombstones || at.Sub(prev.FullAt) >= guardFullEvery {
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
