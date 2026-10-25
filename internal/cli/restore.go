package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	agelib "filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
)

// epochTimeout bounds each call of the epoch write: a synchronous retain
// into the control bank (chunks: an embedding, no LLM call).
const epochTimeout = time.Minute

type restoreResult struct {
	Archive    string           `json:"archive"`
	Plan       bool             `json:"plan"`
	Manifest   backup.Manifest  `json:"manifest"`
	Safety     string           `json:"safety_archive"` // the server as it was, kept like any backup
	Steps      []step           `json:"steps"`
	Restored   map[string]int64 `json:"restored_counts,omitempty"`
	Epoch      *retain.Epoch    `json:"epoch,omitempty"`
	RolledBack bool             `json:"rolled_back,omitempty"`
	Warnings   []string         `json:"warnings,omitempty"`
	MS         int              `json:"ms"`
}

func newRestoreCmd(e *env) *cobra.Command {
	var identityFile string
	var plan bool
	cmd := &cobra.Command{
		Use:   "restore <file>",
		Short: "Restore a backup into the local server, after a safety backup, and start a new restore epoch",
		Long: "Restore takes the maintenance lock, backs the server up (the safety archive), stops Hindsight, replaces its database " +
			"with the archive's, starts it again and checks the row counts, health and a recall. It then writes a new restore epoch, " +
			"so every client re-reads what the server holds (decision 0044). Any failure after the stop restores the safety archive (exit 30).",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			res := restoreResult{Archive: args[0], Plan: plan, Steps: []step{}}
			add := func(name, status, detail string) {
				st := step{name, status, detail}
				res.Steps = append(res.Steps, st)
				if !e.json {
					fmt.Fprint(e.stdout, stepLine(st))
				}
			}
			err := runRestore(cmd.Context(), &res, identityFile, add)
			res.MS = int(time.Since(start).Milliseconds())
			if err != nil && len(res.Steps) == 0 {
				return err // refused before anything ran
			}
			return e.report("restore", res, func() string { return restoreSummary(res) }, err)
		},
	}
	cmd.Flags().StringVar(&identityFile, "identity", "", "age identity file or SSH private key (default: the identity in secrets.toml)")
	cmd.Flags().BoolVar(&plan, "plan", false, "show the steps, the archive's manifest and the safety archive without acting")
	return cmd
}

