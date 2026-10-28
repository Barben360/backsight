package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/selfcheck"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

func newJobsCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "jobs", Short: "Periodic jobs run by the OS scheduler", Args: cobra.NoArgs}
	cmd.AddCommand(newJobsTickCmd(e), newJobsMaintainCmd(e), newJobsInstallCmd(e), newJobsUninstallCmd(e), newJobsRefreshCmd(e), newJobsStartCmd(e))
	return cmd
}

type tickResult struct {
	Status   string             `json:"status"` // done | skipped | partial
	Reason   string             `json:"reason,omitempty"`
	Sessions map[string]int     `json:"sessions,omitempty"`
	Notes    retain.FlushResult `json:"notes"`
	Backfill string             `json:"backfill,omitempty"`
	Index    string             `json:"index,omitempty"` // entity index: rebuilt | failed: <why>
	Epoch    string             `json:"epoch,omitempty"` // restore epoch reconcile (decision 0044)
	Pruned   []string           `json:"briefings_pruned,omitempty"`
	Families string             `json:"families,omitempty"` // project families: recomputed | failed: <why>
	// Purges: the look for purged documents an older client wrote again
	// (backsight issue 5), "" when it found none.
	Purges string `json:"purges,omitempty"`
	// Binary: the self-check of this binary, made on its first tick
	// (verified | unsigned | failed; SPEC §11.1).
	Binary string `json:"binary,omitempty"`
	// Server: the start of a server whose private address came after
	// Docker started it (startLateServer), "" when none was needed.
	Server string `json:"server,omitempty"`
	// Feedback: what the settle of the feedback journal decided.
	Feedback feedback.SettleResult `json:"feedback"`
	MS       int                   `json:"ms"`
}

// reachCheckTimeout bounds the tick's one health call.
const reachCheckTimeout = 5 * time.Second

func newJobsTickCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "tick",
		Short: "Send settled sessions and queued notes (every 15 minutes)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			cfg, sec, err := loadServerConfig()
			if err != nil {
				return err
			}
			d, home, err := machine()
			if err != nil {
				return err
			}
			tw := telemetry.New(d.State, telemetry.Common{BinaryVersion: buildinfo.Version})
			res := tickResult{Status: "done"}
			finish := func(failed error) error {
				res.MS = int(time.Since(start).Milliseconds())
				tw.Emit(telemetry.Job{Name: "tick", Status: res.Status, MS: res.MS,
					Details: map[string]any{"sessions": res.Sessions, "notes_sent": res.Notes.Sent, "notes_kept": res.Notes.Kept, "reason": res.Reason, "epoch": res.Epoch, "purges": res.Purges, "server": res.Server}})
				return e.report("jobs tick", res, func() string { return tickText(res) }, failed)
			}

			l, err := state.TryAcquire(filepath.Join(d.LocksDir(), "job-tick.lock"), "jobs tick")
			var held *state.HeldError
			switch {
			case errors.As(err, &held) && held.Stale && state.BreakStale(held) == nil:
				if l, err = state.TryAcquire(filepath.Join(d.LocksDir(), "job-tick.lock"), "jobs tick"); err != nil {
					res.Status, res.Reason = "skipped", "another tick is running"
					return finish(nil)
				}
			case err != nil:
				res.Status, res.Reason = "skipped", "another tick is running"
				return finish(nil)
			}
			defer func() { _ = l.Release() }()
			if state.MaintenanceHeld(d.MaintenanceLock()) {
				res.Status, res.Reason = "skipped", "maintenance in progress"
				return finish(nil)
			}
			// After the maintenance check: it writes state.json, whose lock
			// update apply needs to record the new binary.
			res.Binary = ensureSelfCheck(d)
			res.Feedback = settleFeedback(d, cfg, tw) // local only: before the server start, which can wait
			// First: a server left down at boot makes every send below fail.
			sctx, scancel := context.WithTimeout(cmd.Context(), serverCheckTimeout)
			res.Server = startLateServer(sctx, d, cfg)
			scancel()

			f, err := newFlusher(d, home, cfg, sec, true)
			if err != nil {
				return err
			}
			// One look whether the server answers, for doctor's server-reach
			// check and the session banner (the hooks record only recalls).
			hctx, hcancel := context.WithTimeout(cmd.Context(), reachCheckTimeout)
			_, herr := f.Client.Health(hctx)
			hcancel()
			files := transcripts(home)
			if !cfg.Components.Write { // migration flag: another integration writes (SPEC §5 exclusivity)
				recordReach(d, cfg, herr)
				state.RecordWaiting(d, f.Unsent(files))
				res.Status, res.Reason = "skipped", "components.write is off"
				if cfg.Components.Read {
					res.Index = refreshIndex(cmd.Context(), d, cfg.Server.Bank, f.Client)
				}
				if otherServer(d, cfg) == nil { // nothing is written to another server
					res.Pruned = pruneBriefings(cmd.Context(), cfg.Server.Bank, briefingFor(cfg), f.Client)
				}
				pruneSessions(d)
				return finish(nil)
			}
			res.Epoch = reconcile(cmd.Context(), f)
			recordReach(d, cfg, herr) // after the reconcile: it records another server at the address
			rep, failed := reportFlush(flushAll(cmd, f, files, retain.Options{}, d))
			res.Sessions = rep.Counts
			// Every unsent session, from the transcripts: a flush that stops
			// at the first failure (server down, a hold) counts one.
			state.RecordWaiting(d, f.Unsent(files))
			tags, terr := f.Client.Tags(cmd.Context(), cfg.Server.Bank, "project:")
			if terr != nil {
				tags = nil // not read: no project is taken for forgotten
			}
			res.Families = refreshFamilies(d, cfg, f.Resolver, tags)
			if n, err := dropSettled(d); err == nil && n > 0 {
				res.Backfill = fmt.Sprintf("%d queued session(s) had nothing left to send", n)
			}
			if st, err := state.Load(d.StateFile()); err == nil && st.Backfill != nil && len(st.Backfill.Sessions) > 0 && st.Backfill.Paused == "" && f.Hold == "" &&
				(cfg.LLM.Provider != "claude-code" || st.Backfill.AllowSubscription) {
				prices := retain.PricesFor(cfg.LLM.Model, cfg.LLM.PriceInput, cfg.LLM.PriceCached, cfg.LLM.PriceOutput, cfg.LLM.Currency)
				if release, err := acquireBackfill(d); err != nil {
					res.Backfill = "skipped: another backfill is running"
				} else {
					br, berr := runBackfill(cmd.Context(), f, d, cfg, prices, true, nil)
					release()
					res.Backfill = fmt.Sprintf("%d/%d", br.Done, br.Total)
					if berr != nil {
						res.Backfill += " paused: " + br.Paused
					}
				}
			}
			if otherServer(d, cfg) != nil {
				// Not the server this client joined: nothing is written to
				// it, notes stay queued (decision 0073).
				res.Notes.Kept, _ = notesOutboxFor(d, cfg).Pending()
				failed = &output.Error{Code: output.Partial, Message: "nothing sent: a different server answers at the joined address; run backsight doctor"}
			} else {
				res.Notes, err = notesOutboxFor(d, cfg).Flush(cmd.Context(), f.Client, cfg.Server.Bank, f.MachineID)
				if err != nil || res.Notes.Kept > 0 {
					failed = &output.Error{Code: output.Partial, Message: "some notes stay queued; the next tick retries them"}
				}
				// After the sends: a slow server delays this look, never them.
				res.Purges = guardTick(cmd.Context(), f, cfg)
				res.Pruned = pruneBriefings(cmd.Context(), cfg.Server.Bank, briefingFor(cfg), f.Client)
			}
			res.Index = refreshIndex(cmd.Context(), d, cfg.Server.Bank, f.Client)
			pruneSessions(d)
			if failed != nil {
				res.Status = "partial"
			}
			return finish(failed)
		},
	}
}

// recordReach records the tick's health call (state.Reach). An answer
// while another server holds the joined address is not this PC's server
// answering: only a failure is recorded then.
func recordReach(d paths.Dirs, cfg config.Config, herr error) {
	if herr == nil && otherServer(d, cfg) != nil {
		return
	}
	state.RecordReach(d, herr == nil, now())
}

// ensureSelfCheck: the first tick of a binary hashes it against its signed
// checksums.txt and records the result; later ticks read the record. A
// failure is doctor's to report: the tick goes on.
func ensureSelfCheck(d paths.Dirs) string {
	exe, err := selfExe()
	if err != nil {
		return selfcheck.Failed
	}
	rec, _ := selfcheck.Ensure(d, exe)
	return rec.Status
}

