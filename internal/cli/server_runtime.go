package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/backup"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// server runtime switch moves an install's memory between the docker and
// the native runtime (decision 0078): a backup taken with the source's API
// stopped, verified in the target, restored into it, gated on the data
// being exactly the source's, its recall compared, and only then
// server.runtime written. No restore epoch and no temporary key: the
// target holds what the source held, under the same URL, key and
// server_id. The source's data stays until server runtime clean.

// switchProbes are the target's pre-flight probes; tests replace them.
var switchProbes = struct {
	userManager  func(context.Context) error
	linger       func() (string, error)
	freeBytes    func(dir string) (uint64, error)
	shimRuns     func(ctx context.Context, shim string) error
	goos, goarch string
}{userSession, linger, doctor.FreeBytes, shimKnowsNativeCheck, runtime.GOOS, runtime.GOARCH}

// nativeShim is the shim the native db unit's ExecStartPre runs (server
// native-check): its guard at every start.
func nativeShim(d paths.Dirs) string {
	return filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS))
}

// shimMissing: no executable file behind the shim (none, a dangling link,
// or not executable), which systemd would fail with 203/EXEC; server
// install and the switch install it again.
func shimMissing(shim string) bool { return !fsutil.Executable(shim) }

// shimProbeWait bounds the shim's --help.
const shimProbeWait = 15 * time.Second

// shimKnowsNativeCheck runs the shim's server native-check --help, with a
// clean environment: a backsight older than the native runtime prints
// server's own help instead (cobra), which does not name the command.
func shimKnowsNativeCheck(ctx context.Context, shim string) error {
	ctx, cancel := context.WithTimeout(ctx, shimProbeWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, shim, "server", "native-check", "--help")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("it does not run (%w)", err)
	}
	if !strings.Contains(string(out), "server native-check") {
		return errors.New("the backsight it runs has no server native-check (an older release)")
	}
	return nil
}

// recallProbe is one of the user's recent memories, recalled by its own text:
// the ids of its top 10 before the switch.
type recallProbe struct {
	Memory, Text string
	Top          []string
}

type probeReport struct {
	Queries    int     `json:"queries"`
	Overlap    float64 `json:"overlap_at_10"` // raw: the target's top 10 only
	OwnAtRank1 int     `json:"own_at_rank_1"`
	// BeyondTies also counts the near-tie swaps as found (issue #123): it
	// drives the warning. NearTies is how many hits, over all the probes.
	BeyondTies float64 `json:"overlap_beyond_near_ties"`
	NearTies   int     `json:"near_tie_swaps"`
}

type switchResult struct {
	From       string       `json:"from"`
	To         string       `json:"to"`
	Plan       bool         `json:"plan"`
	Declined   bool         `json:"declined,omitempty"`
	Steps      []step       `json:"steps"`
	Backup     string       `json:"backup,omitempty"`
	Gate       []string     `json:"gate_differences,omitempty"`
	Probes     *probeReport `json:"probes,omitempty"`
	SourceKept string       `json:"source_kept,omitempty"`
	Warnings   []string     `json:"warnings,omitempty"`
}

type switchFlags struct {
	plan, nonInteractive bool
	answers              []string // consent=yes only
}

type cleanResult struct {
	Runtime string `json:"runtime"` // the one deleted
	What    string `json:"what"`    // everything Delete removes (server.Cleaner)
	Bytes   int64  `json:"bytes"`
	// SizeUnknown: the runtime could not tell its size (Bytes is 0).
	SizeUnknown bool `json:"size_unknown,omitempty"`
	Plan        bool `json:"plan"`
	Declined    bool `json:"declined,omitempty"`
	Deleted     bool `json:"deleted"`
}

// recallWarnBelow: an overlap@10 below it is said (decision 0078: reported,
// never gated).
const recallWarnBelow = 0.9

// nearTie: a source top-10 hit the target ranks past 10 with a final score
// within it of the target's 10th is a near-tie swap, found (issue #123: an
// order above the torch/onnx score differences and the measured gaps at the
// cutoff, ~1e-3).
const nearTie = 0.01

// probeCount is how many recent memories are probed.
const probeCount = 20

// switchLog is the switches' record, one JSON line each.
func switchLog(stateDir string) string {
	return filepath.Join(stateDir, "logs", "runtime-switch.jsonl")
}

func newServerRuntimeCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{Use: "runtime", Short: "Move the local server between the docker and native runtimes", Args: cobra.NoArgs}
	cmd.AddCommand(newServerRuntimeSwitchCmd(e), newServerRuntimeCleanCmd(e), newServerRuntimeFingerprintCmd(e))
	return cmd
}

func newServerRuntimeSwitchCmd(e *env) *cobra.Command {
	var f switchFlags
	cmd := &cobra.Command{
		Use:   "switch <docker|native>",
		Short: "Move the memory to the other runtime: a verified backup restored into it, gated on the data, rolled back on any failure",
		Long: "Checks the target (Docker and Compose, or the user session, lingering and disk for native) and downloads what it runs, then, " +
			"under the maintenance lock: recalls up to 20 recent memories, stops the source's API, backs it up and verifies the archive in the " +
			"target, stops the source (its data is kept), restores the archive into the target, compares the target's data with the source's " +
			"(every table's rows and keys, the banks, the control documents: any difference fails), starts it, recalls the same memories again " +
			"(reported, never a failure) and only then writes server.runtime to config.toml. Any failure or interruption before that stops the " +
			"target and starts the source as it was (exit 20); the backup is kept. No restore epoch, no client action: same URL, key and server id.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := runSwitch(cmd.Context(), e, args[0], f)
			return e.report("server runtime switch", res, func() string { return switchText(res) }, err)
		},
	}
	cmd.Flags().BoolVar(&f.plan, "plan", false, "check the target and list the steps without changing anything")
	cmd.Flags().BoolVar(&f.nonInteractive, "non-interactive", false, "never prompt: exit 10 when the consent is missing")
	cmd.Flags().StringArrayVar(&f.answers, "answer", nil, "answer a question: consent=yes (go ahead without the prompt)")
	return cmd
}

