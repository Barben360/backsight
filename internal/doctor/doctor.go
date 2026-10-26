// Package doctor runs backsight's read-only health checks (SPEC §15). In M0
// it works with a backsight config or with the reference install it will
// replace, and changes nothing.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/reference"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/target"
)

type Status string

const (
	OK    Status = "ok"
	Info  Status = "info"
	Warn  Status = "warn"
	Error Status = "error"
)

// Check is one line of the report. Fix is a command or an instruction.
type Check struct {
	ID      string `json:"id"`
	Status  Status `json:"status"`
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
	Fix     string `json:"fix,omitempty"`
	// Projects are the project names Summary, Detail and Fix show: report
	// scrubs them, the short ones included (never printed).
	Projects []string `json:"-"`
}

type Report struct {
	Checks   []Check        `json:"checks"`
	Server   *target.Target `json:"server,omitempty"`
	Errors   int            `json:"errors"`
	Warnings int            `json:"warnings"`
}

// Env is everything doctor reads; tests replace the probes.
type Env struct {
	Dirs       paths.Dirs
	Home       string
	Cwd        string // where doctor runs: its .backsight.toml files are checked
	Getenv     func(string) string
	ManagedDir string
	Docker     func(context.Context) (string, error)
	GPUs       func() []string
	FreeBytes  func(path string) (uint64, error)
	// ClaudePlugins runs `claude plugin list --json` in a profile ("" =
	// default); nil skips asking Claude Code.
	ClaudePlugins func(profile string) ([]byte, error)
	Timeout       time.Duration    // per Hindsight call
	Now           func() time.Time // nil: time.Now
	// Linger reports systemd's linger for this user (nil: not asked).
	Linger func() (bool, error)
	// SelfCheck is the running binary's check of itself against its signed
	// checksums.txt (SPEC §11.1); nil: no binary check.
	SelfCheck func() state.SelfCheck
	// Scheduler compares the installed scheduler entries with this
	// binary's templates (nil: not compared).
	Scheduler func() (SchedulerEntries, error)
	InstallProbes
}

const (
	retentionFloor = 365 // days; below it doctor warns (decision 0041)
	diskWarn       = 2 << 30
	diskError      = 256 << 20
)

// Run performs every check in a fixed order.
func Run(ctx context.Context, env Env) Report {
	var r Report
	add := func(cs ...Check) {
		for _, c := range cs {
			r.Checks = append(r.Checks, c)
			switch c.Status {
			case Error:
				r.Errors++
			case Warn:
				r.Warnings++
			}
		}
	}

	cfgCheck, tg := checkConfig(env)
	add(cfgCheck)
	if env.SelfCheck != nil {
		add(checkBinary(env.SelfCheck()))
	}
	add(checkDocker(ctx, env))

	loc := claudecode.Locate(env.Home, env.Getenv)
	ref, refErr := reference.Discover(env.Home, loc)
	if tg == nil {
		tg = target.FromReference(ref)
	}
	r.Server = tg
	srv, cl := checkServer(ctx, env, tg)
	add(srv)
	bank := checkBank(ctx, cl, tg)
	add(bank)
	if bank.Status == OK || bank.Status == Warn {
		add(checkUntagged(ctx, cl, tg.Bank))
		if tg.Source == "config" {
			add(checkBankConfig(ctx, cl, tg.Bank, env.Dirs))
			add(checkEpoch(ctx, cl, tg.Bank, env.Dirs))
		}
	} else {
		add(Check{ID: "untagged", Status: Info, Summary: "skipped: " + skipReason(srv)})
		if tg != nil && tg.Source == "config" {
			add(Check{ID: "restore-epoch", Status: Info, Summary: "skipped: " + skipReason(srv)})
		}
	}
	add(checkNoise(ctx, env, cl, tg, bank, srv)...)
	add(checkOwners(env))
	add(checkMaintenance(env.Dirs))
	add(checkRestore(env.Dirs))
	add(checkUpdateIncomplete(env.Dirs))
	add(checkLocks(env.Dirs))
	add(checkWrites(env))
	add(checkBackfill(env.Dirs))
	add(checkDisk(env))
	add(checkReference(ref, refErr))
	add(checkRetention(loc, env.ManagedDir))
	add(checkClient(env, loc))
	add(checkGPU(env.GPUs()))
	now := time.Now
	if env.Now != nil {
		now = env.Now
	}
	writeOff := false
	if cfg, err := config.Load(env.Dirs.ConfigFile(), env.Dirs, env.Home); err == nil {
		writeOff = !cfg.Components.Write
	}
	add(checkUsage(env.Dirs.TelemetryDir(), now(), writeOff)...)
	add(checkBackup(env, now()))
	add(checkJobs(env.Dirs, now(), env.Linger))
	if env.Scheduler != nil {
		if c, ok := checkScheduler(env.Scheduler); ok {
			add(c)
		}
	}
	add(checkUpdate(env.Dirs, buildinfo.Version, now()))
	add(installChecks(ctx, env, ref, now())...)
	add(checkBacksightToml(env))
	return r
}

