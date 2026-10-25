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
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
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

func (b execBinary) Run(ctx context.Context, exe string, args ...string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	server.OwnGroup(cmd) // a Ctrl-C reaches update apply, which rolls back; a rollback's child is not killed with it
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

// runChild runs exe with args and --json; any exit but 0 is an error
// carrying the child's message (or its stderr's end when it printed no
// envelope).
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
	msg := ""
	if perr == nil && env.Error != nil {
		msg = env.Error.Message
	} else {
		msg = tail(string(stderr)+string(stdout), 400)
	}
	if code == 0 {
		return env, fmt.Errorf("%s: unexpected output: %w: %s", cmdline, perr, msg)
	}
	return env, fmt.Errorf("%s: exit %d: %s", cmdline, code, msg)
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
	Plugin     string          `json:"plugin,omitempty"`
	Server     *updateServer   `json:"server,omitempty"`
	Steps      []step          `json:"steps"`
	RolledBack bool            `json:"rolled_back,omitempty"`
	Warnings   []string        `json:"warnings,omitempty"`
	MS         int             `json:"ms"`
}

// updateServer is the server step: the version deployed and the new
// binary's pin.
type updateServer struct {
	Current  string `json:"current"`
	Target   string `json:"target"`
	Upgrade  bool   `json:"upgrade"`
	Upgraded bool   `json:"upgraded"`
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
	if _, err := os.Stat(filepath.Join(cfg.Server.ComposeDir, "compose.yaml")); err != nil {
		return "no server deployed on this machine"
	}
	return ""
}

