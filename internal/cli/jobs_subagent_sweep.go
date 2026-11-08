package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The sweep of the stored subagent reports that fail the report gate
// (decision 0083 C3), the one deletion backsight makes on its own: the
// rule is measured (a report under the gate cost an extraction and almost
// never gave a memory used) and narrow (one source, one rule). It runs
// once per gate and restore epoch (a marker in state.json), and again when
// either changes: a restore brings the swept reports back.
//
// It writes no tombstone, unlike forget --purge. A subagent resumed later
// reports again under the same document id; a tombstone would have the
// purge guard delete that real report, again and again. Nothing sends a
// swept stub again without one: SubagentStop and a reconcile's rebuild
// both apply the same gate.

// sweepBatch documents go per purge, each under the maintenance lock: a
// long sweep lets a backup or a purge in between, and what it purged stays
// purged when a later batch fails.
const sweepBatch = 100

// sweepCandidate is a stored report that fails the gate, with the text it
// was judged on.
type sweepCandidate struct {
	id    string
	text  string
	facts []retain.PurgeFact
}

// maintainSubagentSweep is jobs maintain's sweep step: "" when it has
// nothing to do (already done for this gate and epoch, recording off),
// else a line for the job's report. backupFailed: today's backup failed,
// so the sweep waits for a day whose backup holds what it deletes. Never
// an error: a sweep not run or not finished leaves the marker as it was,
// and the next run tries again.
func maintainSubagentSweep(ctx context.Context, d paths.Dirs, tw *telemetry.Writer, backupFailed bool) string {
	cfg, _, err := loadServerConfig()
	if err != nil || !cfg.Components.Write {
		return ""
	}
	gate := retain.SubagentGate(cfg.Hooks.SubagentReportMinChars)
	bank := cfg.Server.Bank
	st, err := state.Load(d.StateFile())
	if err != nil {
		return "subagent sweep: not run (" + err.Error() + "), the next run tries again"
	}
	epoch := st.EpochOf(bank)
	if m := st.SubagentSweep; m != nil && m.Gate == gate && state.SameBank(m.Bank, bank) && m.Epoch == epoch {
		return ""
	}
	notRun := func(why string) string {
		tw.Emit(telemetry.Raw{Name: "subagent_sweep", Fields: map[string]any{"status": "skipped", "reason": why, "gate": gate}})
		return "subagent sweep: not run (" + why + "), the next run tries again"
	}
	switch {
	case backupFailed:
		return notRun("today's backup failed")
	case state.RestoreIncomplete(d):
		return notRun("a restore is in progress or did not finish")
	case state.UpdateIncomplete(d):
		return notRun("an update is in progress or did not finish")
	case otherServer(d, cfg) != nil:
		return notRun("a different server answers at the joined address")
	}
	r, err := runSubagentSweep(ctx, cfg.Hooks.SubagentReportMinChars)
	fields := map[string]any{"gate": gate, "checked": r.checked, "purged": len(r.purged), "facts": r.facts, "unread": r.unread, "changed": r.changed}
	if err != nil {
		var oe *output.Error
		why := err.Error()
		if errors.As(err, &oe) {
			why = oe.Message
		}
		fields["status"], fields["reason"] = "failed", why
		tw.Emit(telemetry.Raw{Name: "subagent_sweep", Fields: fields})
		if len(r.purged) == 0 {
			return "subagent sweep: not run (" + why + "), the next run tries again"
		}
		return fmt.Sprintf("subagent sweep: %d short subagent report(s) purged, then stopped (%s), the next run goes on", len(r.purged), why)
	}
	fields["status"] = "done"
	tw.Emit(telemetry.Raw{Name: "subagent_sweep", Fields: fields})
	_ = state.UpdateWithin(d, "jobs maintain", recordWait, func(s *state.State) error {
		s.SubagentSweep = &state.SubagentSweep{Bank: bank, Gate: gate, Epoch: epoch, At: now().UTC().Truncate(time.Second), Checked: r.checked, Purged: len(r.purged)}
		return nil
	})
	out := fmt.Sprintf("subagent sweep: %d short subagent report(s) purged (%d fact(s)) of %d", len(r.purged), r.facts, r.checked)
	if r.unread > 0 {
		out += fmt.Sprintf(", %d with facts but no readable text left", r.unread)
	}
	return out
}

type sweepResult struct {
	checked, facts int
	// unread: documents with facts whose text the server gave empty,
	// never judged; changed: candidates whose text changed (or now
	// passes) before their batch, left.
	unread, changed int
	purged          []string
}

