package setup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
)

// Check statuses. A failed check carries its exit class.
const (
	OK   = "ok"
	Info = "info"
	Warn = "warn"
	Fail = "fail"
)

// Check is one pre-flight line (SPEC §11.3 step 1): a fix and, when it
// fails, whether the environment blocks (exit 20) or the user must act
// (exit 10, with the action).
type Check struct {
	ID      string         `json:"id"`
	Status  string         `json:"status"`
	Summary string         `json:"summary"`
	Fix     string         `json:"fix,omitempty"`
	Class   string         `json:"class,omitempty"` // blocked | needs_user, on failure
	Action  *output.Action `json:"action,omitempty"`
}

// MinFreeBytes: the image, its models and the first months of data.
const MinFreeBytes = 6 << 30

// Probes are what pre-flight reads; the command fills them, tests fake them.
type Probes struct {
	GOOS, GOARCH string
	// Remote: the server runs elsewhere; the local Docker checks are skipped.
	Remote bool
	// Docker is the server runtime's engine version (" rootless" when
	// so); Compose its orchestrator's version.
	Docker  func(ctx context.Context) (string, error)
	Compose func(ctx context.Context) (string, error)
	// PortFree reports whether a loopback port can be bound.
	PortFree func(port int) bool
	// OwnPorts are the ports backsight's own server publishes now (its
	// compose project runs): adopted, or started by an earlier setup run
	// that stopped later (a failed LLM check). Those being taken is expected.
	OwnPorts []int
	// ServerInstalled: the compose project exists; the disk floor is then
	// a warning, the image is already there.
	ServerInstalled bool
	FreeBytes       func() (uint64, error)
	// Claude returns `claude --version`'s first word.
	Claude     func(ctx context.Context) (string, error)
	MinClaude  string
	AtLeast    func(v, minimum string) bool
	APIPort    int
	UIPort     int
	Reference  int    // reference pieces wired into Claude Code
	Retention  string // effective cleanupPeriodDays, described
	Transcript string // count, size and oldest, described
	GPUs       []string
	// UserScheduler reaches the scheduler the scheduler step installs
	// into (systemd's user manager, over the user bus); nil where there
	// is nothing to reach first. SchedulerOff: the answer skips that step.
	UserScheduler func(ctx context.Context) error
	SchedulerOff  bool
	// ServerOnly: a host without Claude Code (setup --role server); the
	// Claude Code checks and lines are skipped.
	ServerOnly bool
	// Runtime is the server's runtime: "" or "docker", or "native"
	// (decision 0078), whose checks replace Docker's: Linux x86_64, the
	// user manager (UserManager) and lingering (Linger: loginctl's
	// Linger property, "yes" or "no").
	Runtime     string
	Linger      func() (string, error)
	UserManager func(ctx context.Context) error
	// Native on Windows (decision 0080) checks instead: the venv root's
	// length (VenvRoot), the paths the server's task names (NativePaths:
	// the data root, the native root and the shim), conhost --headless
	// (Conhost) and Task Scheduler (Task: a probe task created then
	// deleted). A nil probe fails its check.
	VenvRoot    string
	NativePaths []string
	Conhost     func(ctx context.Context) error
	Task        func(ctx context.Context) error
}

// NativeDiskMB: Python, the venv, the models and the first months of
// data on the native runtime.
const NativeDiskMB = 3500

// NoUserBus says where systemd's user manager can be reached.
const NoUserBus = "no systemd user session is reachable from this shell: run it from a login session of this user (an ssh login, not su or sudo)"

// nativePlatformFix is the native runtime's way out on another platform.
const nativePlatformFix = "use the docker runtime (setup without --runtime native), or run the server on a Linux x86_64 machine and connect this PC to it"

