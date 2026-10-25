package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
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
}

// doctorEnv is doctor's view of this machine.
func doctorEnv(d paths.Dirs, home string) doctor.Env {
	denv := doctor.Env{Dirs: d, Home: home, Getenv: os.Getenv, Timeout: 10 * time.Second}
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
