package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/guide"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/migrate"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/selfcheck"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/signing"
	"github.com/Barben360/backsight/internal/state"
)

// update apply (SPEC §11.7, decision 0067 block 2) is driven by the
// running binary, and every step that needs the new version's code runs
// the new binary itself: its migrations (`update migrate`) and its plugin
// (`client install`). The old binary never needs to know what the new one
// does; it only verifies, installs, switches the shim and, on any failure,
// puts back the shim, state.json, config.toml and the previous binary's
// plugin (through that binary's own `client install`).

// binaryRunner runs another backsight binary (the release being
// installed, or the previous one on a rollback); tests replace it.
type binaryRunner interface {
	// Run returns the child's stdout, stderr and exit code; err only when
	// it could not run at all.
	Run(ctx context.Context, exe string, args ...string) (stdout, stderr []byte, code int, err error)
}

// execBinary runs the binary; Progress, when set, also gets its stderr as
// it comes (the child's JSON progress lines, with update apply --json).
type execBinary struct{ Progress io.Writer }

// childWaitDelay is how long a cancelled child has to finish once asked to
// stop: a server upgrade's own rollback (the systemd units' TimeoutStopSec
// covers it). Then it is killed.
var childWaitDelay = 30 * time.Minute

func (b execBinary) Run(ctx context.Context, exe string, args ...string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	// In its own process group: a Ctrl-C at the terminal reaches update
	// apply only. A cancel asks the child's group to stop (SIGTERM), so the
	// child rolls itself back, and is waited for.
	server.OwnGroup(cmd)
	cmd.Cancel = func() error { return server.TerminateGroup(cmd.Process) }
	cmd.WaitDelay = childWaitDelay
	if updateLock != nil {
		cmd.Env = append(os.Environ(), updateLockEnv+"="+lockToken(*updateLock))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if b.Progress != nil {
		cmd.Stderr = io.MultiWriter(&stderr, b.Progress)
	}
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() >= 0 {
		return stdout.Bytes(), stderr.Bytes(), ee.ExitCode(), nil
	}
	if err != nil {
		return stdout.Bytes(), stderr.Bytes(), -1, err
	}
	return stdout.Bytes(), stderr.Bytes(), 0, nil
}

var binaryExec binaryRunner = execBinary{}

// migrations is this binary's migration registry; tests replace it.
var migrations = migrate.All

// childEnvelope is the --json envelope of another backsight binary.
type childEnvelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *output.Error   `json:"error"`
}

// childError is a child that exited with another code than 0: its code
// and its envelope's error (needs_user actions included), when it printed
// one.
type childError struct {
	cmdline string
	code    int
	env     *output.Error
	msg     string
}

func (e *childError) Error() string {
	if e.code == 0 {
		return e.cmdline + ": unexpected output: " + e.msg
	}
	return fmt.Sprintf("%s: exit %d: %s", e.cmdline, e.code, e.msg)
}

// runChild runs exe with args and --json; any exit but 0 is a *childError
// carrying the child's code and message (or its stderr's end when it
// printed no envelope).
func runChild(ctx context.Context, exe string, args ...string) (childEnvelope, error) {
	stdout, stderr, code, err := binaryExec.Run(ctx, exe, append(args, "--json")...)
	cmdline := exe + " " + strings.Join(args, " ")
	if err != nil {
		return childEnvelope{}, fmt.Errorf("%s: %w", cmdline, err)
	}
	var env childEnvelope
	perr := json.Unmarshal(stdout, &env)
	if code == 0 && perr == nil {
		return env, nil
	}
	ce := &childError{cmdline: cmdline, code: code}
	if perr == nil && env.Error != nil {
		ce.env, ce.msg = env.Error, env.Error.Message
	} else {
		ce.msg = tail(string(stderr)+string(stdout), 400)
		if code == 0 {
			ce.msg = perr.Error() + ": " + ce.msg
		}
	}
	return env, ce
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

type updateApplyFlags struct {
	plan            bool
	version, source string
	auto            bool // jobs maintain's auto-apply, not the user
	allowUnordered  bool // passed to the new binary's server upgrade
}

// serverUpgrade is the new binary's server upgrade command line.
func (fl updateApplyFlags) serverUpgrade(args ...string) []string {
	args = append([]string{"server", "upgrade"}, args...)
	if fl.allowUnordered {
		args = append(args, "--allow-unordered")
	}
	return args
}

type updateApplyResult struct {
	Installed  string          `json:"installed"`
	Target     string          `json:"target,omitempty"`
	Source     string          `json:"source"`
	Plan       bool            `json:"plan"`
	UpToDate   bool            `json:"up_to_date"`
	Assets     []release.Asset `json:"assets,omitempty"`
	KeyID      string          `json:"key_id,omitempty"` // the key that signed checksums.txt
	Binary     string          `json:"binary,omitempty"` // where the new binary is (or would be) installed
	Previous   string          `json:"previous,omitempty"`
	Migrations []string        `json:"migrations"` // applied, or pending with --plan
	// IndexMigrations: the metrics index's, applied or pending with --plan
	// (the text says how many, never their ids).
	IndexMigrations []string      `json:"index_migrations,omitempty"`
	Plugin          string        `json:"plugin,omitempty"`
	Server          *updateServer `json:"server,omitempty"`
	Steps           []step        `json:"steps"`
	RolledBack      bool          `json:"rolled_back,omitempty"`
	Resumed         bool          `json:"resumed,omitempty"` // an unfinished update to this binary was finished
	Warnings        []string      `json:"warnings,omitempty"`
	MS              int           `json:"ms"`
	changed         bool          // the update went past its install (the shim step started)
}

// updateServer is the server step: the version deployed and the new
// binary's pin.
type updateServer struct {
	Current  string `json:"current"`
	Target   string `json:"target"`
	Upgrade  bool   `json:"upgrade"`
	Upgraded bool   `json:"upgraded"`
	Safety   string `json:"safety_archive,omitempty"` // the server as it was, from the new binary's upgrade
}

// serverStepSkipped says why update apply has no server to upgrade here:
// none configured, a remote one (upgraded on its host), or none deployed.
func serverStepSkipped(d paths.Dirs, cfg config.Config) string {
	if _, err := os.Stat(d.ConfigFile()); err != nil {
		return "no server configured on this machine"
	}
	if cfg.Server.Mode != "local" {
		return "the server is remote: update backsight on its host"
	}
	if deployed, err := runtimeFor(cfg).Deployed(); err != nil || deployed == "" {
		return "no server deployed on this machine"
	}
	return ""
}

func newUpdateApplyCmd(e *env) *cobra.Command {
	var fl updateApplyFlags
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Install a newer release: verify, install, switch the shim, migrate, update the plugin; roll back on any failure",
		Long: "Apply downloads the release archive with checksums.txt and its minisign signature and verifies both (nothing changes when they " +
			"do not verify: exit 20), then takes the maintenance lock, installs the binary under <data>/bin/ with its checksums, switches the " +
			"shim, and runs the new binary's pending client migrations, its client install (the plugin, then claude plugin update) and its " +
			"server upgrade. Any failure before the server upgrade passed puts back state.json, config.toml, the server, the shim and the " +
			"previous plugin, and exits 30. Once it passed the update is committed: a record that fails exits 40, and a rerun of the new " +
			"binary finishes it, as it resumes any update to itself that did not finish. --plan downloads and verifies into a temporary " +
			"directory, asks the new binary for its pending migrations and its server plan, and changes nothing.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fl.version != "" {
				if _, err := release.Parse(fl.version); err != nil {
					return usageError{fmt.Errorf("--version: %w", err)}
				}
			}
			start := time.Now()
			if b, ok := binaryExec.(execBinary); ok && e.json { // the new binary's progress lines pass through
				b.Progress = e.stderr
				binaryExec = b
				defer func() { binaryExec = execBinary{} }()
			}
			res := updateApplyResult{Installed: buildinfo.Version, Plan: fl.plan, Steps: []step{}, Migrations: []string{}}
			err := runUpdateApply(cmd.Context(), &res, fl, streamStep(e, "update apply", &res.Steps))
			res.MS = int(time.Since(start).Milliseconds())
			return e.report("update apply", res, func() string { return updateApplyText(res) }, err)
		},
	}
	cmd.Flags().BoolVar(&fl.plan, "plan", false, "download and verify into a temporary directory, list the steps, change nothing")
	cmd.Flags().StringVar(&fl.version, "version", "", "install this version (default: the newest under the pre-release policy)")
	cmd.Flags().StringVar(&fl.source, "source", "", "read releases from this local release directory instead of the configured source")
	cmd.Flags().BoolVar(&fl.allowUnordered, "allow-unordered", false, "let the new binary's server upgrade deploy a pin it cannot order against the deployed version")
	return cmd
}