func skipReason(srv Check) string {
	switch srv.Status {
	case Info:
		return "no server"
	case Error:
		return "server not usable"
	}
	return "bank not usable"
}

func checkConfig(env Env) (Check, *target.Target) {
	c := Check{ID: "config"}
	tg, err := target.FromConfig(env.Dirs, env.Home)
	var ce *target.ConfigError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		c.Status, c.Summary = Info, "backsight is not set up yet"
		c.Detail = "expected at " + env.Dirs.ConfigFile() + "; run backsight setup. Until then doctor inspects an older Hindsight install, if Claude Code is wired to one"
		return c, nil
	case errors.As(err, &ce) && ce.File == env.Dirs.SecretsFile():
		c.Status, c.Summary, c.Detail = Error, "secrets.toml cannot be used", err.Error()
		c.Fix = "chmod 600 " + ce.File
		return c, nil
	case err != nil:
		c.Status, c.Summary, c.Detail = Error, "config.toml is invalid", err.Error()
		c.Fix = "correct the key named in the detail in " + env.Dirs.ConfigFile()
		return c, nil
	}
	c.Status, c.Summary = OK, "config "+env.Dirs.ConfigFile()
	return c, tg
}

func checkDocker(ctx context.Context, env Env) Check {
	c := Check{ID: "docker"}
	v, err := env.Docker(ctx)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		c.Status, c.Summary = Warn, "Docker is not installed"
		c.Fix = "install Docker Engine (Linux) or Docker Desktop: https://docs.docker.com/get-docker/"
	case err != nil:
		c.Status, c.Summary, c.Detail = Warn, "Docker is not running or not usable", err.Error()
		c.Fix = "start Docker (systemctl start docker) and check that your user can run docker without sudo"
	default:
		c.Status, c.Summary = OK, "Docker "+v
	}
	return c
}

// checkServer returns the server check and, when it passed, a client.
func checkServer(ctx context.Context, env Env, t *target.Target) (Check, *hindsight.Client) {
	c := Check{ID: "server"}
	if t == nil {
		c.Status, c.Summary = Info, "no server configured"
		return c, nil
	}
	from := " (from " + t.Source + ")"
	cl, err := hindsight.New(t.URL, string(t.Token), env.Timeout)
	if err != nil {
		c.Status, c.Summary = Error, "unusable server URL"+from
		c.Detail, c.Fix = err.Error(), "correct the server URL in the "+t.Source
		return c, nil
	}
	var apiErr *hindsight.APIError
	var v hindsight.Version
	if _, err = cl.Health(ctx); err == nil {
		v, err = cl.Version(ctx)
	}
	switch {
	case err == nil:
	case errors.Is(err, hindsight.ErrUnreachable), errors.Is(err, hindsight.ErrTimeout):
		c.Status, c.Summary = Error, "Hindsight unreachable at "+t.Display()+from
		c.Detail = err.Error()
		if t.Source == "reference" {
			c.Fix = "start the older Hindsight server (docker compose up -d in its directory)"
		} else {
			c.Fix = "check it with backsight server status, start it with backsight server start (on the server's machine if it is remote)"
		}
		return c, nil
	case errors.As(err, &apiErr) && apiErr.Kind == hindsight.Unauthorized:
		c.Status, c.Summary = Error, "the server rejected the API key"+from
		c.Fix = "check the key the " + t.Source + " uses against the server's"
		return c, nil
	default:
		c.Status, c.Summary, c.Detail = Error, "Hindsight at "+t.Display()+" answered with an error"+from, err.Error()
		return c, nil
	}
	c.Status, c.Summary = OK, fmt.Sprintf("Hindsight %s at %s%s", v.APIVersion, t.Display(), from)
	if t.Cleartext() {
		c.Status = Warn
		c.Detail = "the API key travels unencrypted to a non-loopback host"
		c.Fix = "reach the server over a tailnet or WireGuard, or keep it on loopback until the TLS proxy exists"
	}
	return c, cl
}