// command is this run as a command line, for a needs_user resume.
func (f switchFlags) command(verb, arg string) string {
	cmd := "backsight server runtime " + verb
	if arg != "" {
		cmd += " " + arg
	}
	if f.nonInteractive {
		cmd += " --non-interactive"
	}
	return cmd
}

// consentAnswer validates --answer: consent=yes is the only one.
func consentAnswer(list []string, command string) (bool, error) {
	answers, err := parseAnswers(list, "")
	if err != nil {
		return false, err
	}
	for k, v := range answers {
		if k != "consent" {
			return false, &output.Error{Code: output.Usage, Message: "unknown answer " + k + ": " + command + " asks consent only"}
		}
		if v != "yes" {
			return false, &output.Error{Code: output.Usage, Message: "answer consent=" + v + ": want yes"}
		}
	}
	return answers["consent"] == "yes", nil
}

// runtimeOfKind is cfg's install on the runtime kind, in its own process
// group (a Ctrl-C must not kill a rollback's docker or systemctl).
func runtimeOfKind(cfg config.Config, kind string) server.Runtime {
	c := cfg
	c.Server.Runtime = ""
	if kind == "native" {
		c.Server.Runtime = kind
	}
	rt := ownGroupRuntime(c)
	if n, ok := rt.(server.Native); ok && cfg.RuntimeKind() != "native" {
		n.NotInUse = true // config.toml names docker: a refusal points at clean
		rt = n
	}
	return rt
}

func configOfKind(cfg config.Config, kind string) config.Config {
	cfg.Server.Runtime = ""
	if kind == "native" {
		cfg.Server.Runtime = kind
	}
	return cfg
}

func otherRuntime(kind string) string {
	if kind == "native" {
		return "docker"
	}
	return "native"
}

// switchRefusedOnWindows: native is Windows' only runtime in this
// release (decision 0080).
const switchRefusedOnWindows = "the runtime switch is not available on Windows in this release: Docker on Windows is untested"