// streamStep records a step and shows it as it happens: a text line, or a
// JSON progress line on stderr with --json (decision 0064).
func streamStep(e *env, command string, steps *[]step) func(name, status, detail string) {
	return func(name, status, detail string) {
		st := step{name, status, detail}
		*steps = append(*steps, st)
		if !e.json {
			fmt.Fprint(e.stdout, stepLine(st))
			return
		}
		b, _ := json.Marshal(map[string]any{"event": "progress", "command": command, "step": name, "status": status, "detail": detail})
		fmt.Fprintln(e.stderr, string(b))
	}
}

func newUpdateMigrateCmd(e *env) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:    "migrate",
		Short:  "Run this binary's pending client migrations (update apply runs it from the new binary)",
		Hidden: true,
		Args:   usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, _, err := machine()
			if err != nil {
				return err
			}
			list := migrations()
			if err := migrate.Validate(list); err != nil {
				return blocked(err)
			}
			st, err := state.Load(d.StateFile())
			if err != nil {
				return blocked(err)
			}
			res := struct {
				Plan    bool     `json:"plan"`
				Pending []string `json:"pending"`
				Applied []string `json:"applied"`
			}{Plan: plan, Pending: []string{}, Applied: []string{}}
			for _, m := range migrate.Pending(list, st.AppliedMigrations) {
				res.Pending = append(res.Pending, m.ID)
			}
			text := func() string {
				if plan {
					return fmt.Sprintf("pending migrations: %v\n", res.Pending)
				}
				return fmt.Sprintf("applied migrations: %v\n", res.Applied)
			}
			if plan || len(res.Pending) == 0 {
				return e.emit("update migrate", res, text)
			}
			unlock, _, err := maintenanceFor(d, "update migrate")
			if err != nil {
				return err
			}
			defer unlock()
			ran, err := migrate.Apply(cmd.Context(), migrate.Env{Dirs: d}, list)
			res.Applied = append(res.Applied, ran...)
			if err != nil {
				err = blocked(err)
			}
			return e.report("update migrate", res, text, err)
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "list the pending migrations without running them")
	return cmd
}

// maintenanceFor takes the maintenance lock for command, unless update
// apply holds it in this process or in the parent that runs this binary:
// the lock is then held for the whole update and is not released here.
func maintenanceFor(d paths.Dirs, command string) (release func(), note string, err error) {
	if h := state.Inspect(d.MaintenanceLock()); h != nil && !h.Stale && heldByUpdate(h) {
		return func() {}, "", nil
	}
	l, note, err := acquireMaintenance(d, command)
	if err != nil {
		return nil, "", err
	}
	return func() { _ = l.Release() }, note, nil
}

// pickRelease is the release to install: --version, else the newest one
// under the pre-release policy; nil when there is nothing newer than
// installed (the version the shim runs, not the binary running this).
func pickRelease(rels []release.Release, installed, want string, optIn bool) (*release.Release, error) {
	if want != "" {
		r, ok := release.Find(rels, want)
		if !ok {
			return nil, &output.Error{Code: output.Blocked, Message: "the release source has no version " + want + ": nothing was changed"}
		}
		if !release.NewerThan(r.Version, installed) {
			return nil, &output.Error{Code: output.Usage, Message: fmt.Sprintf("%s is not newer than the installed %s: update apply never downgrades", r.Version, installed)}
		}
		return &r, nil
	}
	newer := release.Newer(rels, installed, release.WantPrereleases(installed, optIn))
	if len(newer) == 0 {
		return nil, nil
	}
	return &newer[0], nil
}

// nothingChanged is a refusal before anything was changed: exit 20.
func nothingChanged(format string, a ...any) error {
	return &output.Error{Code: output.Blocked, Message: fmt.Sprintf(format, a...) + ": nothing was changed"}
}

// binaryDir matches <version>-<hash12>, the directory binaryPath installs
// a build into.
var binaryDir = regexp.MustCompile(`^(.+)-[0-9a-f]{12}$`)

// installedVersion is the version of the binary at target (the shim's),
// read from its directory; the running binary's when it does not say.
func installedVersion(target string) string {
	if m := binaryDir.FindStringSubmatch(filepath.Base(filepath.Dir(target))); m != nil {
		return m[1]
	}
	return buildinfo.Version
}

// recordWait bounds the wait for state.json's lock (a tick or a hook
// writing it) when update apply records the new binary or puts the
// previous state back.
var recordWait = 30 * time.Second

