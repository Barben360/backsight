package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// epochTimeout bounds each call of the epoch write: a synchronous retain
// into the control bank (chunks: an embedding, no LLM call).
const epochTimeout = time.Minute

// rollbackStepTimeout bounds each step of a rollback (the health wait has
// its own, shorter bound): a hung docker call must not hold the rollback,
// and the marker, forever. Tests shorten it.
var rollbackStepTimeout = 20 * time.Minute

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

type restoreFlags struct {
	identity                 string
	plan, noSafety, nonInter bool
}

func newRestoreCmd(e *env) *cobra.Command {
	var fl restoreFlags
	cmd := &cobra.Command{
		Use:   "restore <file>",
		Short: "Restore a backup into the local server, after a safety backup, and start a new restore epoch",
		Long: "Restore takes the maintenance lock and server.lock, backs the server up (the safety archive), marks a restore in progress " +
			"(every client holds its sends), stops Hindsight, replaces its database with the archive's, starts it again on a temporary API key " +
			"only restore knows and checks the row counts, health and a recall. It then writes a new restore epoch, so every client re-reads " +
			"what the server holds (decision 0044), and starts the server again on its API key. Any failure after the stop restores the " +
			"safety archive and writes an epoch too (exit 30); a rollback that fails exits 20 and doctor names the way back. " +
			"--no-safety skips the safety backup: to restore a safety archive by hand.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			res := restoreResult{Archive: args[0], Plan: fl.plan, Steps: []step{}}
			add := func(name, status, detail string) {
				st := step{name, status, detail}
				res.Steps = append(res.Steps, st)
				if !e.json {
					fmt.Fprint(e.stdout, stepLine(st))
					return
				}
				// Progress lines on stderr, as setup streams them (decision 0064).
				b, _ := json.Marshal(map[string]any{"event": "progress", "command": "restore", "step": name, "status": status, "detail": detail})
				fmt.Fprintln(e.stderr, string(b))
			}
			err := runRestore(cmd.Context(), e, &res, fl, add)
			res.MS = int(time.Since(start).Milliseconds())
			if err != nil && len(res.Steps) == 0 {
				return err // refused before anything ran
			}
			return e.report("restore", res, func() string { return restoreSummary(res) }, err)
		},
	}
	cmd.Flags().StringVar(&fl.identity, "identity", "", "age identity file or SSH private key (default: the identity in secrets.toml)")
	cmd.Flags().BoolVar(&fl.plan, "plan", false, "show the steps, the archive's manifest and the safety archive without acting")
	cmd.Flags().BoolVar(&fl.noSafety, "no-safety", false, "make no safety backup first (restoring a safety archive by hand)")
	cmd.Flags().BoolVar(&fl.nonInter, "non-interactive", false, "never prompt (a passphrase-protected SSH key then exits 10)")
	return cmd
}

// refuseWhileBusy refuses a command that starts or changes the server
// while maintenance runs (a live owner) or a restore did not finish.
func refuseWhileBusy(d paths.Dirs) error {
	if held := state.Inspect(d.MaintenanceLock()); held != nil && !held.Stale {
		return &output.Error{Code: output.Blocked, Message: "maintenance in progress: " + held.Error()}
	}
	if state.RestoreIncomplete(d) {
		return &output.Error{Code: output.Blocked, Message: "a restore did not finish (" + d.RestoreMarker() + "): run backsight doctor for the way back"}
	}
	return nil
}