// Preflight runs every check in order.
func Preflight(ctx context.Context, p Probes) []Check {
	var out []Check
	add := func(c Check) { out = append(out, c) }

	switch {
	case (p.GOOS == "linux" || p.GOOS == "darwin" || p.GOOS == "windows") && (p.GOARCH == "amd64" || p.GOARCH == "arm64"):
		add(Check{ID: "platform", Status: OK, Summary: p.GOOS + "/" + p.GOARCH})
	default:
		add(Check{ID: "platform", Status: Fail, Class: "blocked", Summary: p.GOOS + "/" + p.GOARCH + " has no server image",
			Fix: "run the server on a linux, macOS or Windows machine (amd64 or arm64) and connect this client to it"})
	}

	native := p.Runtime == "native" && !p.Remote
	if native {
		out[0] = nativePlatform(p)
	}

	switch {
	case p.Remote:
		add(Check{ID: "docker", Status: Info, Summary: "remote server: Docker is checked on its host"})
	case native && p.GOOS == "windows":
		add(Check{ID: "docker", Status: Info, Summary: "not used by the native runtime"})
		add(Check{ID: "compose", Status: Info, Summary: "not used by the native runtime"})
		add(diskCheck(p))
		add(pathLengthCheck(p.VenvRoot))
		add(pathCharsCheck(p.NativePaths))
		add(portCheck(p, "api_port", p.APIPort))
		add(conhostCheck(ctx, p))
		add(taskCheck(ctx, p))
	case native:
		add(Check{ID: "docker", Status: Info, Summary: "not used by the native runtime"})
		add(Check{ID: "compose", Status: Info, Summary: "not used by the native runtime"})
		add(userManagerCheck(ctx, p))
		add(lingerCheck(p))
		add(portCheck(p, "api_port", p.APIPort))
		add(diskCheck(p))
	default:
		add(dockerCheck(ctx, p))
		add(composeCheck(ctx, p))
		for _, port := range []struct {
			id string
			n  int
		}{{"api_port", p.APIPort}, {"ui_port", p.UIPort}} {
			add(portCheck(p, port.id, port.n))
		}
		add(diskCheck(p))
	}
	if !p.ServerOnly {
		add(claudeCheck(ctx, p))
	}
	// On native, the systemd check above reaches the same user manager.
	if p.UserScheduler != nil && !p.SchedulerOff && !native {
		add(schedulerCheck(ctx, p))
	}
	if !p.ServerOnly {
		if p.Reference > 0 {
			add(Check{ID: "reference", Status: Info, Summary: fmt.Sprintf("%d piece(s) of an older Hindsight install wired into Claude Code (see the reference question)", p.Reference)})
		}
		if p.Retention != "" {
			add(Check{ID: "retention", Status: Info, Summary: p.Retention})
		}
		if p.Transcript != "" {
			add(Check{ID: "transcripts", Status: Info, Summary: p.Transcript})
		}
	}
	gpu := Check{ID: "gpu", Status: Info, Summary: "no GPU found: CPU inference"}
	if len(p.GPUs) > 0 {
		gpu.Summary = "GPU found (" + strings.Join(p.GPUs, ", ") + "): reported only, backsight runs on the CPU"
	}
	add(gpu)
	return out
}

func dockerCheck(ctx context.Context, p Probes) Check {
	c := Check{ID: "docker"}
	v, err := p.Docker(ctx)
	text := strings.ToLower(v)
	if err != nil {
		text += " " + strings.ToLower(err.Error())
	}
	switch {
	case errors.Is(err, exec.ErrNotFound):
		c.Status, c.Class, c.Summary = Fail, "needs_user", "Docker is not installed"
		c.Fix = "install Docker Engine (Linux) or Docker Desktop: https://docs.docker.com/get-docker/"
		c.Action = &output.Action{Kind: "install", Cmd: "https://docs.docker.com/get-docker/", Why: "the memory server runs in Docker Compose"}
	case strings.Contains(text, "permission denied"):
		c.Status, c.Class, c.Summary = Fail, "needs_user", "Docker is running but your user cannot use it without sudo"
		c.Fix = "sudo usermod -aG docker $USER, then log out and back in"
		c.Action = &output.Action{Kind: "relogin", Cmd: "sudo usermod -aG docker $USER", Why: "the docker group takes effect at the next login"}
	case err != nil:
		c.Status, c.Class, c.Summary = Fail, "blocked", "Docker is not running"
		c.Fix = "start Docker (systemctl start docker, or open Docker Desktop)"
	default:
		c.Status, c.Summary = OK, "Docker "+strings.TrimSpace(v)
		if strings.Contains(text, "rootless") {
			c.Summary += " (rootless)"
		}
	}
	return c
}

