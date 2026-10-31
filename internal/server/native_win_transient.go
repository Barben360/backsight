package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/supervise"
)

// Windows' transient jobs (decision 0080): no service manager runs them,
// so the calling backsight does, each step in turn in one unnamed
// kill-on-close Job Object that lives for the call: whatever a step leaves
// running (pg0's postmaster, the throwaway database's backends) dies when
// the call returns, or with this process. The steps' markers are
// native_host.go's.

// StepRunner is a ToolRunner that runs Windows' transient steps: the
// process started in job, with exactly env and stdin, its stdout and
// stderr to out when out is set (a file: never a pipe, E34 ruling 2),
// else stdout returned and stderr's tail in the error.
type StepRunner interface {
	RunStep(ctx context.Context, job *supervise.Job, env []string, stdin io.Reader, out *os.File, path string, args ...string) ([]byte, error)
}

// stepStderr is how much of a failed step's stderr, or of its log, its
// error keeps.
const stepStderr = 4096

// RunInJob is StepRunner's real work (the CLI's tool runner and the tests
// share it): a cancel of ctx terminates the job, the step and everything
// it started.
func RunInJob(ctx context.Context, job *supervise.Job, env []string, stdin io.Reader, out *os.File, path string, args ...string) ([]byte, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = append([]string{}, env...) // never nil: nil inherits ours
	cmd.Stdin = stdin
	// A grandchild holding a captured pipe (out unset) must not hang the
	// call once the step exited.
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	if out != nil {
		cmd.Stdout, cmd.Stderr = out, out
	} else {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	}
	if err := job.Start(cmd); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = job.Terminate()
		err = errors.Join(ctx.Err(), <-done)
	}
	if err != nil {
		tail := stderr.Bytes()
		if len(tail) > stepStderr {
			tail = tail[len(tail)-stepStderr:]
		}
		if t := strings.TrimSpace(string(tail)); t != "" {
			err = fmt.Errorf("%w: %s", err, t)
		}
	}
	return stdout.Bytes(), err
}

// winStep is one step as Windows runs it.
type winStep struct {
	argv  []string
	env   []string
	stdin io.Reader
	log   string // the step's log file, "" when its output is captured
}

// windowsStep reads step i of the job name: its own KEY=value words over
// env, its stdin (the call's for "-f -", else none) and its log.
func (n Native) windowsStep(name string, i int, env []string, stdin io.Reader, step []string) (winStep, error) {
	s := winStep{env: slices.Clone(env)}
	words := step
	for len(words) > 0 && envWord.MatchString(words[0]) {
		k, _, _ := strings.Cut(words[0], "=")
		s.env = slices.DeleteFunc(s.env, func(e string) bool {
			ek, _, _ := strings.Cut(e, "=")
			return strings.EqualFold(ek, k)
		})
		s.env = append(s.env, words[0])
		words = words[1:]
	}
	toLog := false
	for _, w := range words {
		switch w {
		case stepNoStdin:
		case stepNoStdout, stepToLog:
			toLog = true
		default:
			s.argv = append(s.argv, w)
		}
	}
	if len(s.argv) == 0 {
		return s, fmt.Errorf("step %d of %s runs nothing", i+1, name)
	}
	for j := 1; j+1 < len(s.argv); j++ {
		if s.argv[j] == "-f" && s.argv[j+1] == "-" {
			s.stdin = stdin
		}
	}
	if toLog {
		prog := s.argv[0][strings.LastIndexAny(s.argv[0], `/\`)+1:] // a Windows path on any host
		prog = strings.TrimSuffix(strings.ToLower(prog), ".exe")
		s.log = n.join(n.Root(), "tmp", prog+"-"+name+"-"+strconv.Itoa(i+1)+".log")
	}
	return s, nil
}

// Transient runs the steps in order, each through n.Tools in one fresh
// Job Object closed when the call returns: every process the steps
// started dies with the call (children of a server step included).
func (h taskHost) Transient(ctx context.Context, name string, env []string, stdin io.Reader, argv [][]string) error {
	sr, ok := h.n.Tools.(StepRunner)
	if !ok {
		return fmt.Errorf("the native runtime's tool runner cannot run %s's steps in a job object", name)
	}
	job, err := h.n.transientJob(name)
	if err != nil {
		return err
	}
	defer func() { _ = job.Close() }()
	for i, st := range argv {
		s, err := h.n.windowsStep(name, i, env, stdin, st)
		if err != nil {
			return err
		}
		var out *os.File
		if s.log != "" {
			if err := fsutil.MkdirPrivate(filepath.Dir(s.log)); err != nil {
				return err
			}
			if out, err = os.Create(s.log); err != nil {
				return err
			}
		}
		_, err = sr.RunStep(ctx, job, s.env, s.stdin, out, s.argv[0], s.argv[1:]...)
		if out != nil {
			_ = out.Close()
		}
		if err != nil {
			return fmt.Errorf("%s: %w%s", filepath.Base(s.argv[0]), err, logTail(s.log))
		}
	}
	return nil
}

// transientJob is the Job Object of the transient job name: a backup
// verify's is named (supervise.NewNamedJob), for the stray scan to skip its
// throwaway database (and initdb's bootstrap postgres) while it runs;
// any other is unnamed.
func (n Native) transientJob(name string) (*supervise.Job, error) {
	if id, ok := strings.CutPrefix(name, n.project()+"-verify-"); ok {
		return supervise.NewNamedJob(n.project(), verifyJobSuffix(id))
	}
	return supervise.NewJob()
}

// logTail is a failed step's log's end, for its error: "" without one.
func logTail(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(raw) > stepStderr {
		raw = raw[len(raw)-stepStderr:]
	}
	if t := strings.TrimSpace(string(raw)); t != "" {
		return " (" + path + ": " + t + ")"
	}
	return " (see " + path + ")"
}

// removeAllRetry is os.RemoveAll, retried for a few seconds on Windows:
// the processes of a job just closed may still hold their files a moment.
func removeAllRetry(path string) error {
	deadline := time.Now().Add(removeRetryWait)
	for {
		err := os.RemoveAll(path)
		if err == nil || !time.Now().Before(deadline) || errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// removeRetryWait bounds removeAllRetry.
var removeRetryWait = 10 * time.Second