func runSwitch(ctx context.Context, e *env, to string, f switchFlags) (switchResult, error) {
	res := switchResult{To: to, Plan: f.plan, Steps: []step{}}
	add := streamStep(e, "server runtime switch", &res.Steps)
	if to != "docker" && to != "native" {
		return res, &output.Error{Code: output.Usage, Message: fmt.Sprintf("server runtime switch: %q is not docker or native", to)}
	}
	if switchProbes.goos == "windows" {
		return res, nothingChanged("%s", switchRefusedOnWindows)
	}
	agreed, err := consentAnswer(f.answers, "server runtime switch")
	if err != nil {
		return res, err
	}

	// 1. Refusals: nothing changes.
	cfg, sec, err := loadServerConfig()
	if err != nil {
		return res, err
	}
	if err := requireLocal(cfg, "server runtime switch"); err != nil {
		return res, err
	}
	res.From = cfg.RuntimeKind()
	if to == "native" {
		switch {
		case switchProbes.goos != "linux" || switchProbes.goarch != "amd64":
			return res, nothingChanged("%s (this is %s/%s): stay on the docker runtime", config.NativeRefusal, switchProbes.goos, switchProbes.goarch)
		case cfg.Server.Role == config.RoleServer:
			return res, &output.Error{Code: output.Usage, Message: config.NativeServerOnly + ": nothing was changed"}
		case cfg.Network.Exposure == "private":
			return res, nothingChanged("the native runtime serves this PC only (loopback) in this release, and this server is open to your other PCs: " +
				"close it first with backsight server expose --loopback, or stay on the docker runtime")
		}
	}
	d, home, err := machine()
	if err != nil {
		return res, err
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		return res, blocked(err)
	}
	var cut *state.RuntimeSwitch // a switch killed part way: its source goes back first
	if rs := st.RuntimeSwitch; rs != nil && rs.Step == "running" {
		if rs.Running() {
			return res, nothingChanged("a runtime switch to %s is already running (pid %d, since %s): wait for it", rs.To, rs.PID, rs.Since.Format(time.RFC3339))
		}
		cut = rs
		if res.From == rs.To { // killed after its config was written: it passed
			if !f.plan {
				done := *rs
				done.Step, done.At = "done", time.Now().UTC()
				_ = state.Update(d, "server runtime switch", func(s *state.State) error { s.RuntimeSwitch = &done; return nil })
			}
			cut = nil
		}
	}
	if cut == nil && to == res.From {
		add("check", "done", "already on "+to)
		return res, nil
	}
	raw, sha, err := fsutil.ReadHashed(d.ConfigFile())
	if err != nil {
		return res, blocked(err)
	}
	newRaw, err := config.SetRuntime(raw, to)
	if err != nil {
		return res, &output.Error{Code: output.Usage, Message: err.Error() + ": nothing was changed"}
	}
	next, err := config.Parse(newRaw, d.ConfigFile(), d, home)
	if err != nil {
		return res, &output.Error{Code: output.Usage, Message: err.Error() + ": nothing was changed"}
	}
	src, dst := runtimeOfKind(cfg, res.From), runtimeOfKind(cfg, to)
	if c, ok := dst.(server.Creator); ok { // a first native database is the switch's to create (ruling R35)
		dst = c.AllowCreate()
	}
	// A switch moves the memory, never the Hindsight release: an upgrade
	// goes through server upgrade's ordering, safety backup and rollback.
	if to != res.From {
		deployed, err := src.Deployed()
		if err != nil {
			return res, blocked(err)
		}
		if deployed == "" {
			return res, nothingChanged("no server is installed on %s here: there is nothing to switch (backsight server install)", res.From)
		}
		switch have, want := server.HindsightVersionOf(deployed), dst.HindsightVersion(); {
		case have == "" || want == "":
			res.Warnings = append(res.Warnings, fmt.Sprintf("cannot tell whether %s (%s) and %s (%s) run the same Hindsight release", res.From, deployed, to, dst.Version()))
		case have != want:
			return res, nothingChanged("%s runs Hindsight %s and this backsight installs %s on %s: a switch never changes the Hindsight release; "+
				"run backsight server upgrade first (or backsight update apply when %s is the newer), then switch", res.From, have, want, to, have)
		}
	}
	status := "done"
	if f.plan {
		status = "planned"
	}
	if cut != nil {
		add("recover", status, "a switch to "+cut.To+" was cut off (pid "+fmt.Sprint(cut.PID)+" is gone): "+cut.To+" is stopped and "+cut.From+" started again first")
	}

	// Pre-flight, read only: what the target needs and what it downloads.
	var toFetch []string // what the preflight found to fetch
	installShimAt := ""  // the native db unit's guard, missing: installed after the consent
	if to != res.From {
		detail, missing, shim, warnings, err := switchPreflight(ctx, dst, to, d, f.command("switch", to))
		if err != nil {
			return res, err
		}
		toFetch, installShimAt = missing, shim
		res.Warnings = append(res.Warnings, warnings...)
		add("preflight", status, detail)
	}
	if f.plan || !agreed {
		plan := switchPlan(res.From, to, cut != nil && to == res.From, installShimAt)
		if f.plan {
			for _, s := range plan {
				add(s[0], "planned", s[1])
			}
			return res, nil
		}
		interactive := !e.json && !f.nonInteractive && stdinIsTerminal() && stdoutIsTerminal()
		question := fmt.Sprintf("Switch the memory server from %s to %s? [y/N] ", res.From, to)
		if to == res.From {
			question = fmt.Sprintf("Start %s again and stop %s (the switch to %s was cut off)? [y/N] ", res.From, cut.To, cut.To)
		}
		if !interactive {
			resume := f.command("switch", to) + " --answer consent=yes"
			return res, &output.Error{Code: output.NeedsUser, Resume: resume,
				Questions: []setup.Question{{ID: "consent", Required: true,
					Prompt:  "The switch stops the memory server for a few minutes and moves the memory from " + res.From + " to " + to + ". Answer yes to go ahead",
					Options: []setup.Option{{Value: "yes", Label: "switch to " + to}}}},
				Message: "the switch from " + res.From + " to " + to + " needs your go: rerun with --answer consent=yes: nothing was changed"}
		}
		fmt.Fprintln(e.stdout)
		for _, s := range plan {
			fmt.Fprint(e.stdout, stepLine(step{s[0], "planned", s[1]}))
		}
		if !confirm(e, question) {
			res.Declined = true
			return res, nil
		}
	}

	// The downloads, before anything stops: a failure changes nothing.
	if to != res.From {
		what, err := switchFetch(ctx, e, dst, toFetch)
		if err != nil {
			add("download", "failed", server.Redact(err.Error(), sec))
			return res, nothingChanged("%s", server.Redact("cannot download what "+to+" runs ("+err.Error()+")", sec))
		}
		add("download", "done", what)
	}
	if installShimAt != "" {
		target, err := shimTarget(d.Data)
		if err == nil {
			err = installShim(target, installShimAt)
		}
		if err != nil {
			add("shim", "failed", err.Error())
			return res, nothingChanged("cannot install the shim the native database unit runs (%s): %v", installShimAt, err)
		}
		add("shim", "done", installShimAt+" → "+target)
	}

	// 2. Under the maintenance lock and server.lock to the end: hooks
	// queue, jobs skip.
	unlock, note, err := serverMaintenanceLock(d, "server runtime switch")
	if err != nil {
		return res, err
	}
	defer unlock()
	if note != "" {
		res.Warnings = append(res.Warnings, note)
	}
	// The pre-flight and the consent were about the config.toml and the
	// secrets read before the downloads, which take minutes: an edit since
	// would fail the commit and roll the whole move back. Refused here,
	// before anything stops.
	if err := switchInputsUnchanged(d, sha, cfg, sec, to); err != nil {
		return res, err
	}
	add("lock", "done", d.MaintenanceLock()+", server.lock")
	if cut != nil {
		if err := putSourceBack(ctx, cfg, sec, cut); err != nil {
			add("recover", "failed", server.Redact(err.Error(), sec))
			return res, redacted(fmt.Errorf("the switch to %s cut off earlier could not be undone (%w): stop %s (%s), then run backsight server start",
				cut.To, err, cut.To, runtimeOfKind(cfg, cut.To).StopCmd()), sec)
		}
		back := *cut
		back.Step, back.At = "rolled-back", time.Now().UTC()
		_ = state.Update(d, "server runtime switch", func(s *state.State) error { s.RuntimeSwitch = &back; return nil })
		add("recover", "done", cut.To+" stopped, "+cut.From+" runs again")
		if to == res.From {
			return res, nil
		}
	}

	// Read through the API while it runs (ruling R3): the bank config the
	// archive keeps, and the probes.
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 60*time.Second)
	if err != nil {
		return res, blocked(err)
	}
	bc := readBankConfig(ctx, cfg, sec)
	probes, warn := takeProbes(ctx, cl, cfg.Server.Bank)
	if warn != "" {
		res.Warnings = append(res.Warnings, server.Redact(warn, sec))
	}
	add("probes", "done", fmt.Sprintf("%d recent memories recalled", len(probes)))

	// From here a failure puts the source back.
	rs := state.RuntimeSwitch{From: res.From, To: to, Step: "running", PID: os.Getpid(), Since: time.Now().UTC(), At: time.Now().UTC()}
	if err := state.Update(d, "server runtime switch", func(s *state.State) error { s.RuntimeSwitch = &rs; return nil }); err != nil {
		return res, nothingChanged("cannot record the switch in state.json (%v)", err)
	}
	failed := func(name string, err error) error {
		add(name, "failed", server.Redact(err.Error(), sec))
		return rollbackSwitch(ctx, d, &res, cfg, sec, src, dst, rs, name, err, add)
	}
	if err := switchFailHook("stop-api"); err != nil {
		return res, failed("stop-api", err)
	}
	if err := src.StopAPI(ctx); err != nil {
		return res, failed("stop-api", err)
	}
	add("stop-api", "done", res.From+"'s API stopped, its database running")

	// The backup, with nothing written since the dump; the source's
	// fingerprint right after it.
	work, done, err := backup.VerifyDir(filepath.Join(d.State, "staging"))
	if err != nil {
		return res, failed("backup", err)
	}
	defer done()
	dump := filepath.Join(work, "switch.dump")
	if err := switchFailHook("backup"); err != nil {
		return res, failed("backup", err)
	}
	sb, err := createBackup(ctx, &safetyCopy{dump: dump, bankConfig: &bc})
	if err != nil {
		return res, failed("backup", err)
	}
	res.Backup, rs.Backup = sb.Path, sb.Path
	res.Warnings = append(res.Warnings, sb.Warnings...)
	_ = state.Update(d, "server runtime switch", func(s *state.State) error { s.RuntimeSwitch = &rs; return nil })
	before, err := server.TakeFingerprint(ctx, src.Database(), cfg.Server.Bank)
	if err != nil {
		return res, failed("backup", fmt.Errorf("the source's fingerprint: %w", err))
	}
	add("backup", "done", fmt.Sprintf("%s (%v)", sb.Path, sb.Counts))
	if len(probes) == 0 && sb.Counts["memory_units"] > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("no recent memory to probe recall with, though the bank holds %d memory units: recall was not compared", sb.Counts["memory_units"]))
	}

	// Verified in the runtime that restores it (requirement 4).
	if err := switchFailHook("verify"); err != nil {
		return res, failed("verify", err)
	}
	counts, err := dst.VerifyDump(ctx, dump, backup.Tables)
	if err == nil && !sameCounts(counts, sb.Counts) {
		err = fmt.Errorf("restored counts %v differ from the backup's %v", counts, sb.Counts)
	}
	if err != nil {
		return res, failed("verify", err)
	}
	add("verify", "done", fmt.Sprintf("restored in a throwaway %s database: %v", to, counts))

	// 3. The source stopped, its data kept.
	if err := switchFailHook("stop-source"); err != nil {
		return res, failed("stop-source", err)
	}
	if err := src.Stop(ctx); err != nil {
		return res, failed("stop-source", err)
	}
	add("stop-source", "done", res.From+" stopped, "+src.DataName()+" kept")

	// 4. The target: same secrets, same API port; its database alone,
	// the archive restored into it.
	if err := switchFailHook("target"); err != nil {
		return res, failed("target", err)
	}
	files, err := renderOn(dst, sec, f.command("switch", to))
	if err == nil {
		_, err = dst.Write(files)
	}
	if err == nil {
		err = dst.StartDatabase(ctx)
	}
	if err != nil {
		return res, failed("target", err)
	}
	add("target", "done", to+"'s database started")
	if err := switchFailHook("restore"); err != nil {
		return res, failed("restore", err)
	}
	if _, err := dst.Database().Restore(ctx, dump); err != nil {
		return res, failed("restore", err)
	}
	add("restore", "done", "the backup restored into "+to)
	if r, ok := dst.(interface{ RecordIdentity(context.Context) error }); ok {
		if err := switchFailHook("identity"); err != nil {
			return res, failed("identity", err)
		}
		if err := r.RecordIdentity(ctx); err != nil {
			return res, failed("identity", err)
		}
		add("identity", "done", "the native database's identity recorded")
	}

	// 5. The gate: the data, exactly, before the target's API starts.
	if err := switchFailHook("gate"); err != nil {
		return res, failed("gate", err)
	}
	after, err := server.TakeFingerprint(ctx, dst.Database(), cfg.Server.Bank)
	if err != nil {
		return res, failed("gate", fmt.Errorf("the target's fingerprint: %w", err))
	}
	if diff := before.Diff(after); len(diff) > 0 {
		res.Gate = diff
		return res, failed("gate", fmt.Errorf("the target's data differs from the source's: %s", strings.Join(diff, "; ")))
	}
	add("gate", "done", fmt.Sprintf("%d tables, %d banks, %d control documents identical", len(before.Tables), len(before.Banks), len(before.Control)))
	if err := switchFailHook("start"); err != nil {
		return res, failed("start", err)
	}
	if err := dst.Start(ctx); err != nil {
		return res, failed("start", err)
	}
	v, err := waitHealthy(ctx, dst, next, sec)
	if err != nil {
		return res, failed("start", err)
	}
	add("start", "done", to+" healthy (Hindsight "+v.APIVersion+")")
	if err := switchFailHook("recall"); err != nil {
		return res, failed("recall", err)
	}
	report, err := recallAgain(ctx, cl, cfg.Server.Bank, probes)
	if err != nil {
		return res, failed("recall", err)
	}
	if len(probes) > 0 {
		res.Probes = &report
		if report.BeyondTies < recallWarnBelow {
			res.Warnings = append(res.Warnings, fmt.Sprintf("recall differs after the switch (overlap@10 %.2f beyond near-ties, raw %.2f): the data is identical (the gate passed); %s's reranker ranks some memories differently", report.BeyondTies, report.Overlap, to))
		}
		add("recall", "done", fmt.Sprintf("%d probes: overlap@10 %.2f beyond near-ties (raw %.2f, %d near-tie swaps)", report.Queries, report.BeyondTies, report.Overlap, report.NearTies))
	} else {
		add("recall", "done", "answered")
	}

	// 6. Only now config.toml names the target.
	if err := switchFailHook("commit"); err != nil {
		return res, failed("commit", err)
	}
	if err := ctx.Err(); err != nil {
		return res, failed("commit", err)
	}
	if err := fsutil.ReplaceIfUnchanged(d.ConfigFile(), newRaw, 0o600, sha); err != nil {
		if errors.Is(err, fsutil.ErrConflict) {
			err = fmt.Errorf("%s changed while the switch ran", d.ConfigFile())
		}
		return res, failed("commit", err)
	}
	add("commit", "done", "config.toml: server.runtime = "+to)
	rs.Step, rs.At, rs.Overlap, rs.OverlapBeyondTies = "done", time.Now().UTC(), report.Overlap, report.BeyondTies
	// What clean would delete of the source, measured once here for
	// doctor (ruling R43's size, without sizing it at every doctor run).
	if c, ok := src.(server.Cleaner); ok {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), switchProbeWait)
		if n, err := c.CleanSize(sctx); err == nil {
			rs.KeptBytes, rs.KeptAt = n, time.Now().UTC()
		}
		cancel()
	}
	if err := state.Update(d, "server runtime switch", func(s *state.State) error { s.RuntimeSwitch = &rs; return nil }); err != nil {
		res.Warnings = append(res.Warnings, "the switch passed, but state.json does not say so ("+err.Error()+")")
	}
	if err := appendSwitchLog(d.State, rs); err != nil {
		res.Warnings = append(res.Warnings, "the switch passed, but "+switchLog(d.State)+" was not written ("+err.Error()+")")
	}
	res.SourceKept = src.DataName()
	if s, ok := src.(server.Sizer); ok {
		if n, err := s.DataSize(ctx); err == nil {
			res.SourceKept += " (" + sizeText(n) + ")"
		}
	}
	return res, nil
}

