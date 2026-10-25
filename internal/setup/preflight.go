package setup

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

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
	// Docker runs `docker info`; Compose `docker compose version --short`.
	Docker  func(ctx context.Context) (string, error)
	Compose func(ctx context.Context) (string, error)
	// PortFree reports whether a loopback port can be bound.
	PortFree func(port int) bool
	// OwnServer: backsight's configured server already answers (adoption),
	// so its ports being taken is expected.
	OwnServer bool
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
}

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

	if p.Remote {
		add(Check{ID: "docker", Status: Info, Summary: "remote server: Docker is checked on its host"})
	} else {
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
	add(claudeCheck(ctx, p))
	if p.Reference > 0 {
		add(Check{ID: "reference", Status: Info, Summary: fmt.Sprintf("%d reference pieces wired into Claude Code (see the reference question)", p.Reference)})
	}
	if p.Retention != "" {
		add(Check{ID: "retention", Status: Info, Summary: p.Retention})
	}
	if p.Transcript != "" {
		add(Check{ID: "transcripts", Status: Info, Summary: p.Transcript})
	}
	gpu := Check{ID: "gpu", Status: Info, Summary: "no GPU found: CPU inference"}
	if len(p.GPUs) > 0 {
		gpu.Summary = "GPU found (" + strings.Join(p.GPUs, ", ") + "): reported only, acceleration comes in v2"
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
	case p.OwnServer:
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
	switch {
	case err != nil:
		c.Status, c.Summary = Warn, "free space unknown: "+err.Error()
	case free >= MinFreeBytes:
		c.Status, c.Summary = OK, fmt.Sprintf("%.1f GB free", gb)
	case p.ServerInstalled:
		c.Status, c.Summary = Warn, fmt.Sprintf("only %.1f GB free", gb)
		c.Fix = "free some space: the memory grows and backups need room"
	default:
		c.Status, c.Class, c.Summary = Fail, "blocked", fmt.Sprintf("only %.1f GB free, the server needs 6 GB", gb)
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
