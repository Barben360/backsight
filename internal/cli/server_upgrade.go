package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
)

// server upgrade (SPEC §11.7) moves the local server to the version this
// binary pins (decision 0017: a digest validated in CI). update apply runs
// the new binary's `server upgrade`: the binary that pins a version is
// the one that deploys it, so an older binary never needs to know a newer
// one's pin, and the version to roll back to is the one deployed before.

type serverUpgradeResult struct {
	Runtime    string        `json:"runtime"`
	Current    string        `json:"current"` // the version deployed before
	Target     string        `json:"target"`  // this binary's pin
	Upgrade    bool          `json:"upgrade"` // they differ
	Plan       bool          `json:"plan"`
	Safety     string        `json:"safety_archive,omitempty"`
	Steps      []step        `json:"steps"`
	RolledBack bool          `json:"rolled_back,omitempty"`
	Epoch      *retain.Epoch `json:"epoch,omitempty"`  // the rollback's
	Pruned     []string      `json:"pruned,omitempty"` // backups beyond the retention, once the upgrade passed
	Warnings   []string      `json:"warnings,omitempty"`
	MS         int           `json:"ms"`
}

func newServerUpgradeCmd(e *env) *cobra.Command {
	var plan, allowUnordered bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Move the local server to the version this binary pins, after a verified safety backup; roll back on any failure",
		Long: "Upgrade compares the server version deployed (the Hindsight image in compose.yaml) with this binary's pin. When they differ it takes " +
			"the maintenance lock and server.lock, pulls the new image (a failure changes nothing), makes a safety backup and verifies it in a " +
			"throwaway database, marks an upgrade in progress (every client holds its sends), deploys and starts the new version, waits for " +
			"health and runs a recall. Any failure from the deploy on restores the safety dump into the previous version through restore's " +
			"rollback (a new restore epoch, exit 30); a rollback that fails exits 20 and doctor names the way back. It never downgrades, and " +
			"refuses versions it cannot order (the same tag with another digest, a digest alone) unless --allow-unordered is given.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			res := serverUpgradeResult{Plan: plan, Steps: []step{}}
			err := runServerUpgrade(cmd.Context(), &res, plan, allowUnordered, streamStep(e, "server upgrade", &res.Steps))
			res.MS = int(time.Since(start).Milliseconds())
			return e.report("server upgrade", res, func() string { return serverUpgradeText(res) }, err)
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show the versions and the steps without acting")
	cmd.Flags().BoolVar(&allowUnordered, "allow-unordered", false, "deploy this binary's pin even when it cannot be ordered against the deployed version (the same tag with another digest)")
	return cmd
}

