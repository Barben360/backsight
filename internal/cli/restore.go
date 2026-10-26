package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	"github.com/Barben360/backsight/internal/setup"
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
	// Purged: the purges made after the archive was taken (tombstones):
	// their records are never sent again, but the archive may hold them.
	Purged []state.Tombstone `json:"purged_after_archive,omitempty"`
	// PurgedAgain: the purged documents the archive brought back, which
	// restore deleted again (issue 285).
	PurgedAgain []string `json:"purged_again,omitempty"`
	// OtherBanks: the server's other memory banks, which the archive's
	// database reverts too: each gets a new epoch (M5 review rules).
	OtherBanks []string       `json:"other_banks,omitempty"`
	Epochs     map[string]int `json:"other_epochs,omitempty"`
	Warnings   []string       `json:"warnings,omitempty"`
	MS         int            `json:"ms"`
}

type restoreFlags struct {
	identity                 string
	plan, noSafety, nonInter bool
	answers                  []string
}

func newRestoreCmd(e *env) *cobra.Command {
	var fl restoreFlags
	cmd := &cobra.Command{
		Use:   "restore <file>",
		Short: "Restore a backup into the local server, after a safety backup, and start a new restore epoch",
		Long: "Restore takes the maintenance lock and server.lock, backs the server up (the safety archive), marks a restore in progress " +
			"(every client holds its sends), stops Hindsight, replaces its database with the archive's, starts it again on a temporary API key " +
			"only restore knows and checks the row counts, health and a recall. It then writes back the purge tombstones and a new restore epoch " +
			"in every memory bank of the server (the database holds them all), so every client re-reads what the server holds (decision 0044), " +
			"deletes again the purged documents the archive brought back, and starts the server again on its API key. A bank without a " +
			"backsight control bank goes back to the archive only with --answer other_banks=revert. Any failure after the stop restores the " +
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
	cmd.Flags().StringArrayVar(&fl.answers, "answer", nil, "answer a question: other_banks=revert (the server's other banks go back to the archive too)")
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
	answers, err := parseAnswers(fl.answers, "")
	if err != nil {
		return err
	}
	for k, v := range answers {
		switch {
		case k != "other_banks":
			return &output.Error{Code: output.Usage, Message: "unknown answer " + k + ": restore asks other_banks"}
		case v != "revert":
			return &output.Error{Code: output.Usage, Message: "other_banks=" + v + ": the only answer is revert (else do not restore this archive)"}
		}
	}
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
	if err := restorable(m, cfg, runtimeFor(cfg)); err != nil {
		return err
	}
	// A live maintenance refuses, the plan too. A restore that did not
	// finish does not: restoring again (its safety archive, or the
	// archive) is the way back, and this run replaces its marker.
	// update apply's own rollback restores under the lock it holds.
	if held := state.Inspect(d.MaintenanceLock()); held != nil && !held.Stale && !heldByUpdate(held) {
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
	// The epoch goes into <bank>-control: one backsight did not make
	// refuses here, before anything stops (the epoch write would refuse
	// it only after the restore, and roll back). A server that cannot
	// answer is left to the epoch read below, which falls back on the
	// database.
	if _, _, err := retain.OwnedBank(ctx, cl, retain.ControlBank(cfg.Server.Bank), retain.ControlMarker); err != nil {
		if foreign := (*retain.ForeignBank)(nil); errors.As(err, &foreign) {
			return &output.Error{Code: output.Blocked, Message: err.Error() + "; the restore epoch could not be written there: nothing was changed"}
		}
	}
	// The archive's database holds every bank of the server: the other
	// memory banks go back to it too, so each gets an epoch. One that has
	// no control bank may not be backsight's: asked first (M5 review
	// rules), before anything changes.
	var listDB server.Database
	if !res.Plan {
		listDB = ownGroupRuntime(cfg).Database()
	}
	others, how, err := otherBanks(ctx, cl, listDB, cfg.Server.Bank)
	switch {
	case err != nil && res.Plan:
		res.Warnings = append(res.Warnings, "cannot list the server's banks ("+err.Error()+"): the other banks, which the archive reverts too, are not listed")
	case err != nil:
		return &output.Error{Code: output.Blocked, Message: server.Redact("cannot list the server's banks ("+err.Error()+"): nothing was changed; check backsight server status", sec)}
	}
	if how != "" {
		res.Warnings = append(res.Warnings, how)
	}
	var unmarked []string
	for _, o := range others {
		res.OtherBanks = append(res.OtherBanks, o.bank)
		if !o.control {
			unmarked = append(unmarked, o.bank)
		}
	}
	// update apply's rollback restores the safety archive the upgrade took
	// minutes before, through the previous binary: nobody to ask there, and
	// the other banks only go back those minutes.
	byUpdate := false
	if held := state.Inspect(d.MaintenanceLock()); held != nil && !held.Stale && heldByUpdate(held) {
		byUpdate = true
	}
	if len(unmarked) > 0 && answers["other_banks"] != "revert" && !byUpdate {
		what := fmt.Sprintf("the archive replaces the server's whole database: bank(s) %s, which have no backsight control bank, go back to the archive's state too "+
			"(what they got since is lost) and get a restore epoch", strings.Join(unmarked, ", "))
		if !res.Plan {
			return &output.Error{Code: output.NeedsUser, Resume: "backsight restore " + res.Archive + " --answer other_banks=revert",
				Questions: []setup.Question{{ID: "other_banks", Required: true, Options: []setup.Option{{Value: "revert", Label: "restore and revert them too"}},
					Prompt: what + ": answer revert to go on"}},
				Message: "restore needs an answer to other_banks (--answer other_banks=revert): nothing was changed"}
		}
		res.Warnings = append(res.Warnings, what+": restore asks --answer other_banks=revert")
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
		tombs, err := retain.ReadTombstones(ctx, cl, cfg.Server.Bank)
		if err != nil {
			res.Warnings = append(res.Warnings, "cannot read the purge tombstones of the server ("+err.Error()+"): only this machine's purges are listed")
		}
		st, _ := state.Load(d.StateFile())
		tombs = mergeTombstones(tombs, st.TombstonesOf(cfg.Server.Bank))
		for _, o := range others {
			ts, err := retain.ReadTombstones(ctx, cl, o.bank)
			if err != nil {
				res.Warnings = append(res.Warnings, "cannot read the purge tombstones of bank "+o.bank+" ("+err.Error()+")")
			}
			tombs = append(tombs, mergeTombstones(ts, st.TombstonesOf(o.bank))...)
		}
		res.Purged = purgedAfter(tombs, m.CreatedAt)
		if len(others) > 0 {
			epoch += " in bank " + cfg.Server.Bank + ", and a new epoch in bank(s) " + strings.Join(res.OtherBanks, ", ")
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
			{"bank", "would compare the archive's bank settings with this backsight's template, and apply it where they differ"},
			{"key", "would start the server again on its API key"},
		} {
			add(s[0], "planned", s[1])
			if s[0] == "recall" && len(res.Purged) > 0 {
				add("purges", "planned", purgesPlanned(res.Purged))
			}
			if s[0] == "epoch" && len(res.Purged) > 0 {
				add("purge again", "planned", "would delete again those of the "+strconv.Itoa(len(res.Purged))+" document(s) purged after the archive was taken that it brings back: "+documentList(res.Purged))
			}
		}
		return nil
	}
	add("check", "done", fmt.Sprintf("archive of %s, Hindsight %s, counts %v", m.CreatedAt.Format(time.RFC3339), m.HindsightImage, m.Counts))

	// 2. Locks, held to the end: hooks only queue, jobs skip, and no other
	// command starts or rewrites the server.
	unlock, note, err := maintenanceFor(d, "restore")
	if err != nil {
		return err
	}
	defer unlock()
	if note != "" {
		res.Warnings = append(res.Warnings, note)
	}
	sl, _, err := state.Acquire(filepath.Join(d.LocksDir(), "server.lock"), "restore")
	if err != nil {
		return &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	defer func() { _ = sl.Release() }()
	add("lock", "done", d.MaintenanceLock()+", server.lock")

	rt := ownGroupRuntime(cfg)
	db := rt.Database()
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
	// The purges known now go on after the restore: the archive's control
	// bank lacks those made since, and a client that reads it would send
	// their records again (M5 review rules).
	tombs, how, err := tombstonesBefore(ctx, cl, db, cfg.Server.Bank)
	if err != nil {
		return &output.Error{Code: output.Blocked, Message: server.Redact("cannot read the server's purge tombstones ("+err.Error()+"): nothing was changed; check backsight server status", sec)}
	}
	if how != "" {
		res.Warnings = append(res.Warnings, how)
	}
	tombs = mergeTombstones(tombs, st.TombstonesOf(cfg.Server.Bank))
	all := slices.Clone(tombs)
	for i := range others {
		o := &others[i]
		if o.foreign {
			res.Warnings = append(res.Warnings, "bank "+o.bank+": its control bank "+retain.ControlBank(o.bank)+" is not backsight's, so no restore epoch is written there")
			continue
		}
		ep, how, err := epochBefore(ctx, cl, db, o.bank)
		if err != nil {
			return &output.Error{Code: output.Blocked, Message: server.Redact("cannot read the restore epoch of bank "+o.bank+" ("+err.Error()+"): nothing was changed; check backsight server status", sec)}
		}
		ts, how2, err := tombstonesBefore(ctx, cl, db, o.bank)
		if err != nil {
			return &output.Error{Code: output.Blocked, Message: server.Redact("cannot read the purge tombstones of bank "+o.bank+" ("+err.Error()+"): nothing was changed; check backsight server status", sec)}
		}
		for _, w := range []string{how, how2} {
			if w != "" {
				res.Warnings = append(res.Warnings, "bank "+o.bank+": "+w)
			}
		}
		o.highest, o.history, o.tombs = ep.Epoch, ep.History, mergeTombstones(ts, st.TombstonesOf(o.bank))
		all = append(all, o.tombs...)
	}
	res.Purged = purgedAfter(all, m.CreatedAt)
	rb := &rollback{res: res, cfg: cfg, sec: sec, rt: rt, db: db, d: d, highest: max(before.Epoch, st.EpochOf(cfg.Server.Bank)), history: before.History,
		safetyAt: now().UTC().Truncate(time.Second), add: add, others: others}

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
	if err := rt.StopAPI(ctx); err != nil {
		// The database is untouched: whatever did stop starts again.
		add("stop", "failed", server.Redact(err.Error(), sec))
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackStepTimeout)
		_ = rt.Start(uctx)
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
	tmp, tcl, err := startOnTemporaryKey(ctx, cfg, sec, rt)
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

	// 6. The purges, before the epoch tells the clients to re-read: the
	// restored control banks lack those made since the archive.
	if len(all) > 0 {
		for _, b := range append([]otherBank{{bank: cfg.Server.Bank, tombs: tombs}}, others...) {
			if len(b.tombs) == 0 || b.foreign {
				continue
			}
			if err := writeTombstonesOn(ctx, cfg, tmp, b.bank, b.tombs); err != nil {
				return failed("purges", fmt.Errorf("bank %s: %w", b.bank, err))
			}
		}
		add("purges", "done", fmt.Sprintf("%d purge(s) kept, %d of them made after the archive", len(all), len(res.Purged)))
	}

	// 7. The new epochs, last: only a restore that passed tells the
	// clients, of this bank and of every other the archive reverted.
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
	if err := writeEpochOn(ctx, cfg, tmp, cfg.Server.Bank, next); err != nil {
		return failed("epoch", err)
	}
	res.Epoch = &next
	detail := fmt.Sprintf("restore epoch %d", next.Epoch)
	for i := range rb.others {
		o := &rb.others[i]
		if o.foreign {
			continue
		}
		restored, err := retain.ReadEpoch(ctx, tcl, o.bank)
		if err != nil {
			return failed("epoch", fmt.Errorf("bank %s: %w", o.bank, err))
		}
		o.highest = max(o.highest, restored.Epoch)
		e := retain.Epoch{Epoch: o.highest + 1, RestoredAt: next.RestoredAt, BackupCreatedAt: next.BackupCreatedAt, History: o.history}
		o.highest = e.Epoch
		if err := writeEpochOn(ctx, cfg, tmp, o.bank, e); err != nil {
			return failed("epoch", fmt.Errorf("bank %s: %w", o.bank, err))
		}
		if res.Epochs == nil {
			res.Epochs = map[string]int{}
		}
		res.Epochs[o.bank] = e.Epoch
		detail += fmt.Sprintf(", bank %s at %d", o.bank, e.Epoch)
	}
	add("epoch", "done", detail)

	// 8. The purged documents the archive brought back, deleted again
	// (privacy first, issue 285): still on the temporary key, so no client
	// reads them meanwhile. A failure rolls back to the safety archive,
	// which does not hold them.
	again, err := purgeAgain(ctx, tcl, append([]otherBank{{bank: cfg.Server.Bank, tombs: tombs}}, rb.others...), cfg.Server.Bank)
	if err != nil {
		return failed("purge again", err)
	}
	if res.PurgedAgain = again; len(again) > 0 {
		add("purge again", "done", fmt.Sprintf("%d purged document(s) the archive brought back, deleted again: %s", len(again), strings.Join(again, ", ")))
	}

	// The bank's settings, still on the temporary key: after the epoch, so
	// a rollback never leaves a hash recorded for settings it put back.
	if w := restoreBank(ctx, tcl, d, cfg.Server.Bank, add); w != "" {
		res.Warnings = append(res.Warnings, w)
	}

	// 9. The server on its own key again; the marker goes only then
	// (issue 285): until that, server start finishes the restore.
	markKeyStep(d)
	if err := startOnKey(ctx, cfg, sec, rt, sec.APIKey); err != nil {
		add("key", "failed", server.Redact(err.Error(), sec))
		return &output.Error{Code: output.Blocked, Message: server.Redact(fmt.Sprintf("restored (epoch %d), but the server did not start again on its API key (%v): run backsight server start", next.Epoch, err), sec)}
	}
	_ = state.ClearRestoreMarker(d)
	add("key", "done", "the server runs on its API key again")
	return nil
}

// otherBank is a memory bank of the server besides this install's: the
// archive's database reverts it too (M5 review rules).
type otherBank struct {
	bank    string
	control bool // its <bank>-control is backsight's
	foreign bool // its <bank>-control exists and is not backsight's
	highest int  // the highest epoch seen or attempted
	history []retain.EpochEntry
	tombs   []state.Tombstone
}

// otherBanks are the server's memory banks besides own: each bank whose
// <bank>-control is backsight's, and every bank listed but control banks
// and docs banks (decision 0069). The listing comes from the API, else
// (db not nil) read only from the database, where a control bank counts as
// backsight's; how names that fallback.
func otherBanks(ctx context.Context, cl *hindsight.Client, db server.Database, own string) ([]otherBank, string, error) {
	ids, apiErr := cl.BankIDs(ctx)
	how := ""
	if apiErr != nil {
		if db == nil {
			return nil, "", apiErr
		}
		var err error
		if ids, err = db.BankIDs(ctx); err != nil {
			return nil, "", fmt.Errorf("API: %w; database: %w", apiErr, err)
		}
		how = "the server's banks were read from the database (the API: " + apiErr.Error() + ")"
	}
	listed := map[string]bool{}
	for _, id := range ids {
		listed[id] = true
	}
	byName := map[string]*otherBank{}
	add := func(b string) *otherBank {
		if byName[b] == nil {
			byName[b] = &otherBank{bank: b}
		}
		return byName[b]
	}
	for _, id := range ids {
		switch {
		case strings.HasSuffix(id, "-control"):
			b := strings.TrimSuffix(id, "-control")
			owned := true
			if apiErr == nil {
				_, _, err := retain.OwnedBank(ctx, cl, id, retain.ControlMarker)
				if foreign := (*retain.ForeignBank)(nil); errors.As(err, &foreign) {
					owned = false
				} else if err != nil {
					return nil, "", fmt.Errorf("bank %s: %w", id, err)
				}
			}
			if owned {
				add(b).control = true
			} else if listed[b] {
				add(b).foreign = true
			}
		case strings.HasSuffix(id, "-docs"):
		default:
			add(id)
		}
	}
	delete(byName, own)
	out := make([]otherBank, 0, len(byName))
	for _, b := range byName {
		out = append(out, *b)
	}
	slices.SortFunc(out, func(a, b otherBank) int { return strings.Compare(a.bank, b.bank) })
	return out, how, nil
}

// purgeAgain deletes the documents of each bank's tombstones that the
// server holds (the archive brought them back), and names them: the
// document id, after "<bank>/" for a bank other than own.
func purgeAgain(ctx context.Context, cl *hindsight.Client, banks []otherBank, own string) ([]string, error) {
	var out []string
	for _, b := range banks {
		if b.foreign {
			continue
		}
		seen := map[string]bool{}
		for _, t := range b.tombs {
			if seen[t.Document] {
				continue
			}
			seen[t.Document] = true
			_, err := cl.GetDocument(ctx, b.bank, t.Document)
			var apiErr *hindsight.APIError
			switch {
			case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
				continue
			case err != nil:
				return out, fmt.Errorf("bank %s, document %s: %w", b.bank, t.Document, err)
			}
			if err := cl.DeleteDocument(ctx, b.bank, t.Document); err != nil && (!errors.As(err, &apiErr) || apiErr.Kind != hindsight.NotFound) {
				return out, fmt.Errorf("bank %s, document %s: %w", b.bank, t.Document, err)
			}
			name := t.Document
			if b.bank != own {
				name = b.bank + "/" + name
			}
			out = append(out, name)
		}
	}
	return out, nil
}

// markKeyStep records in the marker that only the start on the API key is
// left: server start then finishes the restore (issue 285).
func markKeyStep(d paths.Dirs) {
	if m, err := state.ReadRestoreMarker(d); err == nil && m != nil {
		m.Step = state.KeyStep
		_ = state.WriteRestoreMarker(d, *m)
	}
}

// ownGroupRuntime is cfg's runtime with Docker children in their own
// process group: a Ctrl-C must not kill a rollback's docker.
func ownGroupRuntime(cfg config.Config) server.Runtime {
	c := composeFor(cfg)
	if r, ok := c.Runner.(server.DockerRunner); ok {
		r.OwnGroup = true
		c.Runner = r
	}
	return server.Docker{Compose: c, Cfg: cfg}
}

// updateLockEnv tells the binaries update apply runs which maintenance
// lock it holds: "<pid>@<since, Unix seconds>", the lock's own record.
const updateLockEnv = "BACKSIGHT_UPDATE_LOCK"

// updateLock is the maintenance lock update apply took in this process.
var updateLock *state.Owner

func lockToken(o state.Owner) string { return fmt.Sprintf("%d@%d", o.PID, o.Since.Unix()) }

// heldByUpdate: the maintenance lock is update apply's, in this process (the
// lock it took) or in its parent (which named the lock in the child's
// environment): update apply runs the new binary's server upgrade, and its
// rollback the previous binary's restore. A PID alone is not enough: a
// dead update's lock whose PID was reused must not pass.
func heldByUpdate(h *state.HeldError) bool {
	if h.Owner.Command != "update apply" {
		return false
	}
	if o := updateLock; o != nil && o.PID == h.Owner.PID && o.Since.Equal(h.Owner.Since) && o.PID == os.Getpid() {
		return true
	}
	return h.Owner.PID == os.Getppid() && os.Getenv(updateLockEnv) == lockToken(h.Owner)
}

// epochBefore reads the server's restore epoch through the API, else read
// only from its database; how names the fallback.
func epochBefore(ctx context.Context, cl *hindsight.Client, db server.Database, bank string) (retain.Epoch, string, error) {
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
func startOnKey(ctx context.Context, cfg config.Config, sec config.Secrets, rt server.Runtime, key string) error {
	s := sec
	s.APIKey = key
	files, err := renderOn(rt, s, "backsight restore")
	if err != nil {
		return err
	}
	if _, err := rt.Write(files); err != nil {
		return err
	}
	if err := rt.Start(ctx); err != nil {
		return err
	}
	_, err = waitHealthy(ctx, rt, cfg, s)
	return err
}

// startOnTemporaryKey starts the server on a new random key and returns it
// with a client that uses it.
func startOnTemporaryKey(ctx context.Context, cfg config.Config, sec config.Secrets, rt server.Runtime) (string, *hindsight.Client, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	key := "bs-restore-" + hex.EncodeToString(b)
	if err := startOnKey(ctx, cfg, sec, rt, key); err != nil {
		return "", nil, err
	}
	cl, err := hindsight.New(cfg.Server.URL, key, 30*time.Second)
	return key, cl, err
}

func writeTombstonesOn(ctx context.Context, cfg config.Config, key, bank string, ts []state.Tombstone) error {
	cl, err := hindsight.New(cfg.Server.URL, key, epochTimeout)
	if err != nil {
		return err
	}
	_, err = retain.WriteTombstones(ctx, cl, bank, ts)
	return err
}

// tombstonesBefore reads the server's purge tombstones through the API,
// else read only from its database; how names the fallback.
func tombstonesBefore(ctx context.Context, cl *hindsight.Client, db server.Database, bank string) ([]state.Tombstone, string, error) {
	ts, apiErr := retain.ReadTombstones(ctx, cl, bank)
	if apiErr == nil {
		return ts, "", nil
	}
	meta, err := db.DocumentMetadata(ctx, retain.ControlBank(bank), retain.TombstonesDocument)
	if err != nil {
		return nil, "", fmt.Errorf("API: %w; database: %w", apiErr, err)
	}
	if meta == nil {
		return nil, "", nil
	}
	if ts, err = retain.TombstonesFromMetadata(bank, meta); err != nil {
		return nil, "", fmt.Errorf("API: %w; database: %w", apiErr, err)
	}
	return ts, fmt.Sprintf("the purge tombstones (%d) were read from the database (the API: %v)", len(ts), apiErr), nil
}

// mergeTombstones is a with the tombstones of b it lacks.
func mergeTombstones(a, b []state.Tombstone) []state.Tombstone {
	s := state.State{Tombstones: slices.Clone(a)}
	s.AddTombstones(b...)
	return s.Tombstones
}

// purgedAfter are the tombstones of purges made after t, oldest first.
func purgedAfter(ts []state.Tombstone, t time.Time) []state.Tombstone {
	var out []state.Tombstone
	for _, x := range ts {
		if x.PurgedAt.After(t) {
			out = append(out, x)
		}
	}
	slices.SortStableFunc(out, func(a, b state.Tombstone) int { return a.PurgedAt.Compare(b.PurgedAt) })
	return out
}

// purgesPlanned says what restore does with the purges made after the
// archive: kept, so their records are not sent again; the archive itself
// may still hold the documents.
func purgesPlanned(ts []state.Tombstone) string {
	return fmt.Sprintf("%d document(s) purged after the archive was taken: %s. Their records would not be sent again; "+
		"those the archive holds would be deleted again once it is restored", len(ts), documentList(ts))
}

// documentList names the documents of ts with their purge time, the first
// ten, then how many more.
func documentList(ts []state.Tombstone) string {
	ids := make([]string, 0, min(len(ts), 10))
	for _, t := range ts[:min(len(ts), 10)] {
		ids = append(ids, t.Document+" ("+t.PurgedAt.Format(time.RFC3339)+")")
	}
	if len(ts) > 10 {
		ids = append(ids, fmt.Sprintf("and %d more", len(ts)-10))
	}
	return strings.Join(ids, ", ")
}

func writeEpochOn(ctx context.Context, cfg config.Config, key, bank string, e retain.Epoch) error {
	cl, err := hindsight.New(cfg.Server.URL, key, epochTimeout)
	if err != nil {
		return err
	}
	return retain.WriteEpoch(ctx, cl, bank, e)
}

// restorable refuses, before anything stops, an archive this install
// cannot run: one from a newer Hindsight than the pinned one (its schema is
// ahead), one it cannot place, one of another bank, or one whose vectors
// come from other embedding models.
func restorable(m backup.Manifest, cfg config.Config, rt server.Runtime) error {
	pinned := rt.Version()
	cmp, ok := rt.ComparePins(m.HindsightImage, pinned)
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
	rt       server.Runtime // deploying the version to roll back to
	db       server.Database
	d        paths.Dirs
	dump     string           // the safety dump ("" with --no-safety)
	want     map[string]int64 // its counts
	dropped  bool             // the database was dropped: it must be restored
	highest  int              // the highest epoch seen or attempted
	history  []retain.EpochEntry
	safetyAt time.Time
	tempKey  string
	add      func(name, status, detail string)
	op       string // what failed: "restore" (default) or "server upgrade"
	// others: the server's other memory banks, which the restore reverted
	// too: each gets an epoch above its highest.
	others []otherBank
}

func (r *rollback) what() string {
	if r.op == "" {
		return "restore"
	}
	return r.op
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
	err := step(func(ctx context.Context) error { return r.rt.StopAPI(ctx) })
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
			key, _, err = startOnTemporaryKey(ctx, r.cfg, r.sec, r.rt)
			return err
		})
	}
	epoch := retain.Epoch{Epoch: r.highest + 1, RestoredAt: now().UTC().Truncate(time.Second), BackupCreatedAt: r.safetyAt, History: r.history}
	if err == nil {
		err = step(func(ctx context.Context) error { return writeEpochOn(ctx, r.cfg, key, r.cfg.Server.Bank, epoch) })
	}
	for _, o := range r.others {
		if err != nil || o.foreign {
			continue
		}
		e := retain.Epoch{Epoch: o.highest + 1, RestoredAt: epoch.RestoredAt, BackupCreatedAt: r.safetyAt, History: o.history}
		if err = step(func(ctx context.Context) error { return writeEpochOn(ctx, r.cfg, key, o.bank, e) }); err != nil {
			err = fmt.Errorf("bank %s: %w", o.bank, err)
		}
	}
	if err == nil {
		// The marker goes once the server runs on its key: until then,
		// server start finishes the rollback.
		markKeyStep(r.d)
		err = step(func(ctx context.Context) error { return startOnKey(ctx, r.cfg, r.sec, r.rt, r.sec.APIKey) })
		if err == nil {
			_ = state.ClearRestoreMarker(r.d)
		}
		if err != nil {
			r.add("rollback", "failed", server.Redact(err.Error(), r.sec))
			return &output.Error{Code: output.Blocked, Message: server.Redact(fmt.Sprintf("%s failed (%v) and was rolled back (epoch %d), but the server did not start again on its API key (%v): run backsight server start",
				r.what(), cause, epoch.Epoch, err), r.sec)}
		}
	}
	way := "backsight restore " + r.res.Safety + " --no-safety"
	if r.res.Safety == "" {
		way = "backsight restore " + r.res.Archive + " --no-safety"
	}
	if err != nil {
		r.add("rollback", "failed", server.Redact(err.Error(), r.sec))
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("%s failed (%v) and so did the rollback (%v): every client holds its sends; restore the server as it was by hand with %s",
			r.what(), cause, server.Redact(err.Error(), r.sec), way)}
	}
	r.res.RolledBack = true
	r.res.Epoch = &epoch
	r.add("rollback", "done", fmt.Sprintf("the server runs the safety archive's data again, restore epoch %d", epoch.Epoch))
	return &output.Error{Code: output.RolledBack, Message: fmt.Sprintf("%s failed and was rolled back to the safety archive %s: %v", r.what(), r.res.Safety, cause)}
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