func runUpdateApply(ctx context.Context, res *updateApplyResult, fl updateApplyFlags, add func(name, status, detail string)) error {
	d, cfg, err := updateConfig()
	if err != nil {
		return err
	}
	src := releaseSource(cfg, fl.source)
	res.Source = src.Name()

	// The install being updated: the binary the shim runs.
	shim := filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS))
	st, err := state.Load(d.StateFile())
	if err != nil {
		return blocked(err)
	}
	previous := st.ShimTarget
	if runtime.GOOS != "windows" {
		if real, err := filepath.EvalSymlinks(shim); err == nil {
			previous = real
		}
	}
	if previous == "" {
		return &output.Error{Code: output.NeedsUser, Message: "no installed backsight (no shim at " + shim + "): run backsight setup first",
			Actions: []output.Action{{Kind: "run_as_user", Cmd: "backsight setup", Why: "update apply updates an install, and there is none here"}}}
	}
	res.Installed = installedVersion(previous)
	if state.RestoreIncomplete(d) {
		return nothingChanged("a restore did not finish (%s): run backsight doctor for the way back", d.RestoreMarker())
	}
	if held := state.Inspect(d.MaintenanceLock()); held != nil && !held.Stale {
		return nothingChanged("maintenance in progress: %s", held.Error())
	}

	// An update that did not finish: resumed when this binary is its
	// target, else replaced from the install it started from (its marker
	// knows it; the shim may run the half-installed binary).
	prev, merr := state.ReadUpdateMarker(d)
	var saved map[string][]byte
	switch {
	case prev != nil && merr != nil:
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s cannot be read (%v); this update replaces it", d.UpdateMarker(), merr))
		prev = nil
	case prev != nil:
		if prev.Previous != "" {
			previous, res.Installed = prev.Previous, prev.From
		}
		if saved, err = state.ReadUpdateSnapshot(d); err != nil {
			return nothingChanged("cannot read the unfinished update's snapshot (%v)", err)
		}
		_, wanted := release.Find([]release.Release{{Version: prev.To}}, fl.version)
		if prev.To == buildinfo.Version && (fl.version == "" || wanted) {
			if _, err := os.Stat(prev.Binary); err == nil {
				return resumeUpdate(ctx, d, cfg, st, shim, *prev, saved, res, fl, add)
			}
		}
		if prev.Committed {
			return &output.Error{Code: output.NeedsUser, Message: fmt.Sprintf("the update to %s is installed and its server upgraded, only its record is missing: finish it with %s update apply", prev.To, prev.Binary),
				Actions: []output.Action{{Kind: "run_as_user", Cmd: prev.Binary + " update apply", Why: "the new binary resumes its update; an older one must not roll it back"}}}
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf("an update from %s to %s did not finish (started %s); this one replaces it", prev.From, prev.To, prev.StartedAt.Format(time.RFC3339)))
	}
	res.Previous = previous

	rels, err := src.Releases(ctx)
	if err != nil {
		return sourceError(err)
	}
	rel, err := pickRelease(rels, res.Installed, fl.version, cfg.Update.Prereleases)
	if err != nil {
		return err
	}
	if rel == nil {
		res.UpToDate = true
		add("check", "done", "backsight "+res.Installed+" is up to date on "+res.Source)
		return nil
	}
	res.Target = rel.Version
	archive, ok := rel.Archive(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return nothingChanged("release %s has no archive for %s/%s", rel.Version, runtime.GOOS, runtime.GOARCH)
	}
	for _, name := range []string{selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
		if _, ok := rel.Asset(name); !ok {
			return nothingChanged("release %s has no %s: it cannot be verified", rel.Version, name)
		}
	}
	add("check", "done", fmt.Sprintf("%s → %s on %s", res.Installed, rel.Version, res.Source))

	if err := os.MkdirAll(d.State, 0o700); err != nil {
		return blocked(err)
	}
	work, err := os.MkdirTemp(d.State, "update-")
	if err != nil {
		return blocked(err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	// 1. Download and verify, before the lock (hooks keep sending while a
	// slow download runs): a failure here changed nothing.
	staged, err := fetchVerified(ctx, src, *rel, archive.Name, work, res, add)
	if err != nil {
		return err
	}
	hash, err := fsutil.HashFile(staged)
	if err != nil {
		return blocked(err)
	}
	target := binaryPath(d.Data, rel.Version, hash)
	res.Binary = target
	a := newApplyRun(d, cfg, st, shim, previous, target, res, add)
	a.fl = fl

	if fl.plan {
		return a.plan(ctx, staged)
	}

	// The lock, held to the end: hooks only queue, jobs skip, no backup
	// or restore starts.
	unlock, err := takeUpdateLock(d, res)
	if err != nil {
		return err
	}
	defer unlock()
	removeStaleStaging(d, work)
	if state.RestoreIncomplete(d) {
		return nothingChanged("a restore did not finish (%s): run backsight doctor for the way back", d.RestoreMarker())
	}
	if !fl.auto {
		clearAutoApply(d)
	}

	// 2. Install the binary next to its signed checksums (the self-check).
	created := false
	if _, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) {
		created = true
	}
	for name, mode := range map[string]os.FileMode{"": 0o755, selfcheck.ChecksumsFile: 0o644, selfcheck.SignatureFile: 0o644} {
		from, to := staged, target
		if name != "" {
			from, to = filepath.Join(work, name), filepath.Join(filepath.Dir(target), name)
		}
		if err := copyFile(from, to, mode); err != nil {
			if created && target != previous {
				_ = os.RemoveAll(filepath.Dir(target))
			}
			return nothingChanged("cannot install %s (%v)", target, err)
		}
	}
	add("install", "done", target)

	// From here a failure rolls back, from a snapshot that outlives this
	// process: state.json and config.toml before the update (the unfinished
	// one's, when this one replaces it).
	// The unfinished update's binary is this one's: it goes with a
	// rollback when that update installed it.
	a.rb.removeTarget = (created || (prev != nil && prev.RemoveBinary && prev.Binary == target)) && target != previous
	if saved == nil {
		if saved, err = takeSnapshot(d); err != nil {
			a.rb.removeBinary()
			return nothingChanged("cannot save state.json and config.toml before the update (%v)", err)
		}
	}
	a.rb.saved = saved
	a.marker = state.UpdateMarker{From: res.Installed, To: rel.Version, Previous: previous, Binary: target, WayBack: a.rb.wayBack,
		StartedAt: now().UTC().Truncate(time.Second), RemoveBinary: a.rb.removeTarget}
	if err := state.WriteUpdateSnapshot(d, saved); err != nil {
		a.rb.removeBinary()
		return nothingChanged("cannot save state.json and config.toml before the update (%v)", err)
	}
	if err := state.WriteUpdateMarker(d, a.marker); err != nil {
		a.rb.removeBinary()
		_ = state.ClearUpdateMarker(d)
		return nothingChanged("cannot mark the update in progress (%v)", err)
	}
	res.changed = true
	return a.steps(ctx)
}