func runRestore(ctx context.Context, e *env, res *restoreResult, fl restoreFlags, add func(name, status, detail string)) error {
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
	if fl.identity != "" {
		rerun := "backsight restore " + res.Archive + " --identity " + fl.identity
		if ids, err = identitiesFrom(e, fl.identity, !fl.nonInter && !e.json, rerun); err != nil {
			return err
		}
	} else if _, _, ids, err = backupKeys(d, cfg, sec, false); err != nil {
		return err
	}

	// 1. The archive, decrypted under the state directory (0700) and
	// checked against this install before anything changes. VerifyDir
	// first removes what a killed restore or verify left there.
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
	// A live maintenance refuses, the plan too. A restore that did not
	// finish does not: restoring again (its safety archive, or the
	// archive) is the way back, and this run replaces its marker.
	if held := state.Inspect(d.MaintenanceLock()); held != nil && !held.Stale {
		return &output.Error{Code: output.Blocked, Message: "maintenance in progress: " + held.Error()}
	}
	if prev, _ := state.ReadRestoreMarker(d); prev != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("a restore of %s did not finish (started %s); this one replaces it", prev.Archive, prev.StartedAt.Format(time.RFC3339)))
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
		if fl.noSafety {
			res.Safety = ""
		}
		epoch := "the next restore epoch"
		if before, err := retain.ReadEpoch(ctx, cl, cfg.Server.Bank); err == nil {
			epoch = fmt.Sprintf("restore epoch %d or later (the server is at %d)", before.Epoch+1, before.Epoch)
		}
		safety := "would write " + res.Safety
		if fl.noSafety {
			safety = "none (--no-safety)"
		}
		for _, s := range [][2]string{
			{"check", fmt.Sprintf("archive of %s, Hindsight %s, bank %s, counts %v", m.CreatedAt.Format(time.RFC3339), m.HindsightImage, m.Bank, m.Counts)},
			{"lock", "would take the maintenance lock and server.lock until the end (hooks queue, jobs skip)"},
			{"safety backup", safety},
			{"stop", "would mark a restore in progress (every client holds its sends) and stop hindsight (the database keeps running)"},
			{"restore", "would replace the database with the archive's"},
			{"counts", "would compare the rows with the manifest's"},
			{"start", "would start the server on a temporary API key (an older schema migrates on start)"},
			{"health", "would wait for health"},
			{"recall", "would run a recall"},
			{"epoch", "would write " + epoch},
			{"key", "would start the server again on its API key"},
		} {
			add(s[0], "planned", s[1])
		}
		return nil
	}
	add("check", "done", fmt.Sprintf("archive of %s, Hindsight %s, counts %v", m.CreatedAt.Format(time.RFC3339), m.HindsightImage, m.Counts))

	// 2. Locks, held to the end: hooks only queue, jobs skip, and no other
	// command starts or rewrites the server.
	l, note, err := acquireMaintenance(d, "restore")
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()
	if note != "" {
		res.Warnings = append(res.Warnings, note)
	}
	sl, _, err := state.Acquire(filepath.Join(d.LocksDir(), "server.lock"), "restore")
	if err != nil {
		return &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	defer func() { _ = sl.Release() }()
	add("lock", "done", d.MaintenanceLock()+", server.lock")

	c := composeFor(cfg)
	if r, ok := c.Runner.(server.DockerRunner); ok {
		r.OwnGroup = true // a Ctrl-C must not kill the rollback's docker
		c.Runner = r
	}
	db := server.DB{Compose: c}
	_ = db.RemoveStaleDumps(ctx) // a killed backup's or restore's clear dump

	// The epochs known now: the server's (through the API, else read only
	// from its database) and this client's. The next one is above both and
	// above the archive's; without the server's, nothing is changed.
	before, how, err := epochBefore(ctx, cl, db, cfg.Server.Bank)
	if err != nil {
		return &output.Error{Code: output.Blocked, Message: server.Redact("cannot read the server's restore epoch ("+err.Error()+"): nothing was changed; check backsight server status", sec)}
	}
	if how != "" {
		res.Warnings = append(res.Warnings, how)
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		return &output.Error{Code: output.Blocked, Message: "cannot read this client's restore epoch (" + err.Error() + "): nothing was changed"}
	}
	rb := &rollback{res: res, cfg: cfg, sec: sec, c: c, db: db, d: d, highest: max(before.Epoch, st.EpochOf(cfg.Server.Bank)), history: before.History,
		safetyAt: now().UTC().Truncate(time.Second), add: add}

	// 3. The safety archive, its clear dump kept to roll back to.
	marker := state.RestoreMarker{Command: "restore", Safety: res.Archive, Archive: res.Archive, StartedAt: now().UTC().Truncate(time.Second)}
	if fl.noSafety {
		add("safety backup", "skipped", "--no-safety: the way back is this archive")
	} else {
		rb.dump = filepath.Join(work, "safety.dump")
		sb, err := createBackup(ctx, &safetyCopy{dump: rb.dump})
		if err != nil {
			return &output.Error{Code: output.Blocked, Message: server.Redact("the safety backup failed, nothing was changed: "+err.Error(), sec)}
		}
		res.Safety, rb.want, marker.Safety = sb.Path, sb.Counts, sb.Path
		res.Warnings = append(res.Warnings, sb.Warnings...)
		add("safety backup", "done", fmt.Sprintf("%s (%v)", sb.Path, sb.Counts))
	}

	// 4. A restore in progress from here: every client holds until the
	// epoch is written or the rollback finished.
	if err := state.WriteRestoreMarker(d, marker); err != nil {
		return &output.Error{Code: output.Blocked, Message: "cannot mark the restore in progress (" + err.Error() + "): nothing was changed"}
	}
	failed := func(step string, err error) error {
		add(step, "failed", server.Redact(err.Error(), sec))
		return rb.run(ctx, err)
	}
	if err := c.Stop(ctx, "hindsight"); err != nil {
		// The database is untouched: whatever did stop starts again.
		add("stop", "failed", server.Redact(err.Error(), sec))
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackStepTimeout)
		_ = c.Up(uctx)
		cancel()
		_ = state.ClearRestoreMarker(d)
		return &output.Error{Code: output.Blocked, Message: server.Redact("the server could not be stopped, nothing was restored: "+err.Error(), sec)}
	}
	add("stop", "done", "hindsight stopped, the database kept running")
	dropped, err := db.Restore(ctx, filepath.Join(work, "db.dump"))
	rb.dropped = dropped
	if err != nil {
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

	// 5. Started on a key only this run knows: no client sends before the
	// epoch exists (a refused key stops a client's run until later).
	tmp, tcl, err := startOnTemporaryKey(ctx, cfg, sec, c)
	if err != nil {
		return failed("verify", err)
	}
	add("start", "done", "project "+cfg.Server.Project+", on a temporary API key")
	add("health", "done", "healthy")
	if _, err := tcl.Recall(ctx, cfg.Server.Bank, hindsight.RecallRequest{Query: "backsight restore check", Budget: "low", MaxTokens: 512}); err != nil {
		return failed("verify", fmt.Errorf("recall: %w", err))
	}
	add("recall", "done", "answered")
	rb.tempKey = tmp

	// 6. The new epoch, last: only a restore that passed tells the clients.
	restored, err := retain.ReadEpoch(ctx, tcl, cfg.Server.Bank)
	if err != nil {
		return failed("epoch", err)
	}
	rb.highest = max(rb.highest, restored.Epoch)
	// The history goes on from the server's as it was: the restored
	// document is the archive's, older than the restores since.
	next := retain.Epoch{Epoch: rb.highest + 1, RestoredAt: now().UTC().Truncate(time.Second), BackupCreatedAt: m.CreatedAt.UTC().Truncate(time.Second),
		History: before.History}
	rb.highest = next.Epoch // attempted: the rollback's goes above it
	if err := writeEpochOn(ctx, cfg, tmp, next); err != nil {
		return failed("epoch", err)
	}
	res.Epoch = &next
	add("epoch", "done", fmt.Sprintf("restore epoch %d", next.Epoch))
	_ = state.ClearRestoreMarker(d)

	// 7. The server on its own key again.
	if err := startOnKey(ctx, cfg, sec, c, sec.APIKey); err != nil {
		add("key", "failed", server.Redact(err.Error(), sec))
		return &output.Error{Code: output.Blocked, Message: server.Redact(fmt.Sprintf("restored (epoch %d), but the server did not start again on its API key (%v): run backsight server start", next.Epoch, err), sec)}
	}
	add("key", "done", "the server runs on its API key again")
	return nil
}

