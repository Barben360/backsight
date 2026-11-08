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
// once per gate (a marker in state.json) and again when the gate changes.
const (
	// sweepBatch documents go per purge, each under the maintenance lock:
	// a long sweep lets a backup or a purge in between, and what it purged
	// stays purged when a later batch fails.
	sweepBatch = 100
	// sweepQuiet: a report younger than this is left. Its agent may still
	// be resumed and report again under the same document, which a
	// tombstone would have the purge guard delete.
	sweepQuiet = 24 * time.Hour
)

// sweepCandidate is a stored report that fails the gate.
type sweepCandidate struct {
	doc   purgeDoc
	facts []hindsight.MemoryDetail
}

// maintainSubagentSweep is jobs maintain's sweep step: "" when it has
// nothing to do (already done for this gate, recording off), else a line
// for the job's report. backupFailed: today's backup failed, so the sweep
// waits for a day whose backup holds what it deletes. Never an error: a
// sweep not run or not finished leaves the marker as it was, and the next
// run tries again.
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
	if m := st.SubagentSweep; m != nil && m.Gate == gate && state.SameBank(m.Bank, bank) {
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
	fields := map[string]any{"gate": gate, "checked": r.checked, "purged": len(r.purged), "facts": r.facts, "recent_left": r.recent}
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
		s.SubagentSweep = &state.SubagentSweep{Bank: bank, Gate: gate, At: now().UTC().Truncate(time.Second), Checked: r.checked, Purged: len(r.purged)}
		return nil
	})
	out := fmt.Sprintf("subagent sweep: %d short subagent report(s) purged (%d fact(s)) of %d", len(r.purged), r.facts, r.checked)
	if r.recent > 0 {
		out += fmt.Sprintf(", %d from the last day not looked at", r.recent)
	}
	return out
}

type sweepResult struct {
	checked, facts, recent int
	purged                 []string
}

// runSubagentSweep lists the bank's subagent:* documents, reads each
// one's text, and purges those that fail the gate as forget --purge does
// (purgeWrite: tombstones first, the observations of their facts cleared,
// the facts retired, the documents deleted, the tombstones mirrored), a
// batch at a time. A report of zero facts goes too. Briefing pages are
// kept: a status report holds no secret, and the server rewrites them
// after the consolidation the purge starts.
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
	quiet := now().UTC().Add(-sweepQuiet)
	var found []sweepCandidate
	for _, it := range items {
		if !strings.HasPrefix(it.ID, "subagent:") {
			continue
		}
		if it.CreatedAt.After(quiet) {
			r.recent++ // left unread
			continue
		}
		r.checked++
		doc, err := cl.GetDocument(ctx, bank, it.ID)
		switch {
		case notFound(err):
			continue // deleted since the listing
		case err != nil:
			return r, fmt.Errorf("reading document %s: %w", it.ID, err)
		case retain.KeepsSubagentReport(doc.OriginalText, minChars):
			continue
		}
		c := sweepCandidate{doc: purgeDoc{ID: it.ID, Memories: doc.MemoryUnits, tomb: retain.TombstoneOf(bank, it.ID, doc.Metadata, time.Time{})}}
		var foreign string
		err = cl.ListMemories(ctx, bank, hindsight.MemoryQuery{DocumentID: it.ID}, func(m hindsight.Memory) error {
			switch {
			case m.DocumentID != it.ID:
				foreign = m.ID // a listing that did not filter: touch nothing
			case m.FactType != "observation": // an observation is cleared through its sources
				c.facts = append(c.facts, hindsight.MemoryDetail{ID: m.ID, Type: m.FactType, Tags: m.Tags, DocumentID: m.DocumentID, State: m.State})
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
	sort.Slice(found, func(i, j int) bool { return found[i].doc.ID < found[j].doc.ID })
	for start := 0; start < len(found); start += sweepBatch {
		batch := found[start:min(start+sweepBatch, len(found))]
		var facts []hindsight.MemoryDetail
		docs := make([]purgeDoc, 0, len(batch))
		for _, c := range batch {
			facts = append(facts, c.facts...)
			docs = append(docs, c.doc)
		}
		if _, _, err := purgeWrite(ctx, cl, bank, gate, facts, docs, map[string]bool{}, "the next jobs maintain goes on"); err != nil {
			return r, err
		}
		for _, c := range batch {
			r.purged = append(r.purged, c.doc.ID)
			r.facts += len(c.facts)
		}
	}
	return r, nil
}
