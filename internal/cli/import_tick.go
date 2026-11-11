package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fileimport"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

// tickImportLimit: at most this many imported files per tick, all
// folders together.
var tickImportLimit = 10

// importTickResult is what a tick did with queued imports.
type importTickResult struct {
	Sent    int      `json:"sent"`
	Left    int      `json:"left"`
	Gone    int      `json:"gone"`
	Paused  []string `json:"paused,omitempty"`  // sources paused at their cap
	Waiting string   `json:"waiting,omitempty"` // past_sessions | busy
}

func (r importTickResult) String() string {
	if r.Sent+r.Left+r.Gone == 0 && r.Waiting == "" && len(r.Paused) == 0 {
		return ""
	}
	if r.Waiting == "past_sessions" {
		return fmt.Sprintf("waiting for past sessions (%d file(s) queued)", r.Left)
	}
	msg := fmt.Sprintf("%d sent, %d left", r.Sent, r.Left)
	if r.Gone > 0 {
		msg += fmt.Sprintf(", %d no longer in their folder", r.Gone)
	}
	if len(r.Paused) > 0 {
		msg += ", paused at the cap: " + strings.Join(r.Paused, " ")
	}
	if r.Waiting == "busy" {
		msg += ", server busy"
	}
	return msg
}

// acquireImport takes the lock every writer of the import manifests holds:
// the tick while it sends, `import <folder>` while it merges, `import
// purge` while it deletes.
func acquireImport(d paths.Dirs, command string) (func(), error) {
	l, _, err := state.Acquire(filepath.Join(d.LocksDir(), "job-import.lock"), command)
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}

// emitImportTick records what a tick did for one folder: files sent, a
// pause at the cap, the end of the folder.
func emitImportTick(tw *telemetry.Writer, m *fileimport.Manifest, sent int, paused bool) {
	if tw == nil {
		return
	}
	spend := map[string]any{"source": m.Source, "files": sent, "spent_tokens": m.SpentTokens, "spent_cost": m.SpentCost}
	if sent > 0 {
		tw.Emit(telemetry.Raw{Name: "import", Fields: withAction(spend, "send")})
	}
	if paused {
		tw.Emit(telemetry.Raw{Name: "import", Fields: withAction(spend, "pause")})
	}
	if sent > 0 && len(m.Unsent()) == 0 {
		tw.Emit(telemetry.Raw{Name: "import", Fields: map[string]any{"action": "done", "source": m.Source, "files": len(m.Entries),
			"spent_tokens": m.PriorTokens + m.SpentTokens, "spent_cost": m.PriorCost + m.SpentCost}})
	}
}