// epochBefore reads the server's restore epoch through the API, else read
// only from its database; how names the fallback.
func epochBefore(ctx context.Context, cl *hindsight.Client, db server.DB, bank string) (retain.Epoch, string, error) {
	ep, apiErr := retain.ReadEpoch(ctx, cl, bank)
	if apiErr == nil {
		return ep, "", nil
	}
	meta, err := db.DocumentMetadata(ctx, retain.ControlBank(bank), retain.EpochDocument)
	if err != nil {
		return ep, "", fmt.Errorf("API: %w; database: %w", apiErr, err)
	}
	if meta == nil {
		return retain.Epoch{}, "the restore epoch was read from the database (the API: " + apiErr.Error() + "): none yet", nil
	}
	if ep, err = retain.EpochFromMetadata(meta); err != nil {
		return ep, "", fmt.Errorf("API: %w; database: %w", apiErr, err)
	}
	return ep, fmt.Sprintf("the restore epoch (%d) was read from the database (the API: %v)", ep.Epoch, apiErr), nil
}

// startOnKey renders the server's files with key as its API key, starts
// the project (the hindsight container is recreated when the key changes)
// and waits for health on that key.
func startOnKey(ctx context.Context, cfg config.Config, sec config.Secrets, c server.Compose, key string) error {
	s := sec
	s.APIKey = key
	files, err := renderFiles(cfg, s, "backsight restore")
	if err != nil {
		return err
	}
	if _, err := server.Write(cfg.Server.ComposeDir, files); err != nil {
		return err
	}
	if err := c.Up(ctx); err != nil {
		return err
	}
	_, err = waitHealthy(ctx, cfg, s)
	return err
}