func newUpdateApplyCmd(e *env) *cobra.Command {
	var fl updateApplyFlags
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Install a newer release: verify, install, switch the shim, migrate, update the plugin; roll back on any failure",
		Long: "Apply takes the maintenance lock, downloads the release archive with checksums.txt and its minisign signature, verifies both " +
			"(nothing changes when they do not verify: exit 20), installs the binary under <data>/bin/ with its checksums, switches the shim, " +
			"runs the new binary's pending client migrations and its client install (the plugin, then claude plugin update). Any failure after " +
			"the switch puts back the shim, state.json, config.toml and the previous plugin, and exits 30. --plan downloads and verifies into a " +
			"temporary directory, asks the new binary for its pending migrations, and changes nothing.",
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
			d, home, err := machine()
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
			ran, err := migrate.Apply(cmd.Context(), migrate.Env{Dirs: d, Home: home}, list)
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
// under the pre-release policy; nil when there is nothing newer.
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
		return &output.Error{Code: output.Blocked, Message: "no installed backsight (no shim at " + shim + "): install it first, then backsight setup"}
	}
	res.Previous = previous
	if state.RestoreIncomplete(d) {
		return nothingChanged("a restore did not finish (%s): run backsight doctor for the way back", d.RestoreMarker())
	}
	if held := state.Inspect(d.MaintenanceLock()); held != nil && !held.Stale {
		return nothingChanged("maintenance in progress: %s", held.Error())
	}
	if prev, _ := state.ReadUpdateMarker(d); prev != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("an update from %s to %s did not finish (started %s); this one replaces it", prev.From, prev.To, prev.StartedAt.Format(time.RFC3339)))
	}

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

	// The lock, held to the end: hooks only queue, jobs skip, no backup
	// or restore starts.
	if !fl.plan {
		l, note, err := acquireMaintenance(d, "update apply")
		if err != nil {
			return err
		}
		defer func() { _ = l.Release() }()
		if note != "" {
			res.Warnings = append(res.Warnings, note)
		}
		removeStaleStaging(d)
	}
	if err := os.MkdirAll(d.State, 0o700); err != nil {
		return blocked(err)
	}
	work, err := os.MkdirTemp(d.State, "update-")
	if err != nil {
		return blocked(err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	// 1. Download and verify: a failure here changed nothing.
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
	clientWired := st.PluginVersion != ""
	clientArgs := []string{"client", "install"}
	if st.Client != nil && st.Client.Profile != "" {
		clientArgs = append(clientArgs, "--config-dir", st.Client.Profile)
	}
	wayBack := previous + " " + strings.Join(clientArgs, " ")

	if fl.plan {
		env, err := runChild(ctx, staged, "update", "migrate", "--plan")
		if err != nil {
			return nothingChanged("the new binary cannot list its migrations (%v)", err)
		}
		var mr struct {
			Pending []string `json:"pending"`
		}
		_ = json.Unmarshal(env.Data, &mr)
		res.Migrations = append(res.Migrations, mr.Pending...)
		add("install", "planned", "would install "+target+" with "+selfcheck.ChecksumsFile+" and its signature")
		add("shim", "planned", "would switch "+shim+" from "+previous+" to "+target)
		add("migrations", "planned", migrationsText(res.Migrations, "would run"))
		if clientWired {
			res.Plugin = "planned"
			add("plugin", "planned", "would run the new binary's "+strings.Join(clientArgs, " ")+" (plugin regenerated, claude plugin update)")
		} else {
			res.Plugin = "skipped: Claude Code is not wired on this machine"
			add("plugin", "skipped", "Claude Code is not wired on this machine (no client install)")
		}
		if why := serverStepSkipped(d, cfg); why != "" {
			add("server", "skipped", why)
			return nil
		}
		senv, err := runChild(ctx, staged, "server", "upgrade", "--plan")
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
		return nil
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

	// From here a failure rolls back.
	rb := &updateRollback{d: d, shim: shim, previous: previous, target: target, removeTarget: created && target != previous, clientArgs: clientArgs, wayBack: wayBack, add: add, res: res}
	if err := rb.snapshot(); err != nil {
		rb.removeBinary()
		return nothingChanged("cannot save state.json and config.toml before the update (%v)", err)
	}
	marker := state.UpdateMarker{From: res.Installed, To: rel.Version, Previous: previous, Binary: target, WayBack: wayBack, StartedAt: now().UTC().Truncate(time.Second)}
	if err := state.WriteUpdateMarker(d, marker); err != nil {
		rb.removeBinary()
		return nothingChanged("cannot mark the update in progress (%v)", err)
	}
	failed := func(name string, err error) error {
		add(name, "failed", err.Error())
		return rb.run(ctx, fmt.Errorf("%s: %w", name, err))
	}

	// 3. The shim.
	if err := pointShim(target, shim); err != nil {
		return failed("shim", err)
	}
	add("shim", "done", shim+" → "+target)
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
	if clientWired {
		rb.pluginTouched = true
		if _, err := runChild(ctx, target, clientArgs...); err != nil {
			return failed("plugin", err)
		}
		res.Plugin = "updated"
		add("plugin", "done", "the new binary's "+strings.Join(clientArgs, " ")+": plugin regenerated, claude plugin update")
	} else {
		res.Plugin = "skipped: Claude Code is not wired on this machine"
		add("plugin", "skipped", "Claude Code is not wired on this machine (no client install)")
	}
	if err := ctx.Err(); err != nil {
		return failed("plugin", err)
	}

	// 6. The server, when the new binary pins another version than the
	// one deployed: the new binary owns its pin, so it runs the upgrade
	// (and its own rollback to the version deployed before).
	if why := serverStepSkipped(d, cfg); why != "" {
		add("server", "skipped", why)
	} else {
		senv, err := runChild(ctx, target, "server", "upgrade", "--plan")
		if err != nil {
			return failed("server", err)
		}
		sp := decodeServerPlan(senv.Data)
		res.Server = &updateServer{Current: sp.Current, Target: sp.Target, Upgrade: sp.Upgrade}
		if sp.Upgrade {
			rb.serverTouched = true
			if _, err := runChild(ctx, target, "server", "upgrade"); err != nil {
				return failed("server", err)
			}
			res.Server.Upgraded = true
			add("server", "done", sp.Current+" → "+sp.Target)
		} else {
			add("server", "skipped", "the server runs "+sp.Target+" already")
		}
	}

	// 7. Recorded: the shim's target and the one to roll back to.
	if err := state.Update(d, "update apply", func(s *state.State) error {
		if s.Client == nil {
			s.Client = &state.Client{}
		}
		s.ShimTarget, s.Client.ShimPrevious = target, previous
		return nil
	}); err != nil {
		return failed("record", err)
	}
	_ = state.ClearUpdateMarker(d)
	add("done", "done", "backsight "+rel.Version+" installed; restart Claude Code (or /reload-plugins) to load its plugin")
	return nil
}

func migrationsText(ids []string, verb string) string {
	if len(ids) == 0 {
		return "none pending"
	}
	return verb + " " + strings.Join(ids, ", ")
}

// removeStaleStaging removes what a killed update left under the state
// directory (the caller holds the maintenance lock).
func removeStaleStaging(d paths.Dirs) {
	ents, _ := os.ReadDir(d.State)
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "update-") {
			_ = os.RemoveAll(filepath.Join(d.State, e.Name()))
		}
	}
}