func composeCheck(ctx context.Context, p Probes) Check {
	c := Check{ID: "compose"}
	v, err := p.Compose(ctx)
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	if err != nil || major < 2 {
		c.Status, c.Class, c.Summary = Fail, "needs_user", "Docker Compose v2 is missing"
		c.Fix = "install the docker compose plugin: https://docs.docker.com/compose/install/"
		c.Action = &output.Action{Kind: "install", Cmd: "https://docs.docker.com/compose/install/", Why: "backsight runs the server as a compose project"}
		return c
	}
	c.Status, c.Summary = OK, "Docker Compose "+v
	return c
}

func portCheck(p Probes, id string, port int) Check {
	c := Check{ID: id}
	switch {
	case p.PortFree(port):
		c.Status, c.Summary = OK, fmt.Sprintf("port %d free", port)
	case slices.Contains(p.OwnPorts, port):
		c.Status, c.Summary = OK, fmt.Sprintf("port %d: backsight's server already runs", port)
	default:
		c.Status, c.Class, c.Summary = Fail, "blocked", fmt.Sprintf("port %d is taken by another program", port)
		c.Fix = fmt.Sprintf("free it, or choose another port: --answer %s=<port>", id)
	}
	return c
}

func diskCheck(p Probes) Check {
	c := Check{ID: "disk"}
	free, err := p.FreeBytes()
	gb := float64(free) / (1 << 30)
	floor, need := uint64(MinFreeBytes), "the server needs 6 GB"
	if p.Runtime == "native" {
		floor, need = NativeDiskMB<<20, fmt.Sprintf("the native server needs %.1f GB", float64(NativeDiskMB)/1000)
	}
	switch {
	case err != nil:
		c.Status, c.Summary = Warn, "free space unknown: "+err.Error()
	case free >= floor:
		c.Status, c.Summary = OK, fmt.Sprintf("%.1f GB free", gb)
	case p.ServerInstalled:
		c.Status, c.Summary = Warn, fmt.Sprintf("only %.1f GB free", gb)
		c.Fix = "free some space: the memory grows and backups need room"
	default:
		c.Status, c.Class, c.Summary = Fail, "blocked", fmt.Sprintf("only %.1f GB free, %s", gb, need)
		c.Fix = "free some space, or set BACKSIGHT_HOME to a larger disk"
	}
	return c
}

func claudeCheck(ctx context.Context, p Probes) Check {
	c := Check{ID: "claude"}
	v, err := p.Claude(ctx)
	switch {
	case err != nil:
		c.Status, c.Class, c.Summary = Fail, "needs_user", "the claude command is missing"
		c.Fix = "npm install -g @anthropic-ai/claude-code"
		c.Action = &output.Action{Kind: "install", Cmd: "npm install -g @anthropic-ai/claude-code", Why: "backsight wires itself into Claude Code through its plugin commands"}
	case !p.AtLeast(v, p.MinClaude):
		c.Status, c.Class, c.Summary = Fail, "needs_user", fmt.Sprintf("Claude Code %s is older than %s", v, p.MinClaude)
		c.Fix = "claude update"
		c.Action = &output.Action{Kind: "run_as_user", Cmd: "claude update", Why: "the plugin needs a recent Claude Code"}
	default:
		c.Status, c.Summary = OK, "Claude Code "+v
	}
	return c
}

// nativePlatform: the native runtime runs on Linux x86_64 and Windows
// x86_64 only in this release (decisions 0078, 0080).
func nativePlatform(p Probes) Check {
	if config.NativeOn(p.GOOS, p.GOARCH) {
		return Check{ID: "platform", Status: OK, Summary: p.GOOS + "/" + p.GOARCH}
	}
	return Check{ID: "platform", Status: Fail, Class: "blocked", Summary: p.GOOS + "/" + p.GOARCH + ": " + config.NativeRefusal, Fix: nativePlatformFix}
}