// switchInputsUnchanged refuses, with nothing changed, a switch whose
// config.toml (its bytes, sha being the hash read before the downloads)
// or secrets changed since the pre-flight read them.
func switchInputsUnchanged(d paths.Dirs, sha string, cfg config.Config, sec config.Secrets, to string) error {
	_, now, err := fsutil.ReadHashed(d.ConfigFile())
	if err != nil {
		return blocked(err)
	}
	cfgNow, secNow, err := loadServerConfig()
	if err != nil {
		return err
	}
	rerun := "rerun backsight server runtime switch " + to + " to check it again"
	if now != sha || !reflect.DeepEqual(cfgNow, cfg) {
		return nothingChanged("%s changed while the switch downloaded what %s runs: %s", d.ConfigFile(), to, rerun)
	}
	if secNow != sec {
		return nothingChanged("%s changed while the switch downloaded what %s runs: %s", d.SecretsFile(), to, rerun)
	}
	return nil
}

// switchPlan is the switch's steps, as --plan and the prompt list them.
func switchPlan(from, to string, recoverOnly bool, shim string) [][2]string {
	if recoverOnly {
		return [][2]string{{"lock", "would take the maintenance lock and server.lock (hooks queue, jobs skip)"},
			{"recover", "would stop the target of the switch cut off and start " + from + " again"}}
	}
	steps := [][2]string{
		{"download", "would download what " + to + " runs, before anything stops (a failure changes nothing)"},
	}
	if shim != "" {
		steps = append(steps, [2]string{"shim", "would install the shim " + shim + ", which the native database unit runs before every start (server native-check)"})
	}
	steps = append(steps, [][2]string{
		{"lock", "would take the maintenance lock and server.lock until the end (hooks queue, jobs skip)"},
		{"probes", "would recall up to 20 recent memories of the bank by their own text"},
		{"stop-api", "would stop " + from + "'s API, its database running: nothing is written after the backup"},
		{"backup", "would back the server up and take its data's fingerprint"},
		{"verify", "would restore the archive into a throwaway " + to + " database and compare its counts"},
		{"stop-source", "would stop " + from + " (its data is kept until backsight server runtime clean)"},
		{"target", "would write " + to + "'s files (same secrets, same API port) and start its database alone"},
		{"restore", "would restore the archive into " + to},
	}...)
	if to == "native" {
		steps = append(steps, [2]string{"identity", "would record the native database's identity"})
	}
	return append(steps,
		[2]string{"gate", "would compare " + to + "'s data with " + from + "'s: any difference stops the switch and starts " + from + " again"},
		[2]string{"start", "would start " + to + " and wait for health"},
		[2]string{"recall", "would recall the same memories and report the overlap (never a failure)"},
		[2]string{"commit", "would write server.runtime = " + to + " to config.toml"})
}