// startOnTemporaryKey starts the server on a new random key and returns it
// with a client that uses it.
func startOnTemporaryKey(ctx context.Context, cfg config.Config, sec config.Secrets, c server.Compose) (string, *hindsight.Client, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	key := "bs-restore-" + hex.EncodeToString(b)
	if err := startOnKey(ctx, cfg, sec, c, key); err != nil {
		return "", nil, err
	}
	cl, err := hindsight.New(cfg.Server.URL, key, 30*time.Second)
	return key, cl, err
}

func writeEpochOn(ctx context.Context, cfg config.Config, key string, e retain.Epoch) error {
	cl, err := hindsight.New(cfg.Server.URL, key, epochTimeout)
	if err != nil {
		return err
	}
	return retain.WriteEpoch(ctx, cl, cfg.Server.Bank, e)
}

// restorable refuses, before anything stops, an archive this install
// cannot run: one from a newer Hindsight than the pinned one (its schema is
// ahead), one it cannot place, one of another bank, or one whose vectors
// come from other embedding models.
func restorable(m backup.Manifest, cfg config.Config) error {
	pinned := server.ImageFor(cfg)
	cmp, ok := server.CompareImages(m.HindsightImage, pinned)
	embeddings := cfg.Embeddings.Preset + ": " + server.EmbeddingModels
	switch {
	case !ok:
		_, digest, _ := strings.Cut(m.HindsightImage, "@")
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("cannot tell whether the archive's Hindsight %q is older than this install's %q: "+
			"restore it with its own image (server.digest = %q in config.toml, then backsight server start)", m.HindsightImage, pinned, digest)}
	case cmp > 0:
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("the archive was taken with Hindsight %s, newer than this install's %s: update backsight first", m.HindsightImage, pinned)}
	case m.Bank != cfg.Server.Bank:
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("the archive holds bank %q, this install uses %q (server.bank)", m.Bank, cfg.Server.Bank)}
	case m.Embeddings != embeddings:
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("the archive's vectors come from the embedding models %q, this install uses %q: its recall would not match", m.Embeddings, embeddings)}
	}
	return nil
}