// takeUpdateLock takes the maintenance lock for update apply and names it
// for the binaries it runs (heldByUpdate).
func takeUpdateLock(d paths.Dirs, res *updateApplyResult) (func(), error) {
	l, note, err := acquireMaintenance(d, "update apply")
	if err != nil {
		return nil, err
	}
	if note != "" {
		res.Warnings = append(res.Warnings, note)
	}
	o := l.Owner()
	updateLock = &o
	return func() { updateLock = nil; _ = l.Release() }, nil
}

// resumeUpdate finishes an update this binary is the target of, killed
// half way or stopped at its record (exit 40): the steps after the
// install run again (each is idempotent), rolling back to the marker's
// previous install from the persisted snapshot until the commit point.
func resumeUpdate(ctx context.Context, d paths.Dirs, cfg config.Config, st state.State, shim string, m state.UpdateMarker, saved map[string][]byte, res *updateApplyResult, fl updateApplyFlags, add func(name, status, detail string)) error {
	res.Installed, res.Target, res.Binary, res.Previous, res.Resumed = m.From, m.To, m.Binary, m.Previous, true
	add("check", "done", fmt.Sprintf("resuming the update from %s to %s started %s", m.From, m.To, m.StartedAt.Format(time.RFC3339)))
	a := newApplyRun(d, cfg, st, shim, m.Previous, m.Binary, res, add)
	a.fl, a.marker, a.rb.removeTarget, a.rb.committed = fl, m, m.RemoveBinary, m.Committed
	if fl.plan {
		add("resume", "planned", "would run the steps after the install again as "+m.Binary+": shim, migrations, plugin, server, record")
		return nil
	}
	unlock, err := takeUpdateLock(d, res)
	if err != nil {
		return err
	}
	defer unlock()
	if state.RestoreIncomplete(d) {
		return nothingChanged("a restore did not finish (%s): run backsight doctor for the way back", d.RestoreMarker())
	}
	if !fl.auto {
		clearAutoApply(d)
	}
	if saved == nil && !m.Committed {
		// A marker from before snapshots were kept: the files as they are
		// now are the best way back there is.
		if saved, err = takeSnapshot(d); err != nil {
			return nothingChanged("cannot save state.json and config.toml (%v)", err)
		}
		res.Warnings = append(res.Warnings, "the unfinished update kept no snapshot of state.json and config.toml: a rollback puts back the current ones")
	}
	a.rb.saved = saved
	res.changed = true
	return a.steps(ctx)
}

// applyRun is one update past its install: the steps the new binary runs
// and the rollback they share.
type applyRun struct {
	d           paths.Dirs
	cfg         config.Config
	shim        string
	target      string
	clientArgs  []string
	clientWired bool
	marker      state.UpdateMarker
	rb          *updateRollback
	fl          updateApplyFlags
	res         *updateApplyResult
	add         func(name, status, detail string)
}

func newApplyRun(d paths.Dirs, cfg config.Config, st state.State, shim, previous, target string, res *updateApplyResult, add func(name, status, detail string)) *applyRun {
	clientArgs := []string{"client", "install"}
	if st.Client != nil && st.Client.Profile != "" {
		clientArgs = append(clientArgs, "--config-dir", st.Client.Profile)
	}
	wayBack := previous + " " + strings.Join(clientArgs, " ")
	return &applyRun{d: d, cfg: cfg, shim: shim, target: target, clientArgs: clientArgs, clientWired: st.PluginVersion != "", res: res, add: add,
		rb: &updateRollback{d: d, shim: shim, previous: previous, target: target, clientArgs: clientArgs, wayBack: wayBack, add: add, res: res}}
}

// plan asks the staged binary what it would do; nothing changes.
func (a *applyRun) plan(ctx context.Context, staged string) error {
	res, add := a.res, a.add
	env, err := runChild(ctx, staged, "update", "migrate", "--plan")
	if err != nil {
		return nothingChanged("the new binary cannot list its migrations (%v)", err)
	}
	var mr struct {
		Pending []string `json:"pending"`
	}
	_ = json.Unmarshal(env.Data, &mr)
	res.Migrations = append(res.Migrations, mr.Pending...)
	add("install", "planned", "would install "+a.target+" with "+selfcheck.ChecksumsFile+" and its signature")
	add("shim", "planned", "would switch "+a.shim+" from "+a.rb.previous+" to "+a.target)
	add("migrations", "planned", migrationsText(res.Migrations, "would run"))
	if a.clientWired {
		res.Plugin = "planned"
		add("plugin", "planned", "would run the new binary's "+strings.Join(a.clientArgs, " ")+" (plugin regenerated, claude plugin update)")
	} else {
		res.Plugin = "skipped: Claude Code is not wired on this machine"
		add("plugin", "skipped", "Claude Code is not wired on this machine (no client install)")
	}
	a.refreshScheduler(ctx, staged, true)
	a.planIndex(ctx, staged)
	if why := serverStepSkipped(a.d, a.cfg); why != "" {
		add("server", "skipped", why)
		a.planDocs()
		return nil
	}
	senv, err := runChild(ctx, staged, a.fl.serverUpgrade("--plan")...)
	if err != nil {
		return nothingChanged("the new binary cannot plan the server upgrade (%v)", err)
	}
	sp := decodeServerPlan(senv.Data)
	res.Server = &updateServer{Current: sp.Current, Target: sp.Target, Upgrade: sp.Upgrade}
	if sp.Upgrade {
		add("server", "planned", "would run the new binary's server upgrade: "+sp.Current+" → "+sp.Target+" (backup and verify, pull, start, health, smoke)")
	} else {
		add("server", "planned", "the new binary pins "+sp.Target+", which the server runs already")
	}
	a.planDocs()
	return nil
}

func (a *applyRun) planDocs() {
	switch {
	case !a.configured():
	case a.cfg.Server.Mode != "local":
		a.add("bank", "skipped", hostAppliesBank)
		a.add("docs", "skipped", hostIndexesDocs)
	default:
		a.add("bank", "planned", "would run the new binary's bank apply: its bank template, when it differs from the one the bank was given")
		a.add("docs", "planned", "would run the new binary's guide index: its docs into the server's docs bank (0 LLM calls)")
	}
}

// configured: this machine has a config.toml (a server, its own or one it
// is a client of).
func (a *applyRun) configured() bool {
	_, err := os.Stat(a.d.ConfigFile())
	return err == nil
}