// switchPreflight checks the target, read only: Docker and Compose, or
// the user session, lingering, the disk and the shim the db unit runs for
// native; what it still has to download (missing: Docker's images to
// pull, native's Downloads items); the shim to install (missing or
// dangling), "" when it runs.
func switchPreflight(ctx context.Context, dst server.Runtime, to string, d paths.Dirs, resume string) (detail string, missing []string, shim string, warnings []string, err error) {
	if to == "docker" {
		eng, err := dst.Engine(ctx)
		if err != nil {
			return "", nil, "", nil, nothingChanged("Docker is not usable here (%v): start Docker, or stay on the native runtime", err)
		}
		comp, err := dst.Orchestrator(ctx)
		if err != nil {
			return "", nil, "", nil, nothingChanged("Docker Compose is not usable here (%v)", err)
		}
		detail = "Docker " + eng.Version + ", Compose " + comp
	} else {
		if err := switchProbes.userManager(ctx); err != nil {
			if errors.Is(err, setup.ErrSessionSlow) {
				return "", nil, "", nil, nothingChanged("%v: cannot tell whether the native server's units can run", err)
			}
			return "", nil, "", nil, &output.Error{Code: output.NeedsUser, Message: setup.NoUserBus + " (" + err.Error() + "): the native server runs as systemd user units: nothing was changed",
				Actions: []output.Action{{Kind: "relogin", Cmd: resume, Why: "the native server's units load into this user's systemd user manager"}}}
		}
		v, err := switchProbes.linger()
		if err != nil || strings.TrimSpace(v) != "yes" {
			why := "lingering is off for your user"
			if err != nil {
				why = "cannot tell whether lingering is on for your user (" + err.Error() + ")"
			}
			return "", nil, "", nil, &output.Error{Code: output.NeedsUser, Resume: resume,
				Message: why + ": the native server's units would stop at logout and not start at boot: run loginctl enable-linger, then rerun: nothing was changed",
				Actions: []output.Action{{Kind: "run_as_user", Cmd: "loginctl enable-linger", Why: "the native server runs as your systemd user units, which stop at logout without lingering"}}}
		}
		dir := d.Data
		for {
			if _, err := os.Stat(dir); err == nil || filepath.Dir(dir) == dir {
				break
			}
			dir = filepath.Dir(dir)
		}
		free, err := switchProbes.freeBytes(dir)
		switch {
		case err != nil:
			warnings = append(warnings, "free disk space unknown: "+err.Error())
		case free < setup.NativeDiskMB<<20:
			return "", nil, "", nil, nothingChanged("only %.1f GB free under %s, the native server needs %.1f GB: free some space", float64(free)/(1<<30), dir, float64(setup.NativeDiskMB)/1000)
		}
		detail = "user session, lingering on, disk"
		// The db unit's ExecStartPre is the shim: one missing or dangling
		// is installed after the consent, as server install does; one that
		// runs a backsight without native-check would fail every start.
		if shim = nativeShim(d); shimMissing(shim) {
			detail += "; the shim " + shim + " to install"
		} else if err := switchProbes.shimRuns(ctx, shim); err != nil {
			exe, _ := selfExe()
			return "", nil, "", nil, nothingChanged("the native database unit runs %s server native-check before every start, and %v: "+
				"point the shim at this backsight first (%s client install), then switch", shim, err, exe)
		} else {
			shim = ""
			detail += ", the shim"
		}
	}
	if dl, ok := dst.(server.Downloader); ok {
		var models bool
		if missing, models, err = dl.Downloads(ctx); err != nil {
			return "", nil, "", nil, nothingChanged("cannot tell what %s still has to download (%v)", to, err)
		}
		if models { // Docker: inside the server, at its first start
			warnings = append(warnings, fmt.Sprintf("the models (about %d MB) download at the %s server's first start", server.ModelsMB, to))
		}
	}
	if len(missing) > 0 {
		detail += fmt.Sprintf("; to download: %s (about %d MB)", strings.Join(missing, ", "), server.DownloadMB(missing, false))
	} else {
		detail += "; nothing to download"
	}
	return detail, missing, shim, warnings, nil
}

