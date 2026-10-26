package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/retain"
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
	Backup  string   `json:"backup,omitempty"` // the archive written, or why none
	Update  string   `json:"update,omitempty"` // up to date | <v> available | failed: why
	// Reattribute: the free moves made after the backup, what is left to
	// send again, or why it did not run.
	Reattribute string `json:"reattribute,omitempty"`
	MS          int    `json:"ms"`
}

// newJobsMaintainCmd is the daily job (decision 0025): telemetry rotation,
// a backup (when backup.enabled), the update check (and its apply with
// update.auto_apply) and a doctor snapshot.
// A failed update check (offline, rate limited) is recorded for doctor and
// never fails the job.
func newJobsMaintainCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "maintain",
		Short: "Daily upkeep: telemetry rotation, backup, update check, doctor snapshot",
		Args:  usageArgs(cobra.NoArgs),
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
					Details: map[string]any{"rotated": len(res.Rotated), "doctor": res.Doctor, "backup": res.Backup, "reattribute": res.Reattribute, "update": res.Update, "reason": res.Reason}})
				var failed error
				if res.Status == "partial" {
					failed = &output.Error{Code: output.Partial, Message: "the backup failed: " + res.Backup + "; the next run retries"}
				}
				return e.report("jobs maintain", res, func() string { return maintainText(res) }, failed)
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
			if state.MaintenanceHeld(d.MaintenanceLock()) {
				res.Status, res.Reason = "skipped", "maintenance in progress"
				return finish()
			}
			res.Rotated = rotateTelemetry(d.TelemetryDir(), now(), telemetryMonths)
			archive, noBackup := "", "backup.enabled is off, and the moves wait for a backup"
			if cfg, _, err := loadServerConfig(); err == nil && cfg.Backup.Enabled {
				// The maintenance lock above is checked, not held: backup
				// create takes it for the dump itself.
				if b, err := createBackup(cmd.Context(), nil); err != nil {
					res.Status, res.Backup = "partial", "failed: "+err.Error()
					noBackup = "today's backup failed"
				} else {
					res.Backup = b.Path
					archive = b.Path
					if b.Skipped != "" {
						res.Backup, archive = "skipped: "+b.Skipped, ""
						noBackup = "the server is remote, so no backup is taken here: run backsight retain reattribute after backing it up on its host"
					}
				}
			}
			var moved int
			res.Reattribute, moved = maintainReattribute(cmd.Context(), d, archive, noBackup)
			res.Update = maintainUpdate(cmd.Context(), d)
			// This run counts as maintain running: the snapshot must not
			// find maintain late after a weekend off.
			tw.Emit(telemetry.Job{Name: "maintain", Status: "started"})
			rep := runDoctor(cmd, d, home)
			// Not on a day the server consolidates moved memories again: the
			// reading would be taken mid-way, and serve as next week's base.
			if moved == 0 && serverSettled(cmd.Context()) {
				recordNoise(d, rep)
			}
			res.Doctor = fmt.Sprintf("%d error(s), %d warning(s)", rep.Errors, rep.Warnings)
			snapshot := map[string]any{"generated_at": now().UTC(), "binary_version": buildinfo.Version, "report": rep}
			if raw, err := json.MarshalIndent(snapshot, "", "  "); err == nil {
				_ = fsutil.WriteAtomic(filepath.Join(d.State, "doctor-snapshot.json"), raw, 0o600)
			}
			return finish()
		},
	}
}

// maintainUpdate runs the update check against the configured source and
// records it; with update.auto_apply (off by default, SPEC §11.7) it then
// applies the newest release, or resumes an unfinished update to this
// binary. Its outcome is one line for the job's report; a failed or rolled
// back apply never fails the job (doctor keeps showing the update). A
// release whose apply failed past its install is recorded and skipped
// until a newer one appears or the user runs update apply: each try costs
// a safety backup and a rollback.
func maintainUpdate(ctx context.Context, d paths.Dirs) string {
	_, cfg, err := updateConfig()
	if err != nil {
		return "failed: " + err.Error()
	}
	if m, err := state.ReadUpdateMarker(d); cfg.Update.AutoApply && err == nil && m != nil && m.To == buildinfo.Version {
		return autoApply(ctx, d, m.To)
	}
	cctx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	res, err := checkUpdates(cctx, releaseSource(cfg, ""), buildinfo.Version, cfg.Update.Prereleases)
	cancel()
	recordUpdateCheck(d, res, err)
	switch {
	case err != nil:
		return "failed: " + err.Error()
	case !res.UpdateAvailable:
		return "up to date"
	case !cfg.Update.AutoApply:
		return res.Latest + " available"
	}
	if st, err := state.Load(d.StateFile()); err == nil && st.AutoApply != nil && !release.NewerThan(res.Latest, st.AutoApply.Failed) {
		f := st.AutoApply
		return fmt.Sprintf("%s available, auto-apply skipped: %s failed on %s (exit %d); run backsight update apply", res.Latest, f.Failed, f.At.UTC().Format("2006-01-02"), f.Code)
	}
	return autoApply(ctx, d, res.Latest)
}