// bank runs the new binary's bank apply (not forced): it sends the new
// binary's template when it differs from the one recorded (M5 review
// rules, "Bank template follows the binary"). Its client install left it
// to this step, after the server runs the new pin. A failure is a warning,
// never a rollback: client install and doctor name it again.
// On a client of a shared server the bank is its host's.
func (a *applyRun) bank(ctx context.Context) {
	if !a.configured() {
		a.add("bank", "skipped", "no server configured on this machine")
		return
	}
	if a.cfg.Server.Mode != "local" {
		a.add("bank", "skipped", hostAppliesBank)
		return
	}
	env, err := runChild(ctx, a.target, "bank", "apply")
	if err != nil {
		a.res.Warnings = append(a.res.Warnings, "the bank template was not applied ("+err.Error()+"): run backsight bank apply")
		a.add("bank", "failed", err.Error())
		return
	}
	var r server.BankResult
	_ = json.Unmarshal(env.Data, &r)
	a.add("bank", map[bool]string{true: "done", false: "unchanged"}[r.Sent], strings.TrimSpace(bankText(r)))
}

// steps runs the update from the shim switch on. Before the commit point
// any failure, or a cancel between steps, rolls back; after it (the server
// step passed) only the record is left, and its failure is exit 40.
func (a *applyRun) steps(ctx context.Context) error {
	res, add, target := a.res, a.add, a.target
	failed := func(name string, err error) error {
		add(name, "failed", err.Error())
		return a.rb.run(ctx, fmt.Errorf("%s: %w", name, err))
	}
	if !a.rb.committed {
		// 3. The shim.
		if err := pointShim(target, a.shim); err != nil {
			return failed("shim", err)
		}
		add("shim", "done", a.shim+" → "+target)
		if link, older := olderPathLink(a.d, a.shim); older && pointShim(a.shim, link) == nil { // the PATH command follows the shim
			add("path", "done", link+" → "+a.shim)
		}
		if err := ctx.Err(); err != nil {
			return failed("shim", err)
		}

		// 4. The new binary's migrations.
		env, err := runChild(ctx, target, "update", "migrate")
		var mr struct {
			Applied []string `json:"applied"`
		}
		_ = json.Unmarshal(env.Data, &mr)
		res.Migrations = append(res.Migrations, mr.Applied...)
		if err != nil {
			return failed("migrations", err)
		}
		add("migrations", "done", migrationsText(res.Migrations, "ran"))
		if err := ctx.Err(); err != nil {
			return failed("migrations", err)
		}

		// 5. The new binary's plugin: generated, then claude plugin update.
		if a.clientWired {
			a.rb.pluginTouched = true
			cenv, err := runChild(ctx, target, a.clientArgs...)
			if err != nil {
				return failed("plugin", err)
			}
			res.Plugin = "updated"
			add("plugin", "done", "the new binary's "+strings.Join(a.clientArgs, " ")+": plugin regenerated, claude plugin update")
			// Its scheduler step (issue 222), shown as update apply's own.
			var cr clientInstallResult
			_ = json.Unmarshal(cenv.Data, &cr)
			for _, st := range cr.Steps {
				if st.Name == "scheduler" {
					a.schedulerStep(st.Status, st.Detail)
				}
			}
		} else {
			res.Plugin = "skipped: Claude Code is not wired on this machine"
			add("plugin", "skipped", "Claude Code is not wired on this machine (no client install)")
			a.refreshScheduler(ctx, target, false)
		}
		if err := ctx.Err(); err != nil {
			return failed("plugin", err)
		}

		// 6. The server, when the new binary pins another version than the
		// one deployed: the new binary owns its pin, so it runs the upgrade
		// (and its own rollback to the version deployed before).
		if err := a.server(ctx); err != nil {
			return failed("server", err)
		}

		// The commit point: every step passed, the server (if any) runs the
		// new pin. From here the update is never rolled back.
		a.rb.committed, a.marker.Committed = true, true
		_ = state.WriteUpdateMarker(a.d, a.marker)

		// 7. The new binary's bank template, docs and metrics index (M5
		// review rules, decisions 0069 and 0079): warnings, never a
		// rollback. The index migrates only now: a rollback never meets a
		// migrated index.
		a.bank(ctx)
		a.docs(ctx)
		a.metricsIndex(ctx)
	}

	// 8. Recorded: the shim's target and the one to roll back to.
	if err := state.UpdateWithin(a.d, "update apply", recordWait, func(s *state.State) error {
		if s.Client == nil {
			s.Client = &state.Client{}
		}
		s.ShimTarget, s.Client.ShimPrevious = target, a.rb.previous
		return nil
	}); err != nil {
		add("record", "failed", err.Error())
		return a.rb.run(ctx, fmt.Errorf("record: %w", err))
	}
	_ = state.ClearUpdateMarker(a.d)
	add("done", "done", "backsight "+res.Target+" installed; restart Claude Code (or /reload-plugins) to load its plugin")
	return nil
}

// docs runs the new binary's guide index: its docs into the server's docs
// bank. A failure is a warning, never a rollback: guide search answers
// with its keyword search meanwhile and refreshes the bank on first use.
// On a client of a shared server the docs bank is its host's.
func (a *applyRun) docs(ctx context.Context) {
	if !a.configured() {
		a.add("docs", "skipped", "no server configured on this machine")
		return
	}
	if a.cfg.Server.Mode != "local" {
		a.add("docs", "skipped", hostIndexesDocs)
		return
	}
	env, err := runChild(ctx, a.target, "guide", "index")
	var ce *childError
	switch {
	case errors.As(err, &ce) && ce.code == output.Usage:
		a.add("docs", "skipped", "the new binary has no guide index")
	case err != nil:
		a.res.Warnings = append(a.res.Warnings, "the docs were not indexed ("+err.Error()+"): backsight guide search answers with its keyword search meanwhile")
		a.add("docs", "failed", err.Error())
	default:
		var r guide.IndexResult
		_ = json.Unmarshal(env.Data, &r)
		a.add("docs", "done", fmt.Sprintf("%d section(s) in bank %s: %d sent, %d unchanged", r.Sections, r.Bank, r.Sent, r.Unchanged+r.Retagged))
	}
}

// indexMigrateWait bounds update apply's call to the new binary's stats
// index migrate: a lock or a disk that hangs never holds the update.
var indexMigrateWait = 60 * time.Second

// noIndexCommand: a new binary older than the metrics index (exit 2).
const noIndexCommand = "the new binary has no metrics index"