// switchFetch downloads what the target runs without changing what runs:
// Docker pulls the images it lacks (by reference: no compose.yaml needed
// on an install that never ran Docker), native installs uv, Python, the
// venv, pg0's PostgreSQL (unpacked when missing lists it) and the models.
// missing is what the preflight found to fetch.
func switchFetch(ctx context.Context, e *env, dst server.Runtime, missing []string) (string, error) {
	if dst.Kind() != "docker" {
		if err := dst.Fetch(ctx); err != nil {
			return "", err
		}
		what := "uv, Python, the venv " + dst.Version()
		if slices.Contains(missing, server.NativeUnpack) {
			what += ", pg0's PostgreSQL unpacked"
		}
		return what + " and the models in place", nil
	}
	images := missing
	if len(images) == 0 {
		return "the images are here", nil
	}
	dl, ok := dst.(server.Downloader)
	if !ok {
		return "", dst.FetchAll(ctx)
	}
	w := io.Discard
	if !e.json {
		w = e.stderr
	}
	if err := dl.Download(ctx, images, w); err != nil {
		return "", err
	}
	return "pulled " + strings.Join(images, ", "), nil
}

// stopTarget stops a switch's target, which must not start at boot
// (native: its units disabled). A target whose files were never written
// runs nothing: Docker without its compose.yaml has nothing to stop.
func stopTarget(ctx context.Context, rt server.Runtime) error {
	if v, err := rt.Deployed(); err == nil && v == "" {
		return nil
	}
	return rt.Stop(ctx)
}

// putSourceBack undoes a switch that was cut off: its target stopped
// (native: disabled), its source started and healthy. Not cancellable.
func putSourceBack(ctx context.Context, cfg config.Config, sec config.Secrets, cut *state.RuntimeSwitch) error {
	ctx = context.WithoutCancel(ctx)
	sctx, cancel := context.WithTimeout(ctx, rollbackStepTimeout)
	err := stopTarget(sctx, runtimeOfKind(cfg, cut.To))
	cancel()
	if err != nil {
		return fmt.Errorf("stopping %s: %w", cut.To, err)
	}
	src := runtimeOfKind(cfg, cut.From)
	sctx, cancel = context.WithTimeout(ctx, rollbackStepTimeout)
	defer cancel()
	if err := src.Start(sctx); err != nil {
		return fmt.Errorf("starting %s: %w", cut.From, err)
	}
	_, err = waitHealthy(sctx, src, configOfKind(cfg, cut.From), sec)
	return err
}

// rollbackSwitch stops the target and starts the source as it was, even
// when the switch was interrupted (Ctrl-C, a job's SIGTERM): the source's
// data was never touched, so it runs again as before. config.toml was not
// written. A rollback that fails leaves runtime_switch running, for doctor.
func rollbackSwitch(ctx context.Context, d paths.Dirs, res *switchResult, cfg config.Config, sec config.Secrets, src, dst server.Runtime,
	rs state.RuntimeSwitch, at string, cause error, add func(name, status, detail string)) error {
	ctx = context.WithoutCancel(ctx)
	kept := ""
	if res.Backup != "" {
		kept = "; the backup " + res.Backup + " is kept"
	}
	sctx, cancel := context.WithTimeout(ctx, rollbackStepTimeout)
	stopErr := stopTarget(sctx, dst)
	cancel()
	sctx, cancel = context.WithTimeout(ctx, rollbackStepTimeout)
	defer cancel()
	startErr := src.Start(sctx)
	if startErr == nil {
		_, startErr = waitHealthy(sctx, src, cfg, sec)
	}
	if stopErr != nil || startErr != nil {
		add("rollback", "failed", server.Redact(errors.Join(stopErr, startErr).Error(), sec))
		what := ""
		if stopErr != nil {
			what = fmt.Sprintf("%s could not be stopped (%v); ", res.To, stopErr)
		}
		if startErr != nil {
			what += fmt.Sprintf("%s did not start again (%v); ", res.From, startErr)
		}
		// runtime_switch stays running: doctor names the way back.
		return redacted(&output.Error{Code: output.Blocked, Message: fmt.Sprintf("the switch to %s failed at %s (%v), and putting %s back failed: %s"+
			"make sure %s is stopped (%s), then run backsight server start, then backsight server status%s",
			res.To, at, cause, res.From, what, res.To, dst.StopCmd(), kept)}, sec)
	}
	add("rollback", "done", res.To+" stopped, "+res.From+" runs again")
	rs.Step, rs.At = "rolled-back", time.Now().UTC()
	if err := state.UpdateWithin(d, "server runtime switch", recordWait, func(s *state.State) error { s.RuntimeSwitch = &rs; return nil }); err != nil {
		res.Warnings = append(res.Warnings, "state.json still says the switch runs ("+err.Error()+"): doctor reports it until the next switch")
	}
	return redacted(&output.Error{Code: output.Blocked, Message: fmt.Sprintf("the switch to %s failed at %s (%v): %s runs again as before%s", res.To, at, cause, res.From, kept)}, sec)
}