// autoApply runs update apply for jobs maintain and records a failure
// past the install (a resumable exit 40 is not one).
func autoApply(ctx context.Context, d paths.Dirs, latest string) string {
	ar := updateApplyResult{Installed: buildinfo.Version, Steps: []step{}, Migrations: []string{}}
	err := runUpdateApply(ctx, &ar, updateApplyFlags{auto: true}, func(string, string, string) {})
	code := output.CodeOf(err)
	switch {
	case err == nil && ar.UpToDate:
		return "up to date"
	case err == nil:
		if st, lerr := state.Load(d.StateFile()); lerr == nil && st.AutoApply != nil {
			_ = state.UpdateWithin(d, "jobs maintain", recordWait, func(s *state.State) error { s.AutoApply = nil; return nil })
		}
		return "applied " + ar.Target
	case ar.changed && code != output.Partial:
		_ = state.UpdateWithin(d, "jobs maintain", recordWait, func(s *state.State) error {
			s.AutoApply = &state.AutoApply{Failed: ar.Target, Code: code, Error: err.Error(), At: now().UTC().Truncate(time.Second)}
			return nil
		})
	}
	return latest + " available, auto-apply failed (exit " + fmt.Sprint(code) + "): " + err.Error()
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
	return fmt.Sprintf("%s: %d telemetry file(s) rotated, backup %s, re-attribution %s, update %s, doctor %s (%d ms)\n", r.Status, len(r.Rotated),
		map[bool]string{true: "off", false: r.Backup}[r.Backup == ""], r.Reattribute, r.Update, r.Doctor, r.MS)
}

// maintainReattribute applies the moves in place of retain reattribute
// after the daily backup, once that backup is verified, and never sends
// anything again (that costs extraction: the user answers it, doctor
// shows it). archive is today's backup, "" when there is none (noBackup
// says why). When it does not run, the reason is recorded for doctor.
// moved is how many documents it moved.
func maintainReattribute(ctx context.Context, d paths.Dirs, archive, noBackup string) (string, int) {
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return "failed: " + err.Error(), 0
	}
	skip := func(why string) (string, int) {
		st, _, _ := retain.LoadReattributionStatus(d.State)
		st.Skipped, st.SkippedAt = why, now().UTC()
		_ = retain.SaveReattributionStatus(d.State, st)
		return "skipped: " + why, 0
	}
	switch {
	case !cfg.Components.Write:
		return skip(writeOffReason)
	case cfg.LLM.Provider == "claude-code":
		return skip("the claude-code provider would run the server's work after the moves on your subscription: run backsight retain reattribute --allow-subscription")
	case archive == "":
		return skip(noBackup)
	}
	_, _, ids, err := backupKeys(d, cfg, sec, false)
	if err == nil {
		var v backupVerifyResult
		if v, err = verifyArchive(ctx, d, cfg, ids, archive); err == nil && !v.OK {
			err = fmt.Errorf("its counts differ from its manifest")
		}
	}
	if err != nil {
		return skip("today's backup did not verify (" + err.Error() + ")")
	}
	r, err := runReattribute(ctx, reattributeFlags{}, reattributeOptions{backedUp: true, freeOnly: true})
	if err != nil {
		return "failed: " + err.Error(), len(r.Retagged)
	}
	out := fmt.Sprintf("%d moved", len(r.Retagged))
	if r.Deferred > 0 {
		out += fmt.Sprintf(" (%d more on the next days)", r.Deferred)
	}
	if n := len(r.Plan.Resends); n > 0 {
		out += fmt.Sprintf(", %d session(s) to send again (%s): backsight retain reattribute --plan", n, estimateText(r.Plan.Cost))
	}
	return out, len(r.Retagged)
}

// writeOffReason: another integration writes; nothing of backsight's to
// keep in line.
const writeOffReason = "components.write is off"

// serverSettled: the server has no operation queued (a consolidation
// running would move the noise reading).
func serverSettled(ctx context.Context) bool {
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return false
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 10*time.Second)
	if err != nil {
		return false
	}
	n, err := cl.QueuedOperations(ctx, cfg.Server.Bank)
	return err == nil && n == 0
}

// recordNoise keeps today's reading of the transverse pool's noise from
// the doctor snapshot, so doctor can tell when it grows.
func recordNoise(d paths.Dirs, rep doctor.Report) {
	for _, c := range rep.Checks {
		if c.ID == "noise-transverse" && c.Measure != nil && c.Measure["total"] > 0 {
			_ = retain.RecordNoise(d.State, retain.NoisePoint{At: now().UTC(), Named: int(c.Measure["named"]), Total: int(c.Measure["total"])})
		}
	}
}