func withAction(fields map[string]any, action string) map[string]any {
	out := map[string]any{"action": action}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

// filesImportInTick sends queued imported files, oldest queued folder
// first, while no past sessions are queued, each folder within its cap.
func filesImportInTick(ctx context.Context, f *retain.Flusher, d paths.Dirs, cfg config.Config, tw *telemetry.Writer) (importTickResult, error) {
	var r importTickResult
	all, err := fileimport.ListManifests(d.ImportDir())
	if err != nil || len(all) == 0 {
		return r, err
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		return r, err
	}
	for _, m := range all {
		r.Left += len(m.Unsent())
	}
	if r.Left == 0 {
		return r, nil
	}
	if st.Backfill != nil && len(st.Backfill.Sessions) > 0 {
		r.Waiting = "past_sessions"
		return r, nil
	}
	release, err := acquireImport(d, "jobs tick")
	if err != nil {
		r.Waiting = "busy"
		return r, nil
	}
	defer release()
	// Read again under the lock: an import or a purge may have changed
	// them since.
	if all, err = fileimport.ListManifests(d.ImportDir()); err != nil {
		return r, err
	}
	r.Left = 0
	prices := retain.PricesForLLM(cfg.LLM)
	bank, tombs := cfg.Server.Bank, st.TombstonesOf(cfg.Server.Bank)
	machineID := retain.ReadMachineID(d.State)
	budget := tickImportLimit
	for i := range all {
		m := &all[i]
		left := len(m.Unsent())
		switch {
		case left == 0:
			continue
		case m.Paused != "":
			r.Paused, r.Left = append(r.Paused, m.Source), r.Left+left
			continue
		case cfg.LLM.Provider == "claude-code" && !m.AllowSubscription, budget == 0:
			r.Left += left
			continue
		}
		before := r
		sent, err := sendImport(ctx, f.Client, d, m, bank, tombs, machineID, prices, importKeptOut(f), &budget, &r)
		r.Left += left - sent
		emitImportTick(tw.For(m.Project), m, r.Sent-before.Sent, len(r.Paused) > len(before.Paused))
		if err != nil {
			return r, err
		}
	}
	return r, nil
}

// importKeptOut reports whether a working folder is kept out of memory, as for
// a session (privacy.exclude_paths, .backsight.toml); nil without a
// resolver.
func importKeptOut(f *retain.Flusher) func(cwd string) bool {
	if f == nil || f.Resolver == nil {
		return nil
	}
	return func(cwd string) bool {
		out, _ := f.Resolver.OptedOut(cwd, f.Exclude)
		return out
	}
}

// sendImport sends m's unsent files until budget runs out, the cap is
// reached or the server stays busy; it saves the manifest after each file
// and returns the files it finished (sent or gone). optedOut checks a
// preset's file mapped to a working folder again just before its send.
func sendImport(ctx context.Context, c *hindsight.Client, d paths.Dirs, m *fileimport.Manifest, bank string, tombs []state.Tombstone,
	machineID string, prices retain.Prices, optedOut func(string) bool, budget *int, r *importTickResult) (int, error) {
	done := 0
	save := func() error { return fileimport.SaveManifest(d.ImportDir(), *m) }
	for i := range m.Entries {
		e := &m.Entries[i]
		if e.Sent || e.Gone {
			continue
		}
		if *budget == 0 {
			return done, nil
		}
		days, err := c.LLMTokensByDay(ctx, bank, "30d")
		if err != nil {
			return done, err
		}
		if m.StartDay == "" {
			// The spend counts from the first send, not from the queue: past
			// sessions sent before it are not this import's.
			m.StartDay = now().UTC().Format("2006-01-02")
			t := days[m.StartDay]
			m.StartBase = [4]int64{t.Input, t.Cached, t.Output, t.Thoughts}
		}
		m.SpentTokens, m.SpentCost = spendSince(days, m.StartDay, m.StartBase, prices)
		if (m.MaxCost > 0 && m.SpentCost >= m.MaxCost) || (m.MaxTokens > 0 && m.SpentTokens >= m.MaxTokens) {
			m.Paused = "budget"
			r.Paused = append(r.Paused, m.Source)
			return done, save()
		}
		if err := waitQueue(ctx, c, bank, backfillQueueLimit, tickQueueWait, nil); err != nil {
			if errors.Is(err, errQueueBusy) {
				r.Waiting = "busy"
				return done, save()
			}
			return done, err
		}
		abs := filepath.Join(m.Root, filepath.FromSlash(e.Rel))
		raw, redacted, reason := fileimport.ReadForSend(abs)
		var codexDate time.Time
		codexDated := false
		if reason == "" && m.Preset == fileimport.PresetCodex {
			// Its header is Codex's, read again: a folder kept out since the
			// import is not sent.
			codexDate, codexDated, reason = fileimport.CodexRecheck(e.Rel, raw, optedOut)
		}
		if reason != "" {
			// Gone, a link, too large, empty or no longer text: nothing is
			// sent, what an earlier send stored stays.
			e.Gone = true
			r.Gone++
			done++
			if err := save(); err != nil {
				return done, err
			}
			continue
		}
		if sha := sha256Hex(string(raw)); sha != e.SHA256 {
			e.SHA256 = sha
			if codexDated {
				e.Date = codexDate
			} else if t, ok := fileimport.FrontmatterDate(raw, now()); ok {
				e.Date = t
			} else if info, err := os.Stat(abs); err == nil {
				e.Date = info.ModTime().UTC()
			}
		}
		if slices.ContainsFunc(tombs, func(t state.Tombstone) bool { return t.Document == e.Document }) {
			e.Document = fileimport.DocumentID(m.Source, e.Rel, tombs)
		}
		project, movedFrom := m.ProjectOf(*e), ""
		if e.SentProject != "" && e.SentProject != project {
			// The file now belongs to another project: what the old one drew
			// from its document is cleared before the document is replaced
			// with its new tags, so none of it stays in the old project.
			movedFrom = e.SentProject
			if err := clearDocumentObservations(ctx, c, bank, e.Document); err != nil {
				return done, errors.Join(err, save())
			}
		}
		item, op := retain.ImportItem(bank, retain.ImportFile{Source: m.Source, Rel: e.Rel, DocumentID: e.Document, Content: redacted,
			SHA256: e.SHA256, Project: project, MovedFrom: movedFrom, MachineID: machineID, Version: buildinfo.Version, Date: e.Date})
		if err := c.Retain(ctx, bank, []hindsight.RetainItem{item}, op); err != nil {
			return done, errors.Join(err, save())
		}
		e.Sent, e.SentProject = true, project
		r.Sent++
		done++
		*budget--
		if err := save(); err != nil {
			return done, err
		}
	}
	return done, nil
}

// clearDocumentObservations clears the observations drawn from each live
// fact of document doc, as a purge does (retain.PurgeWrites), leaving the
// facts to the replacement of their document. A fact gone already is done.
func clearDocumentObservations(ctx context.Context, c *hindsight.Client, bank, doc string) error {
	var facts []string
	if err := c.ListMemories(ctx, bank, hindsight.MemoryQuery{DocumentID: doc}, func(mem hindsight.Memory) error {
		if mem.DocumentID == doc && mem.FactType != "observation" && mem.State != "invalidated" {
			facts = append(facts, mem.ID)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("the facts of %s: %w", doc, err)
	}
	for _, id := range facts {
		var apiErr *hindsight.APIError
		if err := c.ClearObservations(ctx, bank, id); err != nil && (!errors.As(err, &apiErr) || apiErr.Kind != hindsight.NotFound) {
			return fmt.Errorf("clearing the observations of %s: %w", id, err)
		}
	}
	return nil
}