// takeProbes recalls up to probeCount of the bank's latest memories,
// whatever their project (ruling R14), each by its own text, and keeps
// their top 10. A failure is a warning: recall is reported, never gated.
func takeProbes(ctx context.Context, cl *hindsight.Client, bank string) ([]recallProbe, string) {
	mems, err := cl.Recent(ctx, bank, "", "", probeCount)
	if err != nil {
		return nil, "recall not compared: listing the recent memories: " + err.Error()
	}
	var out []recallProbe
	for _, m := range mems {
		if len(out) == probeCount {
			break
		}
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		top, err := recallTop(ctx, cl, bank, m.Text)
		if err != nil {
			return out, "recall compared on " + fmt.Sprint(len(out)) + " memories only: " + err.Error()
		}
		out = append(out, recallProbe{Memory: m.ID, Text: m.Text, Top: top})
	}
	return out, ""
}

// recallTop is the ids of the first 10 hits of a recall of text.
func recallTop(ctx context.Context, cl *hindsight.Client, bank, text string) ([]string, error) {
	hits, err := recallHits(ctx, cl, bank, text)
	if err != nil {
		return nil, err
	}
	var top []string
	for _, h := range hits[:min(10, len(hits))] {
		top = append(top, h.ID)
	}
	return top, nil
}

// recallHits is every hit of a probe's recall, best first.
func recallHits(ctx context.Context, cl *hindsight.Client, bank, text string) ([]hindsight.RecallHit, error) {
	return cl.Recall(ctx, bank, hindsight.RecallRequest{Query: text, Budget: "mid", MaxTokens: 4096})
}

// recallAgain runs the probes on the target: the mean over the probes of
// the share of their top 10 found again in its top 10 (raw), and also
// past it in a near-tie (BeyondTies), and how many find their own memory
// first. A recall that fails is the target not serving: an error. Without
// probes, one recall checks it answers.
func recallAgain(ctx context.Context, cl *hindsight.Client, bank string, probes []recallProbe) (probeReport, error) {
	if len(probes) == 0 {
		_, err := cl.Recall(ctx, bank, hindsight.RecallRequest{Query: "backsight runtime switch check", Budget: "low", MaxTokens: 512})
		return probeReport{}, err
	}
	r := probeReport{Queries: len(probes)}
	sum, sumTies := 0.0, 0.0
	for _, p := range probes {
		hits, err := recallHits(ctx, cl, bank, p.Text)
		if err != nil {
			return r, fmt.Errorf("recall: %w", err)
		}
		common, ties := 0, 0
		for _, was := range p.Top {
			i := slices.IndexFunc(hits, func(h hindsight.RecallHit) bool { return h.ID == was })
			switch {
			case i < 0:
			case i < 10:
				common++
			case hits[i].Scores.Final >= hits[9].Scores.Final-nearTie:
				ties++
			}
		}
		n := float64(max(1, len(p.Top)))
		sum += float64(common) / n
		sumTies += float64(common+ties) / n
		r.NearTies += ties
		if len(hits) > 0 && hits[0].ID == p.Memory {
			r.OwnAtRank1++
		}
	}
	r.Overlap = sum / float64(len(probes))
	r.BeyondTies = sumTies / float64(len(probes))
	return r, nil
}

// appendSwitchLog records a passed switch, one JSON line.
func appendSwitchLog(stateDir string, rs state.RuntimeSwitch) error {
	p := switchLog(stateDir)
	if err := fsutil.MkdirPrivate(filepath.Dir(p)); err != nil {
		return err
	}
	line, err := json.Marshal(map[string]any{"from": rs.From, "to": rs.To, "at": rs.At, "backup": rs.Backup, "overlap": rs.Overlap, "overlap_beyond_near_ties": rs.OverlapBeyondTies, "gate_ok": true})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// sizeText is n bytes for a reader.
func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func switchText(r switchResult) string {
	var b strings.Builder
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning  %s\n", w)
	}
	switch {
	case r.Plan:
		fmt.Fprintf(&b, "\nplan only: nothing was changed\n")
	case r.Declined:
		fmt.Fprintf(&b, "\nnothing was changed\n")
	case r.SourceKept != "":
		fmt.Fprintf(&b, "\nthe memory server runs on %s; the backup %s is kept, and so is %s until backsight server runtime clean\n", r.To, r.Backup, r.SourceKept)
	case len(r.Steps) == 1 && r.Steps[0].Name == "check":
		fmt.Fprintf(&b, "the server is already on %s: nothing to do\n", r.To)
	}
	return b.String()
}

func newServerRuntimeCleanCmd(e *env) *cobra.Command {
	var f switchFlags
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Delete the other runtime's server and data, kept since a switch (asks first; backups are kept)",
		Long: "After server runtime switch, the runtime switched from keeps its data (the Docker volumes, or server/native). clean shows what " +
			"it deletes and its size, asks, then deletes that runtime's server and data. The runtime in use and the backups are never touched.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := runClean(cmd.Context(), e, f)
			return e.report("server runtime clean", res, func() string { return cleanText(res) }, err)
		},
	}
	cmd.Flags().BoolVar(&f.plan, "plan", false, "show what would be deleted and its size without deleting it")
	cmd.Flags().BoolVar(&f.nonInteractive, "non-interactive", false, "never prompt: exit 10 when the consent is missing")
	cmd.Flags().StringArrayVar(&f.answers, "answer", nil, "answer a question: consent=yes (delete without the prompt)")
	return cmd
}