func runRestore(ctx context.Context, res *restoreResult, identityFile string, add func(name, status, detail string)) error {
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return err
	}
	if err := requireLocal(cfg); err != nil {
		return err
	}
	d, _, err := machine()
	if err != nil {
		return err
	}
	var ids []agelib.Identity
	if identityFile != "" {
		if ids, err = readIdentities(identityFile); err != nil {
			return &output.Error{Code: output.Usage, Message: err.Error()}
		}
	} else if _, _, ids, err = backupKeys(d, cfg, sec, false); err != nil {
		return err
	}

	// 1. The archive, decrypted under the state directory (0700) and
	// checked against this install before anything changes.
	work, done, err := backup.VerifyDir(filepath.Join(d.State, "staging"))
	if err != nil {
		return blocked(err)
	}
	defer done()
	m, err := backup.Open(res.Archive, ids, work, func(string) bool { return false })
	var noMatch *agelib.NoIdentityMatchError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &output.Error{Code: output.Usage, Message: err.Error()}
	case errors.As(err, &noMatch):
		return &output.Error{Code: output.Usage, Message: err.Error() + "; or give the key that encrypted it with --identity"}
	case err != nil:
		return blocked(err)
	}
	res.Manifest = m
	if err := restorable(m, cfg); err != nil {
		return err
	}
	dir := cfg.Backup.Dir
	if dir == "" {
		dir = filepath.Join(d.Data, "backups")
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second)
	if err != nil {
		return blocked(err)
	}
	if res.Plan {
		res.Safety = filepath.Join(dir, backup.Name(now()))
		epoch := "the next restore epoch"
		if before, err := retain.ReadEpoch(ctx, cl, cfg.Server.Bank); err == nil {
			epoch = fmt.Sprintf("restore epoch %d or later (the server is at %d)", before.Epoch+1, before.Epoch)
		}
		for _, s := range [][2]string{
			{"check", fmt.Sprintf("archive of %s, Hindsight %s, bank %s, counts %v", m.CreatedAt.Format(time.RFC3339), m.HindsightImage, m.Bank, m.Counts)},
			{"lock", "would take the maintenance lock until the end (hooks queue, jobs skip)"},
			{"safety backup", "would write " + res.Safety},
			{"stop", "would stop hindsight (the database keeps running)"},
			{"restore", "would replace the database with the archive's"},
			{"counts", "would compare the rows with the manifest's"},
			{"start", "would start the server (an older schema migrates on start)"},
			{"health", "would wait for health"},
			{"recall", "would run a recall"},
			{"epoch", "would write " + epoch},
		} {
			add(s[0], "planned", s[1])
		}
		return nil
	}
	add("check", "done", fmt.Sprintf("archive of %s, Hindsight %s, counts %v", m.CreatedAt.Format(time.RFC3339), m.HindsightImage, m.Counts))

	// 2. Lock, held to the end: hooks only queue, jobs skip.
	l, note, err := acquireMaintenance(d, "restore")
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()
	if note != "" {
		res.Warnings = append(res.Warnings, note)
	}
	add("lock", "done", d.MaintenanceLock())

	// The epoch the clients hold now: the next one must differ from it,
	// even when the archive holds an older one.
	before, err := retain.ReadEpoch(ctx, cl, cfg.Server.Bank)
	if err != nil {
		res.Warnings = append(res.Warnings, "the current restore epoch could not be read ("+err.Error()+"): the next one follows the archive's")
	}

	// 3. The safety archive, its clear dump kept to roll back to.
	safetyDump := filepath.Join(work, "safety.dump")
	sb, err := createBackup(ctx, &safetyCopy{dump: safetyDump})
	if err != nil {
		return &output.Error{Code: output.Blocked, Message: server.Redact("the safety backup failed, nothing was changed: "+err.Error(), sec)}
	}
	res.Safety = sb.Path
	add("safety backup", "done", fmt.Sprintf("%s (%v)", sb.Path, sb.Counts))

	// 4. From the stop on, any failure restores the safety archive.
	c := composeFor(cfg)
	db := server.DB{Compose: c}
	failed := func(step string, err error) error {
		add(step, "failed", server.Redact(err.Error(), sec))
		return rollback(context.WithoutCancel(ctx), res, cfg, sec, db, safetyDump, sb.Counts, err, add)
	}
	if err := c.Stop(ctx, "hindsight"); err != nil {
		// The database is untouched: whatever did stop starts again.
		add("stop", "failed", server.Redact(err.Error(), sec))
		_ = c.Up(context.WithoutCancel(ctx))
		return &output.Error{Code: output.Blocked, Message: server.Redact("the server could not be stopped, nothing was restored: "+err.Error(), sec)}
	}
	add("stop", "done", "hindsight stopped, the database kept running")
	if err := db.Restore(ctx, filepath.Join(work, "db.dump")); err != nil {
		return failed("restore", err)
	}
	add("restore", "done", "database replaced")
	// Counted before the start: a queued retain the server picks up once
	// running must not look like a bad restore.
	counts, err := db.Counts(ctx, backup.Tables)
	if err != nil {
		return failed("counts", err)
	}
	res.Restored = counts
	if !sameCounts(counts, m.Counts) {
		return failed("counts", fmt.Errorf("restored counts %v differ from the manifest's %v", counts, m.Counts))
	}
	add("counts", "done", fmt.Sprintf("%v, as in the manifest", counts))
	if err := startAndCheck(ctx, cfg, sec, c, cl, add); err != nil {
		return failed("verify", err)
	}

	// 5. The new epoch, last: only a restore that passed tells the clients.
	restored, err := retain.ReadEpoch(ctx, cl, cfg.Server.Bank)
	if err != nil {
		return failed("epoch", err)
	}
	next := retain.Epoch{Epoch: max(before.Epoch, restored.Epoch) + 1, RestoredAt: now().UTC().Truncate(time.Second), BackupCreatedAt: m.CreatedAt.UTC().Truncate(time.Second)}
	wcl, err := hindsight.New(cfg.Server.URL, sec.APIKey, epochTimeout)
	if err == nil {
		err = retain.WriteEpoch(ctx, wcl, cfg.Server.Bank, next)
	}
	if err != nil {
		return failed("epoch", err)
	}
	res.Epoch = &next
	add("epoch", "done", fmt.Sprintf("restore epoch %d", next.Epoch))
	return nil
}

