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

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/reference"
	"github.com/Barben360/backsight/internal/retain"
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
	Getenv     func(string) string
	ManagedDir string
	Docker     func(context.Context) (string, error)
	GPUs       func() []string
	FreeBytes  func(path string) (uint64, error)
	// ClaudePlugins runs `claude plugin list --json` in a profile ("" =
	// default); nil skips asking Claude Code.
	ClaudePlugins func(profile string) ([]byte, error)
	Timeout       time.Duration // per Hindsight call
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
	} else {
		add(Check{ID: "untagged", Status: Info, Summary: "skipped: " + skipReason(srv)})
	}
	add(checkMaintenance(env.Dirs))
	add(checkWrites(env.Dirs))
	add(checkDisk(env))
	add(checkReference(ref, refErr))
	add(checkRetention(loc, env.ManagedDir))
	add(checkClient(env, loc))
	add(checkGPU(env.GPUs()))
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
		c.Detail = "expected at " + env.Dirs.ConfigFile() + "; until setup exists (M4) doctor inspects the reference install"
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
		c.Fix = "start the server (the reference stack: docker compose up -d in its directory)"
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

// checkWrites reports what has not reached the server: queued notes and
// quarantined sessions and notes (SPEC §15).
func checkWrites(d paths.Dirs) Check {
	c := Check{ID: "writes", Status: OK}
	ob := retain.Outbox{Path: filepath.Join(d.OutboxDir(), "notes.jsonl"), QuarantinePath: filepath.Join(d.QuarantineDir(), "notes.jsonl")}
	pending, _ := ob.Pending()
	qNotes, _ := ob.Quarantined()
	qSessions, _ := retain.QuarantinedSessions(d.CursorsDir())
	var size int64
	if fi, err := os.Stat(ob.Path); err == nil {
		size = fi.Size()
	}
	c.Summary = fmt.Sprintf("%d note(s) queued, %d session(s) and %d note(s) quarantined", pending, len(qSessions), qNotes)
	switch {
	case size >= retain.OutboxMaxBytes:
		c.Status, c.Fix = Error, "the notes outbox is full: bring the server back (backsight server status), then backsight jobs tick"
	case len(qSessions)+qNotes > 0:
		c.Status = Warn
		c.Detail = "the server refused them; reasons in " + d.QuarantineDir()
		c.Fix = "fix the cause, then backsight retain retry-quarantine"
	case size >= retain.OutboxWarnBytes:
		c.Status, c.Fix = Warn, "the server has not accepted notes for a while: backsight server status"
	}
	return c
}

// checkMaintenance reports the maintenance lock (§11.4): owner when held,
// and a stale lock as such, never removed here.
func checkMaintenance(d paths.Dirs) Check {
	c := Check{ID: "maintenance", Status: OK, Summary: "no maintenance in progress"}
	l, err := state.TryAcquire(d.MaintenanceLock(), "doctor")
	var held *state.HeldError
	switch {
	case err == nil:
		_ = l.Release()
	case errors.As(err, &held) && held.Stale:
		c.Status, c.Summary, c.Detail = Warn, "stale maintenance lock", held.Error()
		c.Fix = "check that no backsight update, backup or restore is running, then remove " + held.Path
	case errors.As(err, &held):
		c.Status, c.Summary = Info, fmt.Sprintf("maintenance in progress: %s (pid %d, since %s)", held.Owner.Command, held.Owner.PID, held.Owner.Since.Format(time.RFC3339))
	default:
		c.Status, c.Summary, c.Detail = Warn, "cannot check the maintenance lock", err.Error()
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
	err := cl.EachMemory(ctx, bank, "observation", func(m hindsight.Memory) error {
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
		c.Detail = "untagged observations are invisible to project recall (SPEC §4)"
		c.Fix = "report it upstream (backsight report, M5); a fresh backsight bank prevents it for its own writes"
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
		c.Status, c.Summary = OK, "no reference install wired into Claude Code"
	default:
		lines := make([]string, len(in.Pieces))
		for i, p := range in.Pieces {
			lines[i] = p.ID + " (" + p.Where + ")"
		}
		c.Status = Info
		c.Summary = fmt.Sprintf("%d reference pieces wired into Claude Code, retired at the %s cut-over", len(in.Pieces), in.Pieces[0].ReplacedIn)
		c.Detail = strings.Join(lines, "; ")
	}
	return c
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
	c.Detail += "; transcripts are the only source for backfill and rebuilds (decision 0041)"
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
		c.Summary = "no GPU detected; acceleration comes in v2 (decision 0035)"
		return c
	}
	c.Summary = strings.Join(gpus, ", ") + "; used from v2 (decision 0035)"
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

// DockerVersion asks the Docker daemon for its version.
func DockerVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