// metricsIndex runs the new binary's stats index migrate, after the
// commit point. A failure is a warning, never a rollback: the next jobs
// tick opens the index, which migrates it.
func (a *applyRun) metricsIndex(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, indexMigrateWait)
	defer cancel()
	r, err := runIndexMigrate(cctx, a.target, false)
	var ce *childError
	switch {
	case errors.As(err, &ce) && ce.code == output.Usage:
		a.add("index", "skipped", noIndexCommand)
	case err != nil:
		why := indexMigrateReason(ctx, cctx, err)
		a.res.Warnings = append(a.res.Warnings, "the metrics index was not migrated ("+why+"): the next jobs tick retries")
		a.add("index", "failed", why)
	default:
		status, detail := indexStep(r, false)
		a.res.IndexMigrations = append(a.res.IndexMigrations, r.Applied...)
		a.add("index", status, detail)
	}
}

// planIndex asks the staged binary for its pending index migrations. A
// plan never fails on the index, whose migration is only ever a warning.
func (a *applyRun) planIndex(ctx context.Context, staged string) {
	cctx, cancel := context.WithTimeout(ctx, indexMigrateWait)
	defer cancel()
	r, err := runIndexMigrate(cctx, staged, true)
	var ce *childError
	switch {
	case errors.As(err, &ce) && ce.code == output.Usage:
		a.add("index", "skipped", noIndexCommand)
	case err != nil:
		a.add("index", "skipped", "the new binary cannot list the metrics index migrations ("+indexMigrateReason(ctx, cctx, err)+")")
	default:
		status, detail := indexStep(r, true)
		a.res.IndexMigrations = append(a.res.IndexMigrations, r.Pending...)
		a.add("index", status, detail)
	}
}

// runIndexMigrate is exe's stats index migrate (with plan, --plan), its
// result decoded from its JSON envelope: its text is never relayed.
func runIndexMigrate(ctx context.Context, exe string, plan bool) (index.MigrateResult, error) {
	args := []string{"stats", "index", "migrate"}
	if plan {
		args = append(args, "--plan")
	}
	env, err := runChild(ctx, exe, args...)
	if err != nil {
		return index.MigrateResult{}, err
	}
	var r index.MigrateResult
	if err := json.Unmarshal(env.Data, &r); err != nil {
		return index.MigrateResult{}, err
	}
	return r, nil
}

// indexStep is a migrate result as the index step: how many migrations,
// never their ids.
func indexStep(r index.MigrateResult, plan bool) (status, detail string) {
	ids, status, verb := r.Applied, "done", "migrated"
	if plan {
		ids, status, verb = r.Pending, "planned", "would migrate"
	}
	switch {
	case !r.Built:
		return "skipped", "no metrics index yet"
	case r.Newer:
		return "skipped", "the metrics index is newer than this backsight"
	case len(ids) == 1:
		return status, verb + " the metrics index (1 change)"
	case len(ids) > 1:
		return status, fmt.Sprintf("%s the metrics index (%d changes)", verb, len(ids))
	case plan:
		return "skipped", "the metrics index is up to date"
	}
	return "done", "the metrics index is up to date"
}

// indexMigrateReason is why stats index migrate failed, in fixed words:
// never the child's stderr nor its error's text. Every failure exits 20
// with one of indexError's fixed sentences, which name the cause; ctx is
// update apply's, cctx the call's bound.
func indexMigrateReason(ctx, cctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return "interrupted"
	case cctx.Err() != nil:
		return "timed out"
	}
	var ce *childError
	if !errors.As(err, &ce) || ce.code != output.Blocked || ce.env == nil {
		return "failed"
	}
	switch ce.env.Message {
	case indexUpdatingSentence:
		return "an update is in progress"
	case index.ErrInUse.Error(), indexClassMessage("busy"):
		return "the index is in use"
	case indexClassMessage("full"):
		return "the disk is full"
	case indexUnreadableSentence, indexClassMessage("corrupt"), indexClassMessage("notadb"):
		return "the index is unreadable"
	case indexClassMessage("readonly"):
		return "the index is read-only"
	case indexClassMessage("io"):
		return "a disk error"
	}
	return "failed"
}

// refreshScheduler runs the new binary's jobs refresh (with plan, its
// --plan): the entries it renders are its own (issue 222). client install
// runs it too, so it is called only when Claude Code is not wired, and
// for a plan.
func (a *applyRun) refreshScheduler(ctx context.Context, exe string, plan bool) {
	args := []string{"jobs", "refresh"}
	if plan {
		args = append(args, "--plan")
	}
	env, err := runChild(ctx, exe, args...)
	if err != nil {
		if plan {
			a.add("scheduler", "skipped", "the new binary cannot plan the refresh of the scheduler entries ("+err.Error()+")")
			return
		}
		a.schedulerStep("failed", err.Error()+" (backsight jobs refresh retries)")
		return
	}
	var r jobsRefreshResult
	_ = json.Unmarshal(env.Data, &r)
	switch r.Status {
	case "none":
	case "unchanged":
		a.schedulerStep("unchanged", r.Kind+" entries up to date")
	default:
		a.schedulerStep(r.Status, refreshDetail(r))
	}
}

// schedulerStep records the scheduler refresh. Its failure is a warning,
// never a rollback: the entries run the shim whatever its version, and
// doctor flags entries left stale.
func (a *applyRun) schedulerStep(status, detail string) {
	switch status {
	case "unchanged":
		status = "skipped"
	case "failed":
		a.res.Warnings = append(a.res.Warnings, "the scheduler entries were not refreshed: "+detail)
	}
	a.add("scheduler", status, detail)
}

// server runs the new binary's server step. Its upgrade passed when it
// exits 0, or when it ended otherwise (killed after its last step, an
// envelope that does not parse) with the server on the new pin and no
// upgrade marked in progress: a server rollback re-renders nothing, it
// only ever restores the safety archive (updateRollback.serverBack).
func (a *applyRun) server(ctx context.Context) error {
	res, add := a.res, a.add
	if why := serverStepSkipped(a.d, a.cfg); why != "" {
		add("server", "skipped", why)
		return nil
	}
	senv, err := runChild(ctx, a.target, a.fl.serverUpgrade("--plan")...)
	if err != nil {
		return err
	}
	sp := decodeServerPlan(senv.Data)
	res.Server = &updateServer{Current: sp.Current, Target: sp.Target, Upgrade: sp.Upgrade}
	if !sp.Upgrade {
		add("server", "skipped", "the server runs "+sp.Target+" already")
		return nil
	}
	a.rb.serverTouched = true
	uenv, err := runChild(ctx, a.target, a.fl.serverUpgrade()...)
	var ur struct {
		Safety string `json:"safety_archive"`
	}
	_ = json.Unmarshal(uenv.Data, &ur)
	res.Server.Safety, a.marker.Safety = ur.Safety, ur.Safety
	if err != nil {
		if !upgradePassed(a.d, a.cfg, sp) {
			return err
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf("the new binary's server upgrade ended with an error (%v), but the server runs %s and no upgrade is in progress: it passed", err, sp.Target))
	}
	res.Server.Upgraded = true
	add("server", "done", sp.Current+" → "+sp.Target)
	return nil
}