func runClean(ctx context.Context, e *env, f switchFlags) (cleanResult, error) {
	res := cleanResult{Plan: f.plan}
	agreed, err := consentAnswer(f.answers, "server runtime clean")
	if err != nil {
		return res, err
	}
	cfg, _, err := loadServerConfig()
	if err != nil {
		return res, err
	}
	if err := requireLocal(cfg, "server runtime clean"); err != nil {
		return res, err
	}
	d, _, err := machine()
	if err != nil {
		return res, err
	}
	st, err := state.Load(d.StateFile())
	if err != nil {
		return res, blocked(err)
	}
	if rs := st.RuntimeSwitch; rs != nil && rs.Step == "running" {
		return res, nothingChanged("a runtime switch to %s runs or was cut off: finish it first (backsight server runtime switch %s)", rs.To, rs.To)
	}
	res.Runtime = otherRuntime(cfg.RuntimeKind())
	rt := runtimeOfKind(cfg, res.Runtime)
	// What clean shows is everything Delete removes (ruling R43).
	exists, what, size := false, rt.DataName(), func(context.Context) (int64, error) { return 0, errors.New("unknown") }
	if c, ok := rt.(server.Cleaner); ok {
		exists, err = c.Leftover(ctx)
		what, size = c.CleanWhat(), c.CleanSize
	} else {
		exists, err = rt.DataExists(ctx)
		if s, ok := rt.(server.Sizer); ok {
			size = s.DataSize
		}
	}
	if errors.Is(err, exec.ErrNotFound) { // no Docker on this machine: nothing of it here
		exists, err = false, nil
	}
	if err != nil {
		return res, blocked(err)
	}
	if !exists {
		deployed, err := rt.Deployed()
		if err != nil {
			return res, blocked(err)
		}
		if deployed == "" {
			return res, nil
		}
	}
	res.What = what
	if res.Bytes, err = size(ctx); err != nil {
		res.Bytes, res.SizeUnknown = 0, true
	}
	if f.plan {
		return res, nil
	}
	if !agreed {
		interactive := !e.json && !f.nonInteractive && stdinIsTerminal() && stdoutIsTerminal()
		if !interactive {
			return res, &output.Error{Code: output.NeedsUser, Resume: f.command("clean", "") + " --answer consent=yes",
				Questions: []setup.Question{{ID: "consent", Required: true, Prompt: "Delete " + res.What + " (" + res.size() + ")? Backups are kept",
					Options: []setup.Option{{Value: "yes", Label: "delete " + res.Runtime + "'s server and data"}}}},
				Message: "deleting " + res.What + " needs your go: rerun with --answer consent=yes: nothing was deleted"}
		}
		if !confirm(e, "Delete "+res.What+" ("+res.size()+")? Backups are kept. [y/N] ") {
			res.Declined = true
			return res, nil
		}
	}
	unlock, _, err := serverMaintenanceLock(d, "server runtime clean")
	if err != nil {
		return res, err
	}
	defer unlock()
	if err := rt.Delete(ctx); err != nil {
		return res, blocked(fmt.Errorf("deleting %s: %w", res.What, err))
	}
	res.Deleted = true
	return res, nil
}

// size is the size clean shows: "size unknown" when the runtime could
// not tell, never "0 bytes".
func (r cleanResult) size() string {
	if r.SizeUnknown {
		return "size unknown"
	}
	return sizeText(r.Bytes)
}

func cleanText(r cleanResult) string {
	switch {
	case r.What == "":
		return "nothing to clean: the " + r.Runtime + " runtime has no server or data here\n"
	case r.Plan:
		return "would delete " + r.What + " (" + r.size() + "), the " + r.Runtime + " runtime's, kept since the switch; backups are kept\n"
	case r.Declined:
		return "nothing was deleted\n"
	case r.Deleted:
		return "deleted " + r.What + " (" + r.size() + "); backups are kept\n"
	}
	return ""
}

type fingerprintResult struct {
	server.Fingerprint
	KeysFile string   `json:"keys_file,omitempty"`
	Missing  []string `json:"missing_keys,omitempty"`
}

// newServerRuntimeFingerprintCmd prints the running server's fingerprint,
// the switch's gate (decision 0078's round trip compares them by hand).
func newServerRuntimeFingerprintCmd(e *env) *cobra.Command {
	var keysOut, compare string
	cmd := &cobra.Command{
		Use:    "fingerprint",
		Short:  "Print the server's data fingerprint (the switch's gate); --keys writes every table's keys, --compare names those gone since",
		Args:   usageArgs(cobra.NoArgs),
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadServerConfig()
			if err != nil {
				return err
			}
			if err := requireLocal(cfg, "server runtime fingerprint"); err != nil {
				return err
			}
			ctx := cmd.Context()
			db := runtimeFor(cfg).Database()
			var res fingerprintResult
			if res.Fingerprint, err = server.TakeFingerprint(ctx, db, cfg.Server.Bank); err != nil {
				return blocked(err)
			}
			var failed error
			if keysOut != "" || compare != "" {
				keys, err := server.TakeKeys(ctx, db)
				if err != nil {
					return blocked(err)
				}
				if keysOut != "" {
					raw, _ := json.Marshal(keys)
					if err := fsutil.WriteAtomic(keysOut, raw, 0o600); err != nil {
						return blocked(err)
					}
					res.KeysFile = keysOut
				}
				if compare != "" {
					raw, err := os.ReadFile(compare)
					if err != nil {
						return &output.Error{Code: output.Usage, Message: err.Error()}
					}
					var before map[string][]string
					if err := json.Unmarshal(raw, &before); err != nil {
						return &output.Error{Code: output.Usage, Message: compare + ": " + err.Error()}
					}
					if res.Missing = server.MissingKeys(before, keys); len(res.Missing) > 0 {
						failed = &output.Error{Code: output.Blocked, Message: fmt.Sprintf("%d key(s) of %s are gone: %s", len(res.Missing), compare, strings.Join(res.Missing, ", "))}
					}
				}
			}
			return e.report("server runtime fingerprint", res, func() string {
				raw, _ := json.MarshalIndent(res.Fingerprint, "", "  ")
				s := string(raw) + "\n"
				if compare != "" && len(res.Missing) == 0 {
					s += "no key of " + compare + " is gone\n"
				}
				return s
			}, failed)
		},
	}
	cmd.Flags().StringVar(&keysOut, "keys", "", "also write every table's keys to this file (mode 0600)")
	cmd.Flags().StringVar(&compare, "compare", "", "name the keys of this file (written by --keys) that are gone now; exit 20 when any is")
	return cmd
}