func checkBank(ctx context.Context, cl *hindsight.Client, t *target.Target) Check {
	c := Check{ID: "bank"}
	if cl == nil {
		c.Status, c.Summary = Info, "skipped: "
		if t == nil {
			c.Summary += "no server"
		} else {
			c.Summary += "server not usable"
		}
		return c
	}
	banks, err := cl.Banks(ctx)
	if err != nil {
		c.Status, c.Summary, c.Detail = Error, "cannot list banks", err.Error()
		return c
	}
	found := false
	for _, b := range banks {
		found = found || b.ID == t.Bank
	}
	if !found {
		c.Status, c.Summary = Error, "bank "+t.Bank+" does not exist on the server"
		c.Fix = "check the bank id in the " + t.Source
		return c
	}
	st, err := cl.BankStats(ctx, t.Bank)
	if err != nil {
		c.Status, c.Summary, c.Detail = Error, "cannot read bank "+t.Bank, err.Error()
		return c
	}
	c.Status = OK
	c.Summary = fmt.Sprintf("bank %s: %d memories, %d documents, %d awaiting consolidation", t.Bank, st.TotalNodes, st.TotalDocuments, st.PendingConsolidation)
	if st.FailedOperations > 0 || st.FailedConsolidation > 0 {
		c.Status = Warn
		c.Detail = fmt.Sprintf("%d failed operations, %d failed consolidations", st.FailedOperations, st.FailedConsolidation)
		c.Fix = "open the Hindsight UI operations page to see the provider errors"
	}
	return c
}

// checkBankConfig compares the bank's settings with backsight's template
// (SPEC §15 "bank config drift"). It tells an older backsight template
// (the bank holds the one recorded as applied: bank apply sends this
// binary's) from a change made on the server (only --force puts
// backsight's values back, over those edits), and a template never
// recorded as applied (M5 review rules, "Bank template follows the
// binary"). The comparison is against the templates' own values, never a
// hash of what the server returns (Hindsight normalises settings).
func checkBankConfig(ctx context.Context, cl *hindsight.Client, bank string, d paths.Dirs) Check {
	c := Check{ID: "bank-config", Status: OK, Summary: "bank settings match backsight's template"}
	drift, err := server.BankDrift(ctx, cl, bank)
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot read the bank's settings", err.Error()
		return c
	}
	recorded := ""
	if st, err := state.Load(d.StateFile()); err == nil {
		recorded = st.BankConfigHash
	}
	current := server.TemplateHash()
	changed := func() Check {
		c.Status, c.Summary, c.Detail = Warn, "bank settings were changed on the server", "keys that differ from backsight's template: "+strings.Join(drift, ", ")
		c.Fix = "backsight bank apply --force (puts backsight's values back: it overwrites the edits made to these keys on the server)"
		return c
	}
	switch {
	case len(drift) == 0 && recorded != current:
		c.Status, c.Summary = Warn, "the bank holds this backsight's template, not recorded as applied"
		c.Fix = "backsight bank apply (records it)"
		return c
	case len(drift) == 0:
		return c
	case recorded == current:
		return changed()
	}
	if old, err := server.LoadApplied(d.BankTemplateFile()); err == nil && recorded != "" && old.Hash() == recorded {
		if left, err := server.DriftFrom(ctx, cl, bank, old.Config); err == nil && len(left) > 0 {
			return changed()
		} else if err == nil {
			c.Status, c.Summary = Warn, "the bank holds an older backsight template"
			c.Detail = "keys this backsight's template changes: " + strings.Join(drift, ", ")
			c.Fix = "backsight bank apply (sends this backsight's template)"
			return c
		}
	}
	// No record of the template applied (a version before the record, a
	// restore that could not apply it): either is possible.
	c.Status, c.Summary = Warn, "bank settings differ from this backsight's template: an older backsight template, or a change made on the server"
	c.Detail = "keys that differ: " + strings.Join(drift, ", ")
	c.Fix = "backsight bank apply (sends this backsight's template: it also overwrites any edit made to these keys on the server)"
	return c
}