// rollback is what a failed restore needs to put the server back.
type rollback struct {
	res      *restoreResult
	cfg      config.Config
	sec      config.Secrets
	c        server.Compose
	db       server.DB
	d        paths.Dirs
	dump     string           // the safety dump ("" with --no-safety)
	want     map[string]int64 // its counts
	dropped  bool             // the database was dropped: it must be restored
	highest  int              // the highest epoch seen or attempted
	history  []retain.EpochEntry
	safetyAt time.Time
	tempKey  string
	add      func(name, status, detail string)
}

// run puts the safety dump back when restore dropped the database (else
// the database is as it was), starts the server on a temporary key, writes
// an epoch above every one seen or attempted from the safety dump's time
// (a client that sent meanwhile reconciles), then starts the server on its
// key: exit 30. Each step has a deadline, and none follows a cancel. When
// a step fails the exit is 20 and the marker stays: doctor names the way
// back.
func (r *rollback) run(ctx context.Context, cause error) error {
	cause = redacted(cause, r.sec)
	base := context.WithoutCancel(ctx)
	step := func(fn func(ctx context.Context) error) error {
		sctx, cancel := context.WithTimeout(base, rollbackStepTimeout)
		defer cancel()
		return fn(sctx)
	}
	err := step(func(ctx context.Context) error { return r.c.Stop(ctx, "hindsight") })
	if err == nil && r.dropped {
		if r.dump == "" {
			err = errors.New("the database was replaced and there is no safety dump (--no-safety)")
		} else {
			err = step(func(ctx context.Context) error {
				if _, err := r.db.Restore(ctx, r.dump); err != nil {
					return err
				}
				got, err := r.db.Counts(ctx, backup.Tables)
				if err == nil && !sameCounts(got, r.want) {
					err = fmt.Errorf("counts %v differ from the safety archive's %v", got, r.want)
				}
				return err
			})
		}
	}
	// Health only: the safety archive's data was serving before, and a
	// recall that fails for another reason would fail here too.
	var key string
	if err == nil {
		err = step(func(ctx context.Context) (err error) {
			key, _, err = startOnTemporaryKey(ctx, r.cfg, r.sec, r.c)
			return err
		})
	}
	epoch := retain.Epoch{Epoch: r.highest + 1, RestoredAt: now().UTC().Truncate(time.Second), BackupCreatedAt: r.safetyAt, History: r.history}
	if err == nil {
		err = step(func(ctx context.Context) error { return writeEpochOn(ctx, r.cfg, key, epoch) })
	}
	if err == nil {
		_ = state.ClearRestoreMarker(r.d)
		err = step(func(ctx context.Context) error { return startOnKey(ctx, r.cfg, r.sec, r.c, r.sec.APIKey) })
		if err != nil {
			r.add("rollback", "failed", server.Redact(err.Error(), r.sec))
			return &output.Error{Code: output.Blocked, Message: server.Redact(fmt.Sprintf("restore failed (%v) and was rolled back (epoch %d), but the server did not start again on its API key (%v): run backsight server start",
				cause, epoch.Epoch, err), r.sec)}
		}
	}
	way := "backsight restore " + r.res.Safety + " --no-safety"
	if r.res.Safety == "" {
		way = "backsight restore " + r.res.Archive + " --no-safety"
	}
	if err != nil {
		r.add("rollback", "failed", server.Redact(err.Error(), r.sec))
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("restore failed (%v) and so did the rollback (%v): every client holds its sends; restore the server as it was by hand with %s",
			cause, server.Redact(err.Error(), r.sec), way)}
	}
	r.res.RolledBack = true
	r.res.Epoch = &epoch
	r.add("rollback", "done", fmt.Sprintf("the server runs the safety archive's data again, restore epoch %d", epoch.Epoch))
	return &output.Error{Code: output.RolledBack, Message: fmt.Sprintf("restore failed and was rolled back to the safety archive %s: %v", r.res.Safety, cause)}
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
	case r.Epoch != nil && !r.RolledBack:
		fmt.Fprintf(&b, "\nrestored %s (%v); restore epoch %d: clients re-read the server. The server as it was is in %s\n",
			r.Archive, r.Restored, r.Epoch.Epoch, r.Safety)
	}
	return b.String()
}
