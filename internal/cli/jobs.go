package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

func newJobsCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "jobs", Short: "Periodic jobs run by the OS scheduler", Args: cobra.NoArgs}
	cmd.AddCommand(newJobsTickCmd(e))
	return cmd
}

type tickResult struct {
	Status   string             `json:"status"` // done | skipped | partial
	Reason   string             `json:"reason,omitempty"`
	Sessions map[string]int     `json:"sessions,omitempty"`
	Notes    retain.FlushResult `json:"notes"`
	MS       int                `json:"ms"`
}

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
					Details: map[string]any{"sessions": res.Sessions, "notes_sent": res.Notes.Sent, "notes_kept": res.Notes.Kept, "reason": res.Reason}})
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
			if _, err := os.Stat(d.MaintenanceLock()); err == nil {
				res.Status, res.Reason = "skipped", "maintenance in progress"
				return finish(nil)
			}

			f, err := newFlusher(d, home, cfg, sec)
			if err != nil {
				return err
			}
			rep, failed := reportFlush(flushAll(cmd, f, transcripts(home), retain.Options{}, d))
			res.Sessions = rep.Counts
			res.Notes, err = notesOutbox(d).Flush(cmd.Context(), f.Client, cfg.Server.Bank, f.MachineID)
			if err != nil || res.Notes.Kept > 0 {
				failed = &output.Error{Code: output.Partial, Message: "some notes stay queued; the next tick retries them"}
			}
			if failed != nil {
				res.Status = "partial"
			}
			return finish(failed)
		},
	}
}

func tickText(r tickResult) string {
	if r.Status == "skipped" {
		return "skipped: " + r.Reason + "\n"
	}
	return fmt.Sprintf("%s: sessions %v, notes sent %d, queued %d (%d ms)\n", r.Status, r.Sessions, r.Notes.Sent, r.Notes.Kept, r.MS)
}