func tickText(r tickResult) string {
	if r.Status == "skipped" {
		return "skipped: " + r.Reason + "\n"
	}
	msg := fmt.Sprintf("%s: sessions %v, notes sent %d, queued %d (%d ms)", r.Status, r.Sessions, r.Notes.Sent, r.Notes.Kept, r.MS)
	if r.Backfill != "" {
		msg += ", backfill " + r.Backfill
	}
	if r.Epoch != "" {
		msg += ", " + r.Epoch
	}
	if r.Purges != "" {
		msg += ", " + r.Purges
	}
	if r.Server != "" {
		msg += ", " + r.Server
	}
	return msg + "\n"
}

// sessionTTL: session state older than this is deleted (it holds prompt
// words and reply excerpts; a resumed session starts a fresh state).
const (
	sessionTTL = 30 * 24 * time.Hour
	pausedTTL  = 90 * 24 * time.Hour
)

func pruneSessions(d paths.Dirs) {
	(retain.Attributor{Dir: d.AttributionDir()}).PruneAttribution(now(), retain.AttributionKeep)
	session.PruneLive(d.SessionsDir(), 7*24*time.Hour, now())
	ents, err := os.ReadDir(d.SessionsDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && !e.IsDir() && now().Sub(fi.ModTime()) > sessionTTL {
			id := strings.TrimSuffix(e.Name(), ".json")
			// A pause, or a paused stretch not flushed yet, outlives the
			// pruning, for pausedTTL: a session resumed weeks later stays
			// paused; one never resumed does not hold its excerpts forever.
			if st, err := (session.Store{Dir: d.SessionsDir()}).Load(id); err != nil || ((st.Paused || len(st.PausedSpans) > 0) && now().Sub(fi.ModTime()) < pausedTTL) {
				continue
			}
			_ = os.Remove(filepath.Join(d.SessionsDir(), e.Name()))
		}
	}
}

// briefingFor is the briefing in force: none when the read side is off, so
// that its pages go.
func briefingFor(cfg config.Config) string {
	if !cfg.Components.Read {
		return "recent"
	}
	return cfg.Recall.Briefing
}

// pruneBriefings deletes backsight's briefing pages (decision 0060) of
// projects with no memory in briefingIdleDays, and all of them when the
// briefing is "recent": the server would keep refreshing them for nothing.
// Other mental models are never touched. Best effort.
func pruneBriefings(ctx context.Context, bank, briefing string, cl *hindsight.Client) []string {
	models, err := cl.MentalModels(ctx, bank)
	if err != nil {
		return nil
	}
	var pruned []string
	for _, m := range models {
		if !strings.HasPrefix(m.ID, briefingModelPrefix) {
			continue
		}
		idle := briefing != "model"
		if t, err := time.Parse(time.RFC3339Nano, m.LastRefreshedAt); !idle && (err != nil || now().Sub(t) < briefingIdleDays*24*time.Hour) {
			continue // new (first refresh pending) or refreshed lately: deleting would recreate it at the next session
		}
		if !idle {
			idle = true
			for _, t := range m.Tags {
				if mem, err := cl.Recent(ctx, bank, t, "", 1); err != nil || (len(mem) > 0 && now().Sub(mem[0].When()) < briefingIdleDays*24*time.Hour) {
					idle = false // active, or unknown: kept
				}
			}
		}
		if idle && cl.DeleteMentalModel(ctx, bank, m.ID) == nil {
			pruned = append(pruned, m.ID)
		}
	}
	return pruned
}

// refreshIndex rebuilds the entity index when it is older than a day; best
// effort, the hooks only read it.
func refreshIndex(ctx context.Context, d paths.Dirs, bank string, cl *hindsight.Client) string {
	if idx, err := recall.LoadIndex(d.EntitiesFile()); err == nil && now().Sub(idx.BuiltAt) < recall.IndexMaxAge {
		return ""
	}
	idx, err := recall.BuildIndex(ctx, cl, bank, now())
	if err == nil {
		err = idx.Save(d.EntitiesFile())
	}
	if err != nil {
		return "failed: " + err.Error()
	}
	return "rebuilt"
}
