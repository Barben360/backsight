package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/supervise"
)

// jobsLogGOOS is the OS whose scheduled jobs keep their own log (Windows);
// tests replace it.
var jobsLogGOOS = runtime.GOOS

// jobLogPath is where jobs <name> logs on Windows:
// <state>/logs/<project>-jobs-<name>.log, beside the supervisor's.
func jobLogPath(state, project, name string) string {
	return filepath.Join(state, "logs", project+"-jobs-"+name+".log")
}

// logJob makes the scheduled job cmd (tick, maintain) keep its log
// (jobLogged).
func logJob(e *env, cmd *cobra.Command) *cobra.Command {
	body := cmd.RunE
	cmd.RunE = func(c *cobra.Command, args []string) error {
		return jobLogged(e, c.Name(), func() error { return body(c, args) })
	}
	return cmd
}

// jobLogged runs a job's body (jobs tick, jobs maintain) and, on Windows,
// also writes what it prints, its failure and its exit code to
// jobLogPath, rotated at 10 MiB to .1, the secrets' values redacted as in
// the supervisor's logs. The scheduled task runs the job under conhost
// --headless with no redirect: without it a failed run leaves no trace
// (Linux keeps it in the journal, macOS in launchd's logs/jobs-<name>.log).
// A log that cannot be opened never stops the job.
func jobLogged(e *env, name string, run func() error) error {
	if jobsLogGOOS != "windows" {
		return run()
	}
	log, closeLog := openJobLog(name)
	if log == nil {
		return run()
	}
	stdout, stderr := e.stdout, e.stderr
	// The log first: a console write that fails (no console) must not
	// keep the line from the log.
	e.stdout, e.stderr = io.MultiWriter(log, stdout), io.MultiWriter(log, stderr)
	defer func() { e.stdout, e.stderr = stdout, stderr }()
	err := run()
	code := output.OK
	var rep reported
	switch {
	case errors.As(err, &rep):
		code = rep.code // its message is printed, so logged, already
	case err != nil:
		// Printed by Execute once the command returns: the log gets it here.
		code = output.CodeOf(err)
		_, _ = fmt.Fprintf(log, "backsight: %v\n", maskTokens(err))
	}
	_, _ = fmt.Fprintf(log, "%s jobs %s ended: exit %d\n", now().UTC().Format(time.RFC3339), name, code)
	closeLog()
	return err
}

// openJobLog opens jobs <name>'s log and writes the run's first line; nil
// when the directories or the log cannot be had.
func openJobLog(name string) (io.Writer, func()) {
	d, home, err := machine()
	if err != nil {
		return nil, nil
	}
	cfg, _ := config.Load(d.ConfigFile(), d, home) // on error: the defaults
	project := cfg.Server.Project
	if project == "" {
		project = "backsight"
	}
	if err := fsutil.MkdirPrivate(filepath.Join(d.State, "logs")); err != nil {
		return nil, nil
	}
	f, err := supervise.OpenLog(jobLogPath(d.State, project, name))
	if err != nil {
		return nil, nil
	}
	red := supervise.NewRedactor(f, localSecretValues())
	_, _ = fmt.Fprintf(red, "%s jobs %s started (backsight %s, pid %d)\n", now().UTC().Format(time.RFC3339), name, buildinfo.Version, os.Getpid())
	return red, func() { _ = red.Close(); _ = f.Close() }
}