// fetchVerified downloads the archive, checksums.txt and its signature
// into work, verifies the signature with the embedded keys and the
// archive's hash, unpacks the binary, checks it against the bare binary's
// hash when the release lists it, and runs it once: it must report the
// release's version. It returns the unpacked binary.
func fetchVerified(ctx context.Context, src release.Source, rel release.Release, archive, work string, res *updateApplyResult, add func(name, status, detail string)) (string, error) {
	names := []string{archive, selfcheck.ChecksumsFile, selfcheck.SignatureFile}
	for _, name := range names {
		if err := download(ctx, src, rel, name, filepath.Join(work, name)); err != nil {
			return "", nothingChanged("cannot download %s (%v)", name, err)
		}
		a, _ := rel.Asset(name)
		res.Assets = append(res.Assets, a)
	}
	add("download", "done", strings.Join(names, ", "))
	sums, err := os.ReadFile(filepath.Join(work, selfcheck.ChecksumsFile))
	if err != nil {
		return "", blocked(err)
	}
	sig, err := os.ReadFile(filepath.Join(work, selfcheck.SignatureFile))
	if err != nil {
		return "", blocked(err)
	}
	if res.KeyID, err = signing.VerifyChecksums(sums, sig); err != nil {
		return "", nothingChanged("release %s fails the signature check (%v)", rel.Version, err)
	}
	f, err := os.Open(filepath.Join(work, archive))
	if err != nil {
		return "", blocked(err)
	}
	err = signing.ArchiveSum(sums, archive, f)
	_ = f.Close()
	if err != nil {
		return "", nothingChanged("release %s fails the checksum check (%v)", rel.Version, err)
	}
	staged := filepath.Join(work, "bin", binaryName())
	if err := unpackBinary(filepath.Join(work, archive), staged); err != nil {
		return "", nothingChanged("cannot unpack %s (%v)", archive, err)
	}
	bare := selfcheck.BinaryName(rel.Version, runtime.GOOS, runtime.GOARCH)
	if bytes.Contains(sums, []byte("  "+bare+"\n")) {
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

func download(ctx context.Context, src release.Source, rel release.Release, name, dst string) error {
	r, err := src.Open(ctx, rel, name)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// maxBinary bounds the unpacked binary (a release binary is ~14 MB).
const maxBinary = 512 << 20

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
	saved            map[string][]byte // state.json and config.toml before the update (nil: absent)
	add              func(name, status, detail string)
	res              *updateApplyResult
}

func (r *updateRollback) snapshot() error {
	r.saved = map[string][]byte{}
	for _, p := range []string{r.d.StateFile(), r.d.ConfigFile()} {
		raw, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			r.saved[p] = nil
		case err != nil:
			return err
		default:
			r.saved[p] = raw
		}
	}
	return nil
}

func (r *updateRollback) removeBinary() {
	if r.removeTarget {
		_ = os.RemoveAll(filepath.Dir(r.target))
	}
}

// run puts back state.json and config.toml, the shim and, when the new
// plugin was generated, the previous binary's (its own client install),
// then removes the new binary: exit 30. A step that fails exits 20, keeps
// the update marker, and names the way back. No step follows a cancel.
func (r *updateRollback) run(ctx context.Context, cause error) error {
	base := context.WithoutCancel(ctx)
	var errs []string
	if r.serverTouched {
		if err := r.serverBack(base); err != nil {
			errs = append(errs, "the server: "+err.Error())
		}
	}
	for p, raw := range r.saved {
		var err error
		if raw == nil {
			err = os.Remove(p)
			if errors.Is(err, fs.ErrNotExist) {
				err = nil
			}
		} else {
			err = fsutil.WriteAtomic(p, raw, 0o600)
		}
		if err != nil {
			errs = append(errs, "restoring "+p+": "+err.Error())
		}
	}
	if err := pointShim(r.previous, r.shim); err != nil {
		errs = append(errs, "switching the shim back: "+err.Error())
	}
	if r.pluginTouched && len(errs) == 0 {
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
	return &output.Error{Code: output.RolledBack, Message: fmt.Sprintf("update to %s failed and was rolled back: %v", r.res.Target, cause)}
}

// serverBack makes the server run this binary's pin again. The new
// binary's server upgrade rolls itself back; if it died first, its marker
// names the safety archive, which restore puts back (with this binary's
// pin, under update apply's lock). Otherwise this binary's files are
// rendered again, and the server started when they changed.
func (r *updateRollback) serverBack(ctx context.Context) error {
	if m, _ := state.ReadRestoreMarker(r.d); m != nil {
		if m.Safety == "" {
			return fmt.Errorf("a restore or upgrade did not finish and names no safety archive (%s)", r.d.RestoreMarker())
		}
		r.add("server", "rollback", "restoring the safety archive "+m.Safety)
		rr := restoreResult{Archive: m.Safety, Steps: []step{}}
		return runRestore(ctx, nil, &rr, restoreFlags{noSafety: true, nonInter: true}, r.add)
	}
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return err
	}
	sl, _, err := state.Acquire(filepath.Join(r.d.LocksDir(), "server.lock"), "update apply")
	if err != nil {
		return err
	}
	defer func() { _ = sl.Release() }()
	rt := ownGroupRuntime(cfg)
	files, err := renderOn(rt, sec, "backsight server start")
	if err != nil {
		return err
	}
	ch, err := rt.Write(files)
	if err != nil || (!ch.Compose && !ch.Env) {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, rollbackStepTimeout)
	defer cancel()
	if err := rt.Start(sctx); err != nil {
		return err
	}
	_, err = waitHealthy(sctx, cfg, sec)
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