// checkEpoch reports a client still on an old restore epoch (decision
// 0044): its cursors may hold as sent what the restore lost, until a flush
// reconciles them.
func checkEpoch(ctx context.Context, cl *hindsight.Client, bank string, d paths.Dirs) Check {
	c := Check{ID: "restore-epoch"}
	ep, err := retain.ReadEpoch(ctx, cl, bank)
	var unreadable *retain.EpochUnreadable
	switch {
	case errors.As(err, &unreadable):
		// The server answered with a document that does not parse: every
		// session send holds on it until it does.
		c.Status, c.Summary, c.Detail = Warn, "the bank's restore epoch does not parse: sessions are held", err.Error()
		c.Fix = "the next `backsight restore` rewrites it; or delete document " + retain.EpochDocument + " from bank " + retain.ControlBank(bank) +
			" to go back to epoch 0, once every client reconciled"
		return c
	case err != nil:
		c.Status, c.Summary, c.Detail = Info, "cannot read the bank's restore epoch", err.Error()
		return c
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot read the client's restore epoch", err.Error()
		return c
	}
	if h := st.PurgeHold; h != nil && h.Bank == bank {
		// A reconcile found purged documents on the server it could not
		// purge again: every send holds until one does (issue 285).
		c.Status = Error
		c.Summary = fmt.Sprintf("%d purged document(s) of bank %s are on the server again and could not be purged again: sessions are held", h.Documents, h.Bank)
		if h.Unchecked {
			c.Summary = fmt.Sprintf("%d purged document(s) of bank %s not yet checked (the server could not be read): sessions are held", h.Documents, h.Bank)
		}
		c.Detail = h.Error
		c.Fix = "check backsight server status; the next backsight jobs tick purges them again (as forget --purge does) and reconciles"
		return c
	}
	switch local := st.EpochOf(bank); {
	case local == ep.Epoch:
		c.Status, c.Summary = OK, fmt.Sprintf("client on the bank's restore epoch %d", ep.Epoch)
	case local < ep.Epoch:
		c.Status = Warn
		c.Summary = fmt.Sprintf("client on an old restore epoch (%d; the bank was restored to epoch %d on %s UTC)", local, ep.Epoch, ep.RestoredAt.UTC().Format("2006-01-02 15:04"))
		c.Detail = "what this machine sent after the backup of " + ep.BackupCreatedAt.UTC().Format("2006-01-02 15:04") + " UTC is missing from the server until it reconciles"
		c.Fix = "backsight jobs tick"
	default:
		c.Status, c.Summary = Warn, fmt.Sprintf("client on restore epoch %d, the bank on %d", local, ep.Epoch)
		c.Fix = "backsight jobs tick"
	}
	return c
}