// upgradePassed: no upgrade is marked in progress (the child clears its
// restore marker once it passed, or once its rollback finished) and the
// server's files name the new pin (a finished rollback names the old one).
func upgradePassed(d paths.Dirs, cfg config.Config, sp serverPlan) bool {
	if state.RestoreIncomplete(d) || sp.Target == sp.Current {
		return false
	}
	deployed, err := runtimeFor(cfg).Deployed()
	return err == nil && deployed == sp.Target
}

// clearAutoApply forgets a failed auto-apply: update apply run by hand
// (decision 0067 block 2 review: skipped until then, or a newer release).
func clearAutoApply(d paths.Dirs) {
	if st, err := state.Load(d.StateFile()); err == nil && st.AutoApply == nil {
		return
	}
	_ = state.UpdateWithin(d, "update apply", recordWait, func(s *state.State) error { s.AutoApply = nil; return nil })
}

func migrationsText(ids []string, verb string) string {
	if len(ids) == 0 {
		return "none pending"
	}
	return verb + " " + strings.Join(ids, ", ")
}

// staleStaging: an update- directory older than this is a killed update's
// (a live one downloads for at most downloadTimeout).
const staleStaging = 2 * time.Hour

// removeStaleStaging removes what a killed update left under the state
// directory (the caller holds the maintenance lock), never keep (this
// update's) nor one a concurrent update may still be downloading into.
func removeStaleStaging(d paths.Dirs, keep string) {
	ents, _ := os.ReadDir(d.State)
	for _, e := range ents {
		p := filepath.Join(d.State, e.Name())
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "update-") || p == keep {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > staleStaging {
			_ = os.RemoveAll(p)
		}
	}
}

// verifyRelease downloads the archive, checksums.txt and its signature
// into work and verifies the signature with the embedded keys and the
// archive's hash. assets lists what it downloaded and keyID the signer
// once the signature passed, also on a later failure; every failure
// changed nothing (exit 20).
func verifyRelease(ctx context.Context, src release.Source, rel release.Release, archive, work string) (assets []release.Asset, keyID string, err error) {
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	for _, name := range []string{archive, selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
		if err := download(dctx, src, rel, name, filepath.Join(work, name)); err != nil {
			return assets, "", nothingChanged("cannot download %s (%v)", name, err)
		}
		a, _ := rel.Asset(name)
		assets = append(assets, a)
	}
	cancel()
	sums, err := os.ReadFile(filepath.Join(work, selfcheck.ChecksumsFile))
	if err != nil {
		return assets, "", blocked(err)
	}
	sig, err := os.ReadFile(filepath.Join(work, selfcheck.SignatureFile))
	if err != nil {
		return assets, "", blocked(err)
	}
	if keyID, err = signing.VerifyChecksums(sums, sig); err != nil {
		return assets, "", nothingChanged("release %s fails the signature check (%v)", rel.Version, err)
	}
	f, err := os.Open(filepath.Join(work, archive))
	if err != nil {
		return assets, "", blocked(err)
	}
	err = signing.ArchiveSum(sums, archive, f)
	_ = f.Close()
	if err != nil {
		return assets, keyID, nothingChanged("release %s fails the checksum check (%v)", rel.Version, err)
	}
	return assets, keyID, nil
}

// fetchVerified downloads and verifies the release (verifyRelease),
// unpacks the binary, checks it against the bare binary's hash when the
// release lists it, and runs it once: it must report the release's
// version. It returns the unpacked binary.
func fetchVerified(ctx context.Context, src release.Source, rel release.Release, archive, work string, res *updateApplyResult, add func(name, status, detail string)) (string, error) {
	names := []string{archive, selfcheck.ChecksumsFile, selfcheck.SignatureFile}
	assets, keyID, err := verifyRelease(ctx, src, rel, archive, work)
	res.Assets = append(res.Assets, assets...)
	if len(assets) == len(names) {
		add("download", "done", strings.Join(names, ", "))
	}
	res.KeyID = keyID
	if err != nil {
		return "", err
	}
	sums, err := os.ReadFile(filepath.Join(work, selfcheck.ChecksumsFile))
	if err != nil {
		return "", blocked(err)
	}
	staged := filepath.Join(work, "bin", binaryName())
	if err := unpackBinary(filepath.Join(work, archive), staged); err != nil {
		return "", nothingChanged("cannot unpack %s (%v)", archive, err)
	}
	bare := selfcheck.BinaryName(rel.Version, runtime.GOOS, runtime.GOARCH)
	if listsFile(sums, bare) {
		f, err := os.Open(staged)
		if err != nil {
			return "", blocked(err)
		}
		err = signing.ArchiveSum(sums, bare, f)
		_ = f.Close()
		if err != nil {
			return "", nothingChanged("the binary in %s is not the release's %s (%v)", archive, bare, err)
		}
	} else {
		res.Warnings = append(res.Warnings, "checksums.txt lists no "+bare+": the new binary's self-check will fail (doctor error) until a release lists it")
	}
	add("verify", "done", fmt.Sprintf("checksums.txt signed by key %s, %s and the binary match it", res.KeyID, archive))
	env, err := runChild(ctx, staged, "version")
	if err != nil {
		return "", nothingChanged("the new binary does not run here (%v)", err)
	}
	var v struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(env.Data, &v)
	if strings.TrimPrefix(v.Version, "v") != rel.Version {
		return "", nothingChanged("the binary of release %s reports version %s", rel.Version, v.Version)
	}
	add("run", "done", "the new binary runs and reports "+v.Version)
	return staged, nil
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "backsight.exe"
	}
	return "backsight"
}

// listsFile reports whether checksums.txt lists name ("<hash>  <name>",
// the last line with or without its newline).
func listsFile(sums []byte, name string) bool {
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return true
		}
	}
	return false
}

// downloadTimeout bounds the download of a release's three assets;
// maxAsset bounds each one (an archive is ~5 MB).
var (
	downloadTimeout       = 15 * time.Minute
	maxAsset        int64 = 256 << 20
)

// download copies one asset of rel to dst: no more than the size the
// release lists (and maxAsset), and exactly that size when it lists one.
func download(ctx context.Context, src release.Source, rel release.Release, name, dst string) error {
	a, _ := rel.Asset(name)
	if a.Size > maxAsset {
		return fmt.Errorf("%s is %d bytes, more than the %d an asset may have", name, a.Size, maxAsset)
	}
	limit := maxAsset
	if a.Size > 0 {
		limit = a.Size
	}
	r, err := src.Open(ctx, rel, name)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(ctxReader{ctx, r}, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return err
	case n > limit:
		return fmt.Errorf("%s is larger than the %d bytes the release lists", name, limit)
	case a.Size > 0 && n != a.Size:
		return fmt.Errorf("%s is %d bytes, the release lists %d", name, n, a.Size)
	}
	return nil
}