func runServerUpgrade(ctx context.Context, res *serverUpgradeResult, plan, allowUnordered bool, add func(name, status, detail string)) error {
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
	rt := ownGroupRuntime(cfg)
	res.Runtime, res.Target = rt.Kind(), rt.Version()
	if res.Current, err = rt.Deployed(); err != nil {
		return blocked(err)
	}
	if res.Current == "" {
		return &output.Error{Code: output.Blocked, Message: "no server is installed here (no compose.yaml in " + cfg.Server.ComposeDir + "): run backsight server install"}
	}
	cmp, comparable := rt.ComparePins(res.Current, res.Target)
	if comparable && cmp == 0 {
		add("check", "done", "the server runs "+res.Target+", this binary's pin")
		return nil
	}
	res.Upgrade = true
	if comparable && cmp > 0 {
		return nothingChanged("the server runs %s, newer than this binary's %s: update backsight instead", res.Current, res.Target)
	}
	if !comparable && !allowUnordered { // "never downgrades" cannot be told
		return nothingChanged("the server runs %s and this binary pins %s, which cannot be ordered (the same version with another digest, or no version): "+
			"rerun with --allow-unordered to deploy this binary's pin", res.Current, res.Target)
	}
	if state.RestoreIncomplete(d) {
		return nothingChanged("a restore or upgrade did not finish (%s): run backsight doctor for the way back", d.RestoreMarker())
	}
	if held := state.Inspect(d.MaintenanceLock()); held != nil && !held.Stale && !heldByUpdate(held) {
		return nothingChanged("maintenance in progress: %s", held.Error())
	}
	add("check", map[bool]string{true: "planned", false: "done"}[plan], res.Current+" → "+res.Target)
	if plan {
		for _, s := range [][2]string{
			{"lock", "would take the maintenance lock and server.lock until the end (hooks queue, jobs skip)"},
			{"pull", "would download " + res.Target + " (a failure changes nothing)"},
			{"safety backup", "would back the server up and verify the archive in a throwaway database"},
			{"deploy", "would mark an upgrade in progress (clients hold their sends) and deploy " + res.Target},
			{"start", "would start it (Hindsight migrates its database on start)"},
			{"health", "would wait for health"},
			{"smoke", "would run a recall"},
		} {
			add(s[0], "planned", s[1])
		}
		return nil
	}

	unlock, note, err := maintenanceFor(d, "server upgrade")
	if err != nil {
		return err
	}
	defer unlock()
	if note != "" {
		res.Warnings = append(res.Warnings, note)
	}
	sl, _, err := state.Acquire(filepath.Join(d.LocksDir(), "server.lock"), "server upgrade")
	if err != nil {
		return &output.Error{Code: output.Blocked, Message: err.Error()}
	}
	defer func() { _ = sl.Release() }()
	add("lock", "done", d.MaintenanceLock()+", server.lock")

	db := rt.Database()
	_ = db.RemoveStaleDumps(ctx)
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 30*time.Second)
	if err != nil {
		return blocked(err)
	}
	before, how, err := epochBefore(ctx, cl, db, cfg.Server.Bank)
	if err != nil {
		return nothingChanged("%s", server.Redact("cannot read the server's restore epoch ("+err.Error()+"), which a rollback must go above", sec))
	}
	if how != "" {
		res.Warnings = append(res.Warnings, how)
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		return nothingChanged("cannot read this client's restore epoch (%v)", err)
	}
	// A rollback puts the safety dump back, every bank of the server with
	// it: each other memory bank gets an epoch too, with no question (the
	// dump is minutes old; issue 285 review M3).
	others, how, err := otherBanks(ctx, cl, db, cfg.Server.Bank)
	if err != nil {
		return nothingChanged("%s", server.Redact("cannot list the server's banks ("+err.Error()+"), which a rollback reverts too", sec))
	}
	if how != "" {
		res.Warnings = append(res.Warnings, how)
	}
	if err := readOthers(ctx, cl, db, st, others, func(w string) { res.Warnings = append(res.Warnings, w) }); err != nil {
		return nothingChanged("%s", server.Redact(err.Error()+", which a rollback must go above", sec))
	}

	// 1. The new version on disk first: a bad pin fails here, unchanged.
	if err := rt.Fetch(ctx); err != nil {
		add("pull", "failed", server.Redact(err.Error(), sec))
		return nothingChanged("%s", server.Redact("cannot download "+res.Target+" ("+err.Error()+")", sec))
	}
	add("pull", "done", res.Target)

	// 2. The safety backup, its clear dump verified and kept to roll back to.
	work, done, err := backup.VerifyDir(filepath.Join(d.State, "staging"))
	if err != nil {
		return blocked(err)
	}
	defer done()
	dump := filepath.Join(work, "safety.dump")
	safetyAt := now().UTC().Truncate(time.Second)
	sb, err := createBackup(ctx, &safetyCopy{dump: dump, image: res.Current})
	if err != nil {
		return nothingChanged("%s", server.Redact("the safety backup failed ("+err.Error()+")", sec))
	}
	res.Safety = sb.Path
	res.Warnings = append(res.Warnings, sb.Warnings...)
	counts, err := rt.VerifyDump(ctx, dump, backup.Tables)
	if err != nil || !sameCounts(counts, sb.Counts) {
		if err == nil {
			err = fmt.Errorf("restored counts %v differ from the backup's %v", counts, sb.Counts)
		}
		return nothingChanged("%s", server.Redact("the safety backup "+sb.Path+" does not verify ("+err.Error()+")", sec))
	}
	add("safety backup", "done", fmt.Sprintf("%s (%v), verified", sb.Path, sb.Counts))

	// 3. An upgrade in progress from here: every client holds its sends
	// until it passed or the rollback finished (restore's marker, 0044).
	if err := state.WriteRestoreMarker(d, state.RestoreMarker{Command: "server upgrade", Safety: sb.Path, Archive: sb.Path, StartedAt: safetyAt}); err != nil {
		return nothingChanged("cannot mark the upgrade in progress (%v)", err)
	}
	rres := &restoreResult{Archive: sb.Path, Safety: sb.Path}
	rb := &rollback{res: rres, cfg: cfg, sec: sec, rt: rt.WithVersion(res.Current), db: db, d: d, dump: dump, want: sb.Counts,
		highest: max(before.Epoch, st.EpochOf(cfg.Server.Bank)), history: before.History, safetyAt: safetyAt, add: add, op: "server upgrade", others: others}
	failed := func(name string, err error) error {
		add(name, "failed", server.Redact(err.Error(), sec))
		rerr := rb.run(ctx, fmt.Errorf("%s: %w", name, err))
		res.RolledBack, res.Epoch = rres.RolledBack, rres.Epoch
		return rerr
	}

	// 4. Deploy, start (the database may migrate from here), health, smoke.
	files, err := renderOn(rt, sec, "backsight server upgrade")
	if err != nil {
		return failed("deploy", err)
	}
	if _, err := rt.Write(files); err != nil {
		return failed("deploy", err)
	}
	add("deploy", "done", res.Target)
	rb.dropped = true // a started server may have migrated the database
	if err := rt.Start(ctx); err != nil {
		return failed("start", err)
	}
	add("start", "done", "project "+cfg.Server.Project)
	v, err := waitHealthy(ctx, rt, cfg, sec)
	if err != nil {
		return failed("health", err)
	}
	add("health", "done", "Hindsight "+v.APIVersion+" healthy")
	if _, err := cl.Recall(ctx, cfg.Server.Bank, hindsight.RecallRequest{Query: "backsight upgrade check", Budget: "low", MaxTokens: 512}); err != nil {
		return failed("smoke", fmt.Errorf("recall: %w", err))
	}
	if err := smokeHook(); err != nil {
		return failed("smoke", err)
	}
	add("smoke", "done", "recall answered")
	if err := ctx.Err(); err != nil {
		return failed("smoke", err)
	}
	_ = state.ClearRestoreMarker(d)

	// The safety archive is a backup like the others: the normal retention
	// applies, so upgrades do not pile them up (the newest is kept, and any
	// archive younger than 24 h).
	dir := cfg.Backup.Dir
	if dir == "" {
		dir = filepath.Join(d.Data, "backups")
	}
	pruned, err := backup.Prune(dir, now(), cfg.Backup.KeepDaily, cfg.Backup.KeepWeekly)
	res.Pruned = pruned
	if err != nil {
		res.Warnings = append(res.Warnings, "pruning the backups: "+err.Error())
	}
	return nil
}

func serverUpgradeText(r serverUpgradeResult) string {
	var b strings.Builder
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning  %s\n", w)
	}
	switch {
	case !r.Upgrade:
		fmt.Fprintf(&b, "\nthe server is up to date (%s)\n", r.Target)
	case r.Plan:
		fmt.Fprintf(&b, "\nplan only: nothing was changed (%s would replace %s)\n", r.Target, r.Current)
	case !r.RolledBack && r.Safety != "":
		fmt.Fprintf(&b, "\nthe server runs %s; the server as it was is in %s\n", r.Target, r.Safety)
	}
	return b.String()
}

// serverPlan is what update apply reads from the new binary's
// `server upgrade --plan`.
type serverPlan struct {
	Current string `json:"current"`
	Target  string `json:"target"`
	Upgrade bool   `json:"upgrade"`
}

func decodeServerPlan(raw json.RawMessage) serverPlan {
	var p serverPlan
	_ = json.Unmarshal(raw, &p)
	return p
}