// checkBackup reports the newest backup's age (SPEC §15: > 2 days warns):
// jobs maintain logs its run even when the backup in it fails.
func checkBackup(env Env, now time.Time) Check {
	c := Check{ID: "backup"}
	cfg, err := config.Load(env.Dirs.ConfigFile(), env.Dirs, env.Home)
	switch {
	case err != nil:
		c.Status, c.Summary = Info, "skipped: no backsight config"
		return c
	case !cfg.Backup.Enabled:
		c.Status, c.Summary = Info, "backups are off (backup.enabled = false)"
		return c
	}
	list, err := backup.List(cfg.Backup.Dir)
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot list the backups", err.Error()
		return c
	}
	var last time.Time
	for _, e := range list {
		if e.Manifest.CreatedAt.After(last) {
			last = e.Manifest.CreatedAt
		}
	}
	fix := "backsight backup create; if jobs maintain makes none, see its log (journalctl --user -u backsight-maintain)"
	switch age := now.Sub(last); {
	case last.IsZero():
		c.Status, c.Summary, c.Fix = Warn, "no backup in "+cfg.Backup.Dir, fix
	case age > 48*time.Hour:
		c.Status, c.Summary, c.Fix = Warn, fmt.Sprintf("last backup %.0f days ago (%s)", age.Hours()/24, last.UTC().Format("2006-01-02 15:04")), fix
	default:
		c.Status, c.Summary = OK, fmt.Sprintf("last backup %s UTC, %d kept", last.UTC().Format("2006-01-02 15:04"), len(list))
	}
	return c
}

// checkWrites reports what has not reached the server: queued notes and
// quarantined sessions and notes (SPEC §15).
func checkWrites(env Env) Check {
	d := env.Dirs
	c := Check{ID: "writes", Status: OK}
	ob := retain.Outbox{Path: filepath.Join(d.OutboxDir(), "notes.jsonl"), QuarantinePath: filepath.Join(d.QuarantineDir(), "notes.jsonl")}
	if cfg, err := config.Load(d.ConfigFile(), d, env.Home); err == nil { // no config: the defaults
		ob = ob.WithLimitsMB(cfg.Hooks.OutboxWarnMB, cfg.Hooks.OutboxMaxMB)
	}
	warnAt, maxAt := ob.Limits()
	pending, _ := ob.Pending()
	qNotes, _ := ob.Quarantined()
	qSessions, _ := retain.QuarantinedSessions(d.CursorsDir())
	var size int64
	if fi, err := os.Stat(ob.Path); err == nil {
		size = fi.Size()
	}
	c.Summary = fmt.Sprintf("%d note(s) queued, %d session(s) and %d note(s) quarantined", pending, len(qSessions), qNotes)
	switch {
	case size >= maxAt:
		c.Status, c.Fix = Error, "the notes outbox is full: bring the server back (backsight server status), then backsight jobs tick"
	case len(qSessions)+qNotes > 0:
		c.Status = Warn
		c.Detail = "the server refused them; reasons in " + d.QuarantineDir()
		c.Fix = "fix the cause, then backsight retain retry-quarantine"
	case size >= warnAt:
		c.Status, c.Fix = Warn, "the server has not accepted notes for a while: backsight server status"
	}
	return c
}

// checkMaintenance reports the maintenance lock (§11.4): owner when held,
// and a stale lock as such, never removed here.
func checkMaintenance(d paths.Dirs) Check {
	c := Check{ID: "maintenance", Status: OK, Summary: "no maintenance in progress"}
	// Inspected, never taken: the daily snapshot must not pause the hooks.
	switch held := state.Inspect(d.MaintenanceLock()); {
	case held == nil:
	case held.Stale:
		c.Status, c.Summary, c.Detail = Warn, "stale maintenance lock", held.Error()
		c.Fix = "check that no backsight update, backup or restore is running, then remove " + held.Path
	default:
		c.Status, c.Summary = Info, fmt.Sprintf("maintenance in progress: %s (pid %d, since %s)", held.Owner.Command, held.Owner.PID, held.Owner.Since.Format(time.RFC3339))
	}
	return c
}

