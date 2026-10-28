package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/selfcheck"
	"github.com/Barben360/backsight/internal/state"
)

// probes sets the machine probes; tests replace it.
var probes = realProbes

func realProbes(e *doctor.Env) {
	e.ManagedDir = claudecode.ManagedDir(runtime.GOOS)
	e.Docker = func(ctx context.Context) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		// The runtime's engine needs no config.
		eng, err := runtimeFor(config.Config{}).Engine(ctx)
		return eng.Version, err
	}
	e.GPUs = doctor.LocalGPUs
	e.FreeBytes = doctor.FreeBytes
	if runtime.GOOS == "linux" {
		e.Linger = func() (bool, error) {
			out, err := exec.Command("loginctl", "show-user", strconv.Itoa(os.Getuid()), "-p", "Linger", "--value").Output()
			return strings.TrimSpace(string(out)) == "yes", err
		}
	}
	e.ClaudePlugins = func(profile string) ([]byte, error) {
		var env []string
		if profile != "" {
			env = []string{"CLAUDE_CONFIG_DIR=" + profile}
		}
		return claudeRunner.Run(context.Background(), env, "plugin", "list", "--json")
	}
	e.SelfCheck = func() state.SelfCheck {
		exe, err := selfExe()
		if err != nil {
			return state.SelfCheck{Status: selfcheck.Failed, Detail: "cannot locate the running binary: " + err.Error()}
		}
		return selfcheck.Current(e.Dirs, exe)
	}
	e.ReferenceServer = referenceServer
	e.ExposureDetail = func() (string, string) { return exposureDetail(e.Dirs, e.Home) }
	e.ConfigEdits = func(profile string) ([]claudecfg.FileResult, error) {
		c, err := newClientEnv(e.Dirs, e.Home, profile, e.ManagedDir)
		if err != nil {
			return nil, err
		}
		edits, _, err := c.ownedEdits()
		if err != nil {
			return nil, err
		}
		return claudecfg.Plan(edits)
	}
	e.Scheduler = func() (doctor.SchedulerEntries, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return schedulerEntries(ctx, e.Dirs)
	}
}

// hindsightTimeout bounds each Hindsight call of doctor and report.
const hindsightTimeout = 10 * time.Second

// doctorEnv is doctor's view of this machine.
func doctorEnv(d paths.Dirs, home string) doctor.Env {
	denv := doctor.Env{Dirs: d, Home: home, Getenv: os.Getenv, Timeout: hindsightTimeout}
	denv.Cwd, _ = os.Getwd()
	probes(&denv)
	return denv
}

func newDoctorCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the memory stack and Claude Code wiring (read-only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, home, err := machine()
			if err != nil {
				return err
			}
			rep := runDoctor(cmd, d, home)
			var failed error
			if rep.Errors > 0 {
				failed = shownError{&output.Error{Code: output.Blocked, Message: fmt.Sprintf("%d check(s) failed", rep.Errors)}}
			}
			return e.report("doctor", rep, func() string { return doctorText(rep) }, failed)
		},
	}
}

var statusLabel = map[doctor.Status]string{doctor.OK: "ok", doctor.Info: "info", doctor.Warn: "WARN", doctor.Error: "ERROR"}

func doctorText(r doctor.Report) string {
	var b strings.Builder
	pad := strings.Repeat(" ", 19)
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-6s %-11s %s\n", statusLabel[c.Status], c.ID, c.Summary)
		if c.Detail != "" {
			fmt.Fprintf(&b, "%s%s\n", pad, c.Detail)
		}
		if c.Fix != "" {
			fmt.Fprintf(&b, "%sfix: %s\n", pad, c.Fix)
		}
	}
	fmt.Fprintf(&b, "\n%d error(s), %d warning(s)\n", r.Errors, r.Warnings)
	return b.String()
}

// runDoctor runs every check (doctor and the jobs maintain snapshot).
func runDoctor(cmd *cobra.Command, d paths.Dirs, home string) doctor.Report {
	return doctor.Run(cmd.Context(), doctorEnv(d, home))
}

// referenceServer asks the reference server's /health, then, for a
// loopback server, which Docker container publishes its port: its compose
// project gives the stop command (0067 block 4). Read-only.
func referenceServer(ctx context.Context, rawURL string) (doctor.RefServer, error) {
	cl, err := hindsight.New(rawURL, "", 5*time.Second)
	if err != nil {
		return doctor.RefServer{}, err
	}
	if _, err := cl.Health(ctx); errors.Is(err, hindsight.ErrUnreachable) || errors.Is(err, hindsight.ErrTimeout) {
		return doctor.RefServer{}, nil
	}
	srv := doctor.RefServer{Running: true} // it answered, healthy or not
	port := localPort(rawURL)
	if port == "" {
		return srv, nil // another machine: its Docker is not this one's
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := composeRunner.Run(ctx, "ps", "--filter", "publish="+port, "--format", `{{.Label "com.docker.compose.project"}}`+"\t"+`{{.Names}}`)
	if err == nil {
		srv.Stop, srv.Start = stopCommands(string(out))
	}
	return srv, nil
}

// localPort is the port of a server URL on this machine (localhost or a
// loopback address), "" for another machine or a URL without a port.
func localPort(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Port() == "" {
		return ""
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return ""
	}
	return u.Port()
}

// stopCommands turns `docker ps` lines (compose project, tab, container
// name) into the commands that stop and start that server: the whole
// compose project when there is one, else the container. A project or a
// name that is not a plain one is never put in a command the user would
// paste (0067 block 4 review rules).
func stopCommands(ps string) (stop, start string) {
	for _, line := range strings.Split(ps, "\n") {
		project, name, _ := strings.Cut(strings.TrimRight(line, " \r"), "\t")
		switch {
		case project != "":
			if composeProject.MatchString(project) {
				return "docker compose -p " + project + " stop", "docker compose -p " + project + " start"
			}
		case name != "":
			if containerName.MatchString(name) {
				return "docker stop " + name, "docker start " + name
			}
		}
	}
	return "", ""
}

var (
	// composeProject is a name Compose accepts for a project.
	composeProject = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	// containerName is a name Docker accepts for a container.
	containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
)
