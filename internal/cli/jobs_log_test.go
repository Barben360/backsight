package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
)

// onWindowsJobs makes the jobs keep their own log, as on Windows.
func onWindowsJobs(t *testing.T) {
	t.Helper()
	old := jobsLogGOOS
	jobsLogGOOS = "windows"
	t.Cleanup(func() { jobsLogGOOS = old })
}

// A scheduled job on Windows (no redirect in the task's action) writes
// what it prints, then how it ended, to <state>/logs/<project>-jobs-<name>.log.
func TestJobsKeepTheirOwnLogOnWindows(t *testing.T) {
	home := devInstall(t, true)
	onWindowsJobs(t)
	d := pathsOf(home)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[backup]\nenabled = false\n")...), 0o600)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	code, out, _ := run(t, "jobs", "maintain")
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	log := readFile(t, jobLogPath(d.State, "backsight", "maintain"))
	for _, want := range []string{"2026-10-25T04:00:00Z jobs maintain started (backsight ", strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]), "jobs maintain ended: exit 0\n"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}

	// A tick that fails says why and with which exit code (here a broken
	// config.toml, the project then the default's).
	_ = os.WriteFile(cfg, []byte("[server\n"), 0o600)
	code, _, errOut := run(t, "jobs", "tick")
	if code == output.OK {
		t.Fatal("a tick on a broken config.toml passed")
	}
	log = readFile(t, jobLogPath(d.State, "backsight", "tick"))
	if !strings.Contains(log, strings.TrimSpace(errOut)) || !strings.Contains(log, fmt.Sprintf("jobs tick ended: exit %d\n", code)) {
		t.Errorf("the tick's log lacks its failure (%q, exit %d):\n%s", errOut, code, log)
	}
}

// The log never carries a secret's value, and is named after the project.
func TestJobLogRedactsTheSecrets(t *testing.T) {
	home := devInstall(t, true)
	onWindowsJobs(t)
	d := pathsOf(home)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, []byte(strings.Replace(string(raw), "[server]\n", "[server]\nproject = \"bs-other\"\n", 1)), 0o600)
	sec, err := config.LoadSecrets(secretsFile(home))
	if err != nil || sec.APIKey == "" {
		t.Fatalf("secrets: %v", err)
	}
	e := &env{stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	err = jobLogged(e, "tick", func() error {
		fmt.Fprintln(e.stdout, "sent with "+sec.APIKey)
		return &output.Error{Code: output.Blocked, Message: "the server refused " + sec.LLMAPIKey}
	})
	if err == nil {
		t.Fatal("the body's error was dropped")
	}
	if got := e.stdout.(*strings.Builder).String(); !strings.Contains(got, sec.APIKey) {
		t.Errorf("the console lost the output: %q", got)
	}
	log := readFile(t, jobLogPath(d.State, "bs-other", "tick"))
	if strings.Contains(log, sec.APIKey) || strings.Contains(log, sec.LLMAPIKey) {
		t.Errorf("a secret reached the log:\n%s", log)
	}
	if !strings.Contains(log, "sent with [redacted]") || !strings.Contains(log, "backsight: the server refused [redacted]") ||
		!strings.Contains(log, fmt.Sprintf("ended: exit %d", output.Blocked)) {
		t.Errorf("log:\n%s", log)
	}
}

// The log rotates at 10 MiB to .1, as the supervisor's logs.
func TestJobLogRotatesAt10MiB(t *testing.T) {
	home := devInstall(t, true)
	onWindowsJobs(t)
	path := jobLogPath(pathsOf(home).State, "backsight", "tick")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	old := strings.Repeat("x", 10<<20-10) + "\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &env{stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	_ = jobLogged(e, "tick", func() error { fmt.Fprintln(e.stdout, "this run"); return nil })
	if got := readFile(t, path+".1"); got != old {
		t.Errorf("%s.1 holds %d bytes, want the old log's %d", path, len(got), len(old))
	}
	if got := readFile(t, path); !strings.Contains(got, "this run") || len(got) > 1<<10 {
		t.Errorf("the new log: %d bytes\n%.200s", len(got), got)
	}
}

// Elsewhere the scheduler keeps the output (the journal, launchd's log):
// no log of backsight's own, and a log that cannot be opened never stops
// a job.
func TestJobLogOnlyOnWindows(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	old := jobsLogGOOS
	jobsLogGOOS = "linux"
	t.Cleanup(func() { jobsLogGOOS = old })
	e := &env{stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	_ = jobLogged(e, "tick", func() error { return nil })
	if onDisk(jobLogPath(d.State, "backsight", "tick")) {
		t.Error("a job log on Linux")
	}
	onWindowsJobs(t)
	_ = os.MkdirAll(filepath.Join(d.State, "logs", "backsight-jobs-tick.log"), 0o700) // a directory where the log goes
	ran := false
	if err := jobLogged(e, "tick", func() error { ran = true; return errors.New("boom") }); err == nil || err.Error() != "boom" || !ran {
		t.Errorf("the job: ran %v, %v", ran, err)
	}
}