// restorable refuses, before anything stops, an archive this install
// cannot run: one from a newer Hindsight than the pinned one (its schema is
// ahead), one it cannot place, or one of another bank.
func restorable(m backup.Manifest, cfg config.Config) error {
	pinned := server.ImageFor(cfg)
	cmp, ok := server.CompareImages(m.HindsightImage, pinned)
	switch {
	case !ok:
		_, digest, _ := strings.Cut(m.HindsightImage, "@")
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("cannot tell whether the archive's Hindsight %q is older than this install's %q: "+
			"restore it with its own image (server.digest = %q in config.toml, then backsight server start)", m.HindsightImage, pinned, digest)}
	case cmp > 0:
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("the archive was taken with Hindsight %s, newer than this install's %s: update backsight first", m.HindsightImage, pinned)}
	case m.Bank != cfg.Server.Bank:
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("the archive holds bank %q, this install uses %q (server.bank)", m.Bank, cfg.Server.Bank)}
	}
	return nil
}

// startAndCheck starts the server and checks it answers: health, then a
// recall (embeddings and database read back).
func startAndCheck(ctx context.Context, cfg config.Config, sec config.Secrets, c server.Compose, cl *hindsight.Client, add func(name, status, detail string)) error {
	if err := c.Up(ctx); err != nil {
		return err
	}
	add("start", "done", "project "+cfg.Server.Project)
	if _, err := waitHealthy(ctx, cfg, sec); err != nil {
		return err
	}
	add("health", "done", "healthy")
	if _, err := cl.Recall(ctx, cfg.Server.Bank, hindsight.RecallRequest{Query: "backsight restore check", Budget: "low", MaxTokens: 512}); err != nil {
		return fmt.Errorf("recall: %w", err)
	}
	add("recall", "done", "answered")
	return nil
}

// rollback puts the safety archive's dump back and starts the server: exit
// 30 when it is running again as it was, 20 when even that failed (the
// safety archive is then the way back, by hand).
func rollback(ctx context.Context, res *restoreResult, cfg config.Config, sec config.Secrets, db server.DB, dump string, want map[string]int64, cause error,
	add func(name, status, detail string)) error {
	cause = redacted(cause, sec)
	err := db.Compose.Stop(ctx, "hindsight")
	if err == nil {
		err = db.Restore(ctx, dump)
	}
	if err == nil {
		var got map[string]int64
		if got, err = db.Counts(ctx, backup.Tables); err == nil && !sameCounts(got, want) {
			err = fmt.Errorf("counts %v differ from the safety archive's %v", got, want)
		}
	}
	// Health only: the safety archive's data was serving before, and a
	// recall that fails for another reason would fail here too.
	if err == nil {
		err = db.Compose.Up(ctx)
	}
	if err == nil {
		_, err = waitHealthy(ctx, cfg, sec)
	}
	if err != nil {
		add("rollback", "failed", server.Redact(err.Error(), sec))
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("restore failed (%v) and so did the rollback (%v): restore the safety archive by hand with backsight restore %s",
			cause, server.Redact(err.Error(), sec), res.Safety)}
	}
	res.RolledBack = true
	add("rollback", "done", "the server runs the safety archive's data again")
	return &output.Error{Code: output.RolledBack, Message: fmt.Sprintf("restore failed and was rolled back to the safety archive %s: %v", res.Safety, cause)}
}

// sameCounts compares the rows of the tables a backup counts.
func sameCounts(a, b map[string]int64) bool {
	for _, t := range backup.Tables {
		if a[t] != b[t] {
			return false
		}
	}
	return true
}

func restoreSummary(r restoreResult) string {
	var b strings.Builder
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning  %s\n", w)
	}
	switch {
	case r.Plan:
		fmt.Fprintf(&b, "\nplan only: nothing was changed. The safety archive would be %s\n", r.Safety)
	case r.Epoch != nil:
		fmt.Fprintf(&b, "\nrestored %s (%v); restore epoch %d: clients re-read the server. The server as it was is in %s\n",
			r.Archive, r.Restored, r.Epoch.Epoch, r.Safety)
	}
	return b.String()
}
