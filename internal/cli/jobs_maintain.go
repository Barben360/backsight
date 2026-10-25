package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/telemetry"
)

// telemetryMonths: month files kept by the rotation, a year of stats and
// the month in progress.
const telemetryMonths = 13

type maintainResult struct {
	Status  string   `json:"status"` // done | skipped
	Reason  string   `json:"reason,omitempty"`
	Rotated []string `json:"telemetry_rotated,omitempty"`
	Doctor  string   `json:"doctor,omitempty"` // snapshot: N error(s), M warning(s)
	MS      int      `json:"ms"`
}

// newJobsMaintainCmd is the daily job (decision 0025): telemetry rotation
// and a doctor snapshot; backups join it with M4 block 2, the update check
// once releases are public (0063 Q2).
func newJobsMaintainCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "maintain",
		Short: "Daily upkeep: telemetry rotation, doctor snapshot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			d, home, err := machine()
			if err != nil {
				return err
			}
			tw := telemetry.New(d.State, telemetry.Common{BinaryVersion: buildinfo.Version})
			res := maintainResult{Status: "done"}
			finish := func() error {
				res.MS = int(time.Since(start).Milliseconds())
				tw.Emit(telemetry.Job{Name: "maintain", Status: res.Status, MS: res.MS,
					Details: map[string]any{"rotated": len(res.Rotated), "doctor": res.Doctor, "reason": res.Reason}})
				return e.emit("jobs maintain", res, func() string { return maintainText(res) })
			}
			l, err := state.TryAcquire(filepath.Join(d.LocksDir(), "job-maintain.lock"), "jobs maintain")
			var held *state.HeldError
			switch {
			case errors.As(err, &held) && held.Stale && state.BreakStale(held) == nil:
				if l, err = state.TryAcquire(filepath.Join(d.LocksDir(), "job-maintain.lock"), "jobs maintain"); err != nil {
					res.Status, res.Reason = "skipped", "another maintain is running"
					return finish()
				}
			case err != nil:
				res.Status, res.Reason = "skipped", "another maintain is running"
				return finish()
			}
			defer func() { _ = l.Release() }()
			if _, err := os.Stat(d.MaintenanceLock()); err == nil {
				res.Status, res.Reason = "skipped", "maintenance in progress"
				return finish()
			}
			res.Rotated = rotateTelemetry(d.TelemetryDir(), now(), telemetryMonths)
			rep := runDoctor(cmd, d, home)
			res.Doctor = fmt.Sprintf("%d error(s), %d warning(s)", rep.Errors, rep.Warnings)
			if raw, err := json.MarshalIndent(rep, "", "  "); err == nil {
				_ = fsutil.WriteAtomic(filepath.Join(d.State, "doctor-snapshot.json"), raw, 0o600)
			}
			return finish()
		},
	}
}

// rotateTelemetry removes month files older than keep months (the current
// month counts as one).
func rotateTelemetry(dir string, at time.Time, keep int) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	oldest := time.Date(at.UTC().Year(), at.UTC().Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -(keep - 1), 0).Format("2006-01")
	var removed []string
	for _, e := range ents {
		month, ok := strings.CutPrefix(strings.TrimSuffix(e.Name(), ".jsonl"), "events-")
		if !ok || e.IsDir() || len(month) != 7 || month >= oldest {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed
}

func maintainText(r maintainResult) string {
	if r.Status == "skipped" {
		return "skipped: " + r.Reason + "\n"
	}
	return fmt.Sprintf("done: %d telemetry file(s) rotated, doctor %s (%d ms)\n", len(r.Rotated), r.Doctor, r.MS)
}