// runSubagentSweep lists the bank's subagent:* documents, reads each
// one's text, and purges those that fail the gate as forget --purge does
// (retain.PurgeWrites: the observations of their facts cleared, the facts
// retired, the documents deleted), without tombstones, a batch at a time.
// A report of zero facts goes too; one with facts and no text is left.
// Briefing pages are kept: a status report holds no secret, and the
// server rewrites them after the consolidation the purge starts.
func runSubagentSweep(ctx context.Context, minChars int) (sweepResult, error) {
	var r sweepResult
	cl, bank, gate, err := curateWriter() // refuses under the maintenance lock
	if err != nil {
		return r, err
	}
	items, err := cl.ListDocuments(ctx, bank)
	if err != nil {
		return r, fmt.Errorf("listing the documents of %s: %w", bank, err)
	}
	var found []sweepCandidate
	for _, it := range items {
		if !strings.HasPrefix(it.ID, "subagent:") {
			continue
		}
		r.checked++
		doc, err := cl.GetDocument(ctx, bank, it.ID)
		switch {
		case notFound(err):
			continue // deleted since the listing
		case err != nil:
			return r, fmt.Errorf("reading document %s: %w", it.ID, err)
		case strings.TrimSpace(doc.OriginalText) == "" && doc.MemoryUnits > 0:
			r.unread++ // its text unknown: never judged on an empty read
			continue
		case retain.KeepsSubagentReport(doc.OriginalText, minChars):
			continue
		}
		c := sweepCandidate{id: it.ID, text: doc.OriginalText}
		var foreign string
		err = cl.ListMemories(ctx, bank, hindsight.MemoryQuery{DocumentID: it.ID}, func(m hindsight.Memory) error {
			switch {
			case m.DocumentID != it.ID:
				foreign = m.ID // a listing that did not filter: touch nothing
			case m.FactType != "observation": // an observation is cleared through its sources
				c.facts = append(c.facts, retain.PurgeFact{ID: m.ID, State: m.State})
			}
			return nil
		})
		if err != nil {
			return r, fmt.Errorf("listing the facts of %s: %w", it.ID, err)
		}
		if foreign != "" {
			return r, fmt.Errorf("the listing of document %s's facts named memory %s of another document: nothing purged", it.ID, foreign)
		}
		found = append(found, c)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].id < found[j].id })
	for start := 0; start < len(found); start += sweepBatch {
		done, changed, err := sweepWrite(ctx, cl, bank, gate, minChars, found[start:min(start+sweepBatch, len(found))])
		r.changed += changed
		for _, c := range done {
			r.purged = append(r.purged, c.id)
			r.facts += len(c.facts)
		}
		if err != nil {
			return r, err
		}
	}
	return r, nil
}

// sweepWrite purges one batch under the maintenance lock, after reading
// each document again: one whose text changed since it was judged (a
// resumed subagent's new report), or that is gone, is left. gate
// (curateWriter's) is checked under the lock, which connect takes too.
func sweepWrite(ctx context.Context, cl *hindsight.Client, bank string, gate func() error, minChars int, batch []sweepCandidate) (done []sweepCandidate, changed int, err error) {
	d, _, err := machine()
	if err != nil {
		return nil, 0, err
	}
	cfg, _, err := loadServerConfig()
	if err != nil {
		return nil, 0, err
	}
	if err := otherServerRefusal(d, cfg); err != nil {
		return nil, 0, err
	}
	l, err := state.TryAcquire(d.MaintenanceLock(), "jobs maintain (subagent sweep)")
	var held *state.HeldError
	if errors.As(err, &held) {
		return nil, 0, heldRefusal(held)
	}
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = l.Release() }()
	if err := gate(); err != nil {
		return nil, 0, err
	}
	var facts []retain.PurgeFact
	var ids []string
	for _, c := range batch {
		doc, err := cl.GetDocument(ctx, bank, c.id)
		switch {
		case notFound(err):
			continue
		case err != nil:
			return nil, changed, fmt.Errorf("reading document %s again: %w", c.id, err)
		case doc.OriginalText != c.text || retain.KeepsSubagentReport(doc.OriginalText, minChars):
			changed++
			continue
		}
		facts = append(facts, c.facts...)
		ids = append(ids, c.id)
		done = append(done, c)
	}
	if len(ids) == 0 {
		return nil, changed, nil
	}
	if _, _, err := retain.PurgeWrites(ctx, cl, bank, facts, ids, nil, "the next jobs maintain goes on"); err != nil {
		return nil, changed, err
	}
	return done, changed, nil
}