// checkRestore reports a restore that did not finish (its marker, decision
// 0044): every session and backfill send holds until it is gone, and the
// server may be the old one, the restored one or none.
func checkRestore(d paths.Dirs) Check {
	c := Check{ID: "restore", Status: OK, Summary: "no restore in progress"}
	m, err := state.ReadRestoreMarker(d)
	switch {
	case m == nil:
		return c
	case err != nil:
		c.Status, c.Summary, c.Detail = Error, "a restore did not finish: sessions are held", err.Error()
		c.Fix = "check the server (backsight server status) and the newest safety archive (backsight backup list), restore it, then remove " + d.RestoreMarker()
		return c
	}
	c.Status = Error
	c.Summary = fmt.Sprintf("a restore did not finish (%s, started %s): sessions are held", m.Command, m.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
	if m.Step == state.PurgeStep {
		c.Summary = fmt.Sprintf("a restore (%s, started %s) wrote its epochs and waits for queued retains before purging again: sessions are held",
			m.Command, m.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
		c.Detail = "a queued retain would recreate a purged document of bank(s) " + strings.Join(m.PurgeBanks, ", ") + "; the archive restored is " + m.Archive +
			"; the server as it was before is in the safety archive " + m.Safety
		if len(m.Stuck) > 0 {
			c.Summary = fmt.Sprintf("a restore (%s, started %s) stopped waiting at its purge step: queued operations did not move: sessions are held",
				m.Command, m.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
			c.Detail = "stuck: " + strings.Join(m.Stuck, "; ") + " (Hindsight 1.1.0 runs again only the operations its own worker left processing); " + c.Detail
		}
		c.Fix = "backsight server start (it waits for the queues, purges again, then starts the server on its API key)"
		if m.Safety != "" {
			c.Fix += ", or else backsight restore " + m.Safety + " --no-safety (back to the server as it was)"
		}
		return c
	}
	if m.Step == state.KeyStep {
		c.Summary = fmt.Sprintf("a restore (%s, started %s) wrote its epoch but did not start the server again on its API key: sessions are held",
			m.Command, m.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
		c.Detail = "the archive restored is " + m.Archive + "; the server as it was before is in the safety archive " + m.Safety
		if m.RolledBack {
			c.Detail = "the " + m.Command + " was rolled back: the server holds the safety archive " + m.Safety + " again"
		}
		if m.Image != "" {
			c.Detail += "; its data is for " + m.Image
		}
		c.Fix = "backsight server start (it finishes the " + m.Command + " on that image)"
		if m.Safety != "" {
			c.Fix += ", or else backsight restore " + m.Safety + " --no-safety"
		}
		return c
	}
	c.Detail = "the server as it was before is in the safety archive " + m.Safety + "; the archive being restored is " + m.Archive
	if m.Safety == "" {
		c.Detail = "no safety archive was made; the archive being restored is " + m.Archive
		c.Fix = "rerun backsight restore " + m.Archive
		return c
	}
	c.Fix = "backsight restore " + m.Safety + " --no-safety (back to the server as it was), or rerun backsight restore " + m.Archive
	return c
}

// checkLocks reports locks left by a process that is gone (a crash, an OOM
// kill); the maintenance lock has its own check. The next run of the
// command takes its lock over (state.Acquire): one that stays is worth a look.
func checkLocks(d paths.Dirs) Check {
	c := Check{ID: "locks", Status: OK, Summary: "no stale lock"}
	files, _ := filepath.Glob(filepath.Join(d.LocksDir(), "*.lock"))
	var stale []string
	for _, p := range files {
		if h := state.Inspect(p); h != nil && h.Stale {
			stale = append(stale, fmt.Sprintf("%s (%s, pid %d)", filepath.Base(p), h.Owner.Command, h.Owner.PID))
		}
	}
	if len(stale) > 0 {
		c.Status, c.Summary, c.Detail = Warn, fmt.Sprintf("%d stale lock(s) left by a process that is gone", len(stale)), strings.Join(stale, "; ")
		c.Fix = "the next run of that command takes its lock over; if one stays, check no backsight process runs and remove it from " + d.LocksDir()
	}
	return c
}

// checkBackfill reports the queued backfill: its progress while jobs tick
// sends it, and a pause, which only a rerun lifts.
func checkBackfill(d paths.Dirs) Check {
	c := Check{ID: "backfill", Status: OK, Summary: "no backfill pending"}
	st, err := state.Load(d.StateFile())
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot read the backfill state", err.Error()
		return c
	}
	b := st.Backfill
	switch {
	case b == nil:
	case len(b.Sessions) == 0:
		c.Summary = fmt.Sprintf("backfill finished (%d/%d)", b.Done, b.Total)
	case b.Paused == "budget":
		c.Status, c.Summary = Warn, fmt.Sprintf("backfill paused at %d/%d: budget reached", b.Done, b.Total)
		c.Detail = fmt.Sprintf("spent %.4f %s, %d tokens (the bank's whole extraction since it started)", b.SpentCost, b.Currency, b.SpentTokens)
		c.Fix = "backsight retain backfill --plan, then backsight retain backfill --max-cost <higher>"
	case b.Paused != "":
		c.Status, c.Summary = Warn, fmt.Sprintf("backfill paused at %d/%d: %s", b.Done, b.Total, b.Paused)
		c.Fix = "backsight retain backfill"
	default:
		c.Status, c.Summary = Info, fmt.Sprintf("backfill in progress: %d/%d, sent by jobs tick", b.Done, b.Total)
	}
	return c
}

func checkDisk(env Env) Check {
	c := Check{ID: "disk", Status: OK}
	var parts []string
	for _, dir := range []string{env.Dirs.Data, env.Dirs.State} {
		probe := existingParent(dir)
		free, err := env.FreeBytes(probe)
		if err != nil {
			c.Status, c.Summary, c.Detail = Info, "free space unknown", err.Error()
			return c
		}
		parts = append(parts, fmt.Sprintf("%s: %.1f GB free", dir, float64(free)/1e9))
		switch {
		case free < diskError:
			c.Status = Error
		case free < diskWarn && c.Status == OK:
			c.Status = Warn
		}
	}
	c.Summary = strings.Join(parts, "; ")
	if c.Status != OK {
		c.Fix = "free disk space: outboxes, backups and the server's database grow there"
	}
	return c
}

// existingParent is dir or its nearest existing ancestor (before setup the
// directories do not exist yet).
func existingParent(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

func checkUntagged(ctx context.Context, cl *hindsight.Client, bank string) Check {
	c := Check{ID: "untagged"}
	total, untagged := 0, 0
	err := cl.EachMemory(ctx, bank, "observation", "", func(m hindsight.Memory) error {
		total++
		for _, tag := range m.Tags {
			if strings.HasPrefix(tag, "project:") {
				return nil
			}
		}
		untagged++
		return nil
	})
	switch {
	case err != nil:
		c.Status, c.Summary, c.Detail = Error, "cannot list observations", err.Error()
	case untagged > 0:
		c.Status, c.Summary = Warn, fmt.Sprintf("%d of %d observations have no project tag", untagged, total)
		c.Detail = "untagged observations are invisible to project recall: it searches by project tag"
		c.Fix = "report it upstream (backsight report); a fresh backsight bank prevents it for its own writes"
	default:
		c.Status, c.Summary = OK, fmt.Sprintf("every observation has a project tag (%d)", total)
	}
	return c
}

func checkReference(in *reference.Install, err error) Check {
	c := Check{ID: "reference"}
	switch {
	case err != nil:
		c.Status, c.Summary, c.Detail = Warn, "cannot read the Claude Code files", err.Error()
	case in == nil || len(in.Pieces) == 0:
		c.Status, c.Summary = OK, "no older Hindsight install wired into Claude Code"
		if in != nil && len(in.Leftovers) > 0 {
			c.Detail = "left on disk: " + pieceList(in.Leftovers)
		}
	default:
		c.Status = Info
		c.Summary = fmt.Sprintf("%d piece(s) of an older Hindsight install wired into Claude Code: backsight client retire unwires them", len(in.Pieces))
		c.Detail = pieceList(in.Pieces)
		if len(in.Leftovers) > 0 {
			c.Detail += "; left on disk: " + pieceList(in.Leftovers)
		}
	}
	return c
}

func pieceList(ps []reference.Piece) string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.ID + " (" + p.Where + ")"
	}
	return strings.Join(lines, "; ")
}

func checkRetention(loc claudecode.Location, managedDir string) Check {
	c := Check{ID: "retention"}
	ret, err := claudecode.ReadRetention(loc, managedDir)
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot read the transcript retention", err.Error()
		return c
	}
	tr, scanErr := claudecode.ScanTranscripts(loc.Projects)
	c.Summary = fmt.Sprintf("Claude Code keeps transcripts %d days (%s)", ret.EffectiveDays, ret.Source)
	c.Detail = fmt.Sprintf("%d transcript(s), %.1f MB", tr.Count, float64(tr.Bytes)/(1<<20))
	if !tr.Oldest.IsZero() {
		c.Detail += ", oldest " + tr.Oldest.UTC().Format("2006-01-02")
	}
	if scanErr != nil {
		c.Detail += "; transcripts not counted: " + scanErr.Error()
	}
	if ret.EffectiveDays >= retentionFloor {
		c.Status = OK
		return c
	}
	c.Status = Warn
	c.Detail += "; transcripts are the only source for backfill and memory rebuilds"
	if ret.Source == "managed" {
		c.Fix = "the value is set by policy in " + managedDir + ": ask your administrator"
	} else {
		c.Fix = "backsight client retention (keeps transcripts 3650 days; backs up " + loc.Settings + " first)"
	}
	return c
}

// checkClient compares Claude Code's wiring with what client install
// recorded (SPEC §5 drift, §15).
func checkClient(env Env, loc claudecode.Location) Check {
	c := Check{ID: "client"}
	st, err := state.Load(env.Dirs.StateFile())
	if err != nil || st.PluginVersion == "" {
		c.Status, c.Summary = Info, "backsight is not wired into Claude Code yet"
		c.Fix = "backsight client install"
		return c
	}
	// The profile client install wired, not the shell's (0048: the dev
	// install lives in its own profile).
	profile, fix := "", "backsight client install"
	if st.Client != nil && st.Client.Profile != "" {
		profile = st.Client.Profile
		fix += " --config-dir " + profile
		loc = claudecode.Locate(env.Home, func(k string) string {
			if k == "CLAUDE_CONFIG_DIR" {
				return profile
			}
			return env.Getenv(k)
		})
	}
	drift := plugin.Drift(env.Dirs.Data, loc.Settings, plugin.Installed{Version: st.PluginVersion, ShimTarget: st.ShimTarget}, buildinfo.Version)
	if env.ClaudePlugins != nil {
		if raw, err := env.ClaudePlugins(profile); err == nil {
			var items []struct {
				ID      string `json:"id"`
				Enabled bool   `json:"enabled"`
			}
			if json.Unmarshal(raw, &items) == nil {
				found := false
				for _, it := range items {
					if it.ID == plugin.PluginID() {
						found = true
						if !it.Enabled {
							drift = append(drift, "the plugin is disabled in Claude Code")
						}
					}
				}
				if !found {
					drift = append(drift, "the plugin is not installed in Claude Code")
				}
			}
		}
	}
	if len(drift) == 0 {
		c.Status, c.Summary = OK, "plugin "+st.PluginVersion+" and permission rule in place in "+loc.Dir
		return c
	}
	c.Status, c.Summary, c.Detail, c.Fix = Warn, "Claude Code wiring drifted", strings.Join(drift, "; "), fix
	return c
}

func checkGPU(gpus []string) Check {
	c := Check{ID: "gpu", Status: Info}
	if len(gpus) == 0 {
		c.Summary = "no GPU detected; backsight runs on the CPU"
		return c
	}
	c.Summary = strings.Join(gpus, ", ") + "; not used: backsight runs on the CPU"
	return c
}

// detectGPUs reports devices only; nothing is configured (decision 0035).
func detectGPUs(goos, goarch, devDir string) []string {
	if goos == "darwin" && goarch == "arm64" {
		return []string{"Apple GPU (Metal)"}
	}
	if goos != "linux" {
		return nil
	}
	var out []string
	if _, err := os.Stat(filepath.Join(devDir, "kfd")); err == nil {
		out = append(out, "AMD ROCm (/dev/kfd)")
	}
	if _, err := os.Stat(filepath.Join(devDir, "nvidiactl")); err == nil {
		out = append(out, "NVIDIA (/dev/nvidiactl)")
	}
	if nodes, _ := filepath.Glob(filepath.Join(devDir, "dri", "renderD*")); len(nodes) > 0 {
		out = append(out, "render node "+filepath.Base(nodes[0])+" (Vulkan)")
	}
	return out
}

// LocalGPUs probes this machine's devices.
func LocalGPUs() []string { return detectGPUs(runtime.GOOS, runtime.GOARCH, "/dev") }