// userManagerCheck: the native server runs as two systemd user units, so
// without a reachable user manager there is nothing to run it, and no
// answer changes that (scheduler=off only skips the timers).
func userManagerCheck(ctx context.Context, p Probes) Check {
	c := Check{ID: "systemd"}
	var err error
	if p.UserManager == nil {
		err = errors.New("not probed")
	} else {
		err = p.UserManager(ctx)
	}
	switch {
	case errors.Is(err, ErrSessionSlow):
		c.Status, c.Class = Fail, "blocked"
		c.Summary = err.Error() + ": setup cannot tell whether it can run the native server's units"
		c.Fix = "retry, from a login session of this user (an ssh login, not su or sudo)"
	case err != nil:
		c.Status, c.Class = Fail, "needs_user"
		c.Summary = "no systemd user session here (" + err.Error() + "): the native runtime runs the server as systemd user units"
		c.Fix = NoUserBus + "; on a machine without one (a container), use the docker runtime"
		c.Action = &output.Action{Kind: "relogin", Cmd: "backsight setup --runtime native",
			Why: "the native server's units load into this user's systemd user manager, reachable from a login session of this user"}
	default:
		c.Status, c.Summary = OK, "systemd user session reachable"
	}
	return c
}

// lingerCheck: without lingering, the user's units stop at logout and the
// server with them (decision 0078, requirement 7).
func lingerCheck(p Probes) Check {
	c := Check{ID: "linger"}
	v, err := "", errors.New("not probed")
	if p.Linger != nil {
		v, err = p.Linger()
	}
	v = strings.TrimSpace(v)
	if err == nil && v == "yes" {
		c.Status, c.Summary = OK, "lingering on: the server runs while you are logged out"
		return c
	}
	c.Status, c.Class = Fail, "needs_user"
	c.Summary = "lingering is off for your user"
	if err != nil {
		c.Summary = "cannot tell whether lingering is on for your user (" + err.Error() + ")"
	}
	c.Fix = "loginctl enable-linger"
	c.Action = &output.Action{Kind: "run_as_user", Cmd: "loginctl enable-linger",
		Why: "the native server runs as your systemd user units, which stop at logout without lingering"}
	return c
}

// SchedulerOffFix says what scheduler=off stops and how to run it by hand.
const SchedulerOffFix = "answer scheduler=off, or run setup again from a login session of this user (an ssh login, not su or sudo). " + offEffect

const offEffect = "With scheduler=off nothing runs on a timer: sessions are still sent when they end, but sending the settled sessions and queued notes " +
	"(backsight jobs tick, every 15 min) and the daily upkeep with its backup (backsight jobs maintain, or backsight backup create alone) " +
	"run only when you run them, by hand or from a scheduler of your own"

// ErrSessionSlow: systemd's user manager did not answer in time; it may
// be busy rather than missing, so scheduler=off is not the only way out.
var ErrSessionSlow = errors.New("systemd's user session did not answer within 5 s")

// schedulerCheck: without a reachable user manager the scheduler step
// cannot load its timers, and nothing a rerun does changes that: the
// user chooses between a login session and scheduler=off before anything
// is written.
func schedulerCheck(ctx context.Context, p Probes) Check {
	c := Check{ID: "scheduler"}
	err := p.UserScheduler(ctx)
	if errors.Is(err, ErrSessionSlow) {
		c.Status, c.Class = Fail, "blocked"
		c.Summary = err.Error() + ": setup cannot tell whether it can install the timers that run backsight's jobs"
		c.Fix = "retry, from a login session of this user (an ssh login, not su or sudo); if this machine has no user session at all, answer scheduler=off. " + offEffect
		return c
	}
	if err != nil {
		c.Status, c.Class = Fail, "needs_user"
		c.Summary = "no systemd user session here (" + err.Error() + "): setup cannot install the timers that run backsight's jobs"
		c.Fix = SchedulerOffFix
		c.Action = &output.Action{Kind: "run_as_user", Cmd: "backsight setup --answer scheduler=off",
			Why: "this machine has no systemd user session for the timers; scheduler=off skips them, and then jobs tick and jobs maintain run only by hand"}
		return c
	}
	c.Status, c.Summary = OK, "systemd user session reachable"
	return c
}

// Verdict is the exit class of a pre-flight: needs_user wins over blocked
// (the user's actions may also clear the rest), "" when nothing failed.
func Verdict(cs []Check) (class string, failed []Check) {
	for _, c := range cs {
		if c.Status != Fail {
			continue
		}
		failed = append(failed, c)
		if class == "" || c.Class == "needs_user" {
			class = c.Class
		}
	}
	return class, failed
}
