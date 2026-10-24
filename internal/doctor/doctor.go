// Package doctor runs backsight's read-only health checks (SPEC §15). In M0
// it works with a backsight config or with the reference install it will
// replace, and changes nothing.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/reference"
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

// Target is the Hindsight server doctor inspected.
type Target struct {
	URL    string `json:"url"`
	Bank   string `json:"bank"`
	Source string `json:"source"` // config | reference
	token  string
}

type Report struct {
	Checks   []Check `json:"checks"`
	Server   *Target `json:"server,omitempty"`
	Errors   int     `json:"errors"`
	Warnings int     `json:"warnings"`
}

// Env is everything doctor reads; tests replace the probes.
type Env struct {
	Dirs       paths.Dirs
	Home       string
	Getenv     func(string) string
	ManagedDir string
	Docker     func(context.Context) (string, error)
	GPUs       func() []string
	Timeout    time.Duration // per Hindsight call
}

// retentionFloor is the retention below which doctor warns (decision 0041).
const retentionFloor = 365

// Run performs every check in a fixed order.
func Run(ctx context.Context, env Env) Report {
	var r Report
	add := func(c Check) {
		r.Checks = append(r.Checks, c)
		switch c.Status {
		case Error:
			r.Errors++
		case Warn:
			r.Warnings++
		}
	}

	cfgCheck, target := checkConfig(env)
	add(cfgCheck)
	add(checkDocker(ctx, env))

	loc := claudecode.Locate(env.Home, env.Getenv)
	ref, refErr := reference.Discover(env.Home, loc)
	if target == nil && ref != nil && ref.URL != "" {
		target = &Target{URL: ref.URL, Bank: ref.Bank, Source: "reference", token: string(ref.Token)}
	}
	r.Server = target
	for _, c := range checkServer(ctx, env, target) {
		add(c)
	}
	add(checkReference(ref, refErr))
	add(checkRetention(loc, env.ManagedDir))
	add(checkGPU(env.GPUs()))
	return r
}

func checkConfig(env Env) (Check, *Target) {
	c := Check{ID: "config"}
	path := env.Dirs.ConfigFile()
	cfg, err := config.Load(path, env.Dirs, env.Home)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		c.Status, c.Summary = Info, "backsight is not set up yet"
		c.Detail = "expected at " + path + "; until setup exists (M4) doctor inspects the reference install"
		return c, nil
	case err != nil:
		c.Status, c.Summary, c.Detail = Error, "config.toml is invalid", err.Error()
		c.Fix = "correct the key named in the detail in " + path
		return c, nil
	}
	if err := cfg.Validate(); err != nil {
		c.Status, c.Summary, c.Detail = Error, "config.toml is invalid", err.Error()
		c.Fix = "correct the key named in the detail in " + path
		return c, nil
	}
	sec, err := config.LoadSecrets(env.Dirs.SecretsFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.Status, c.Summary, c.Detail = Error, "secrets.toml cannot be used", err.Error()
		c.Fix = "chmod 600 " + env.Dirs.SecretsFile()
		return c, nil
	}
	c.Status, c.Summary = OK, "config "+path
	return c, &Target{URL: cfg.Server.URL, Bank: cfg.Server.Bank, Source: "config", token: sec.APIKey}
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

func checkServer(ctx context.Context, env Env, t *Target) []Check {
	skipped := func(why string) []Check {
		return []Check{{ID: "bank", Status: Info, Summary: "skipped: " + why}, {ID: "untagged", Status: Info, Summary: "skipped: " + why}}
	}
	if t == nil {
		return append([]Check{{ID: "server", Status: Info, Summary: "no server configured"}}, skipped("no server")...)
	}
	srv := Check{ID: "server"}
	cl, err := hindsight.New(t.URL, t.token, env.Timeout)
	if err != nil {
		srv.Status, srv.Summary = Error, err.Error()
		return append([]Check{srv}, skipped("no usable server URL")...)
	}
	var apiErr *hindsight.APIError
	if _, err = cl.Health(ctx); err == nil {
		var v hindsight.Version
		if v, err = cl.Version(ctx); err == nil {
			srv.Status, srv.Summary = OK, fmt.Sprintf("Hindsight %s at %s (from %s)", v.APIVersion, t.URL, t.Source)
		}
	}
	switch {
	case err == nil:
	case errors.Is(err, hindsight.ErrUnreachable):
		srv.Status, srv.Summary = Error, "Hindsight unreachable at "+t.URL+" (from "+t.Source+")"
		srv.Fix = "start the server (the reference stack: docker compose up -d in its directory)"
	case errors.As(err, &apiErr) && apiErr.Kind == hindsight.Unauthorized:
		srv.Status, srv.Summary = Error, "the server rejected the API key (from "+t.Source+")"
		srv.Fix = "check the key the " + t.Source + " uses against the server's"
	default:
		srv.Status, srv.Summary, srv.Detail = Error, "Hindsight at "+t.URL+" answered with an error", err.Error()
	}
	if srv.Status != OK {
		return append([]Check{srv}, skipped("server not usable")...)
	}

	bank := Check{ID: "bank"}
	banks, err := cl.Banks(ctx)
	found := false
	for _, b := range banks {
		found = found || b.ID == t.Bank
	}
	switch {
	case err != nil:
		bank.Status, bank.Summary, bank.Detail = Error, "cannot list banks", err.Error()
	case !found:
		bank.Status, bank.Summary = Error, "bank "+t.Bank+" does not exist on the server"
		bank.Fix = "check the bank id in the " + t.Source
	}
	if bank.Status != "" {
		return []Check{srv, bank, {ID: "untagged", Status: Info, Summary: "skipped: bank not usable"}}
	}
	st, err := cl.BankStats(ctx, t.Bank)
	if err != nil {
		bank.Status, bank.Summary, bank.Detail = Error, "cannot read bank "+t.Bank, err.Error()
		return []Check{srv, bank, {ID: "untagged", Status: Info, Summary: "skipped: bank not usable"}}
	}
	bank.Status = OK
	bank.Summary = fmt.Sprintf("bank %s: %d memories, %d documents, %d awaiting consolidation", t.Bank, st.TotalNodes, st.TotalDocuments, st.PendingConsolidation)
	if st.FailedOperations > 0 || st.FailedConsolidation > 0 {
		bank.Status = Warn
		bank.Detail = fmt.Sprintf("%d failed operations, %d failed consolidations", st.FailedOperations, st.FailedConsolidation)
		bank.Fix = "open the Hindsight UI operations page to see the provider errors"
	}
	return []Check{srv, bank, checkUntagged(ctx, cl, t.Bank)}
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
	tr, _ := claudecode.ScanTranscripts(loc.Projects)
	c.Summary = fmt.Sprintf("Claude Code keeps transcripts %d days (%s)", ret.EffectiveDays, ret.Source)
	c.Detail = fmt.Sprintf("%d transcript(s), %.1f MB", tr.Count, float64(tr.Bytes)/(1<<20))
	if !tr.Oldest.IsZero() {
		c.Detail += ", oldest " + tr.Oldest.UTC().Format("2006-01-02")
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
		c.Fix = `set "cleanupPeriodDays": 3650 in ` + loc.Settings + " (backsight client retention will do it, M1)"
	}
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