// ctxReader stops a copy once ctx is done (a source that does not watch
// it, a stalled stream between reads).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// maxBinary bounds the unpacked binary (a release binary is ~14 MB).
var maxBinary int64 = 512 << 20

// unpackBinary extracts the backsight binary of a goreleaser archive
// (.tar.gz, or .zip on Windows) to dst.
func unpackBinary(archive, dst string) error {
	want := binaryName()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	write := func(r io.Reader) error {
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, io.LimitReader(r, maxBinary+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && n > maxBinary {
			err = fmt.Errorf("%s is larger than %d bytes", want, maxBinary)
		}
		return err
	}
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer func() { _ = zr.Close() }()
		for _, zf := range zr.File {
			if path.Base(zf.Name) == want && !zf.FileInfo().IsDir() {
				r, err := zf.Open()
				if err != nil {
					return err
				}
				defer func() { _ = r.Close() }()
				return write(r)
			}
		}
		return fmt.Errorf("no %s in the archive", want)
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("no %s in the archive", want)
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == want {
			return write(tr)
		}
	}
}

// updateRollback puts the install back as it was before the shim switch.
type updateRollback struct {
	d                paths.Dirs
	shim             string
	previous, target string
	removeTarget     bool
	clientArgs       []string // client install, with the wired profile
	wayBack          string   // the previous binary's client install, by hand
	pluginTouched    bool
	serverTouched    bool              // the new binary's server upgrade ran
	committed        bool              // past the commit point: never rolled back
	saved            map[string][]byte // state.json and config.toml before the update (nil: absent)
	add              func(name, status, detail string)
	res              *updateApplyResult
}

// takeSnapshot reads state.json and config.toml as they are.
func takeSnapshot(d paths.Dirs) (map[string][]byte, error) {
	saved := map[string][]byte{}
	for _, p := range []string{d.StateFile(), d.ConfigFile()} {
		raw, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			saved[p] = nil
		case err != nil:
			return nil, err
		default:
			saved[p] = raw
		}
	}
	return saved, nil
}

func (r *updateRollback) removeBinary() {
	if r.removeTarget {
		_ = os.RemoveAll(filepath.Dir(r.target))
	}
}

// run rolls back, in the order the previous binary needs: state.json and
// config.toml first (state.json under its lock), then the server (only
// ever by restoring the new binary's safety archive with the previous
// binary), the shim, and the previous binary's own client install when
// the new plugin was generated, whatever failed before it. Then the new
// binary is removed: exit 30, or 10 with the actions of a child that
// needs the user. A step that fails exits 20, keeps the update marker,
// and names the way back. Past the commit point nothing is rolled back:
// exit 40, rerun update apply.
func (r *updateRollback) run(ctx context.Context, cause error) error {
	if r.committed {
		return &output.Error{Code: output.Partial, Message: fmt.Sprintf("backsight %s is installed, but the update could not be recorded (%v): rerun backsight update apply", r.res.Target, cause)}
	}
	base := context.WithoutCancel(ctx)
	var errs []string
	files := make([]string, 0, len(r.saved))
	for p := range r.saved {
		files = append(files, p)
	}
	slices.Sort(files)
	for _, p := range files {
		if err := r.restoreFile(p, r.saved[p]); err != nil {
			errs = append(errs, "restoring "+p+": "+err.Error())
		}
	}
	if r.serverTouched {
		if err := r.serverBack(base); err != nil {
			errs = append(errs, "the server: "+err.Error())
		}
	}
	if err := pointShim(r.previous, r.shim); err != nil {
		errs = append(errs, "switching the shim back: "+err.Error())
	}
	if r.pluginTouched {
		cctx, cancel := context.WithTimeout(base, 5*time.Minute)
		_, err := runChild(cctx, r.previous, r.clientArgs...)
		cancel()
		if err != nil {
			errs = append(errs, "the previous plugin: "+err.Error())
		}
	}
	if len(errs) > 0 {
		r.add("rollback", "failed", strings.Join(errs, "; "))
		return &output.Error{Code: output.Blocked, Message: fmt.Sprintf("update failed (%v) and so did the rollback (%s): run %s to return to the previous version, then backsight doctor",
			cause, strings.Join(errs, "; "), r.wayBack)}
	}
	r.removeBinary()
	_ = state.ClearUpdateMarker(r.d)
	r.res.RolledBack = true
	r.add("rollback", "done", "the shim runs "+r.previous+" again, state.json, config.toml and the plugin as they were")
	msg := fmt.Sprintf("update to %s failed and was rolled back: %v", r.res.Target, cause)
	var ce *childError
	if errors.As(cause, &ce) && ce.code == output.NeedsUser && ce.env != nil {
		e := *ce.env
		e.Code, e.Message = output.NeedsUser, msg
		return &e
	}
	return &output.Error{Code: output.RolledBack, Message: msg}
}

// restoreFile puts one snapshotted file back (raw nil: it did not exist);
// state.json under its lock.
func (r *updateRollback) restoreFile(p string, raw []byte) error {
	if p == r.d.StateFile() {
		return state.SaveWithin(r.d, "update apply", recordWait, raw)
	}
	if raw == nil {
		return removeIfExists(p)
	}
	return fsutil.WriteAtomic(p, raw, 0o600)
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// serverBack puts the server back when the new binary's server upgrade did
// not: it rolls itself back, and only when it died first (or its rollback
// failed) does its marker remain, naming the safety archive. The previous
// binary's restore then puts that archive back on its own pin, under
// update apply's lock, with a new restore epoch and a temporary key
// (restore's rollback, decision 0044). Nothing is ever re-rendered: the
// database may have migrated.
func (r *updateRollback) serverBack(ctx context.Context) error {
	m, err := state.ReadRestoreMarker(r.d)
	if m == nil {
		return nil // the upgrade changed nothing, or rolled itself back
	}
	if err != nil || m.Safety == "" {
		return fmt.Errorf("a restore or upgrade did not finish and names no safety archive (%s)", r.d.RestoreMarker())
	}
	r.add("server", "rollback", "restoring the safety archive "+m.Safety+" with "+r.previous)
	_, err = runChild(ctx, r.previous, "restore", m.Safety, "--no-safety", "--non-interactive")
	return err
}

func updateApplyText(r updateApplyResult) string {
	var b strings.Builder
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning  %s\n", w)
	}
	if r.Plan && r.Target != "" {
		fmt.Fprintf(&b, "\nplan only: nothing was changed (%s would replace %s)\n", r.Target, r.Installed)
	}
	return b.String()
}
