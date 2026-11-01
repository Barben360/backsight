package supervise

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// proc is a running child: done receives its exit once; outDone closes
// once its output is all in the log (every holder of the pipe gone).
type proc struct {
	cmd     *exec.Cmd
	pid     int
	done    chan error
	outDone chan struct{}
	tree    *tree // the child and what it starts (job_*.go)
	treeErr error // why the tree is the child alone, if it is
}

// startChild starts argv with exactly env (nothing inherited) in dir, in a
// process group and a process tree of its own (job_*.go: kill ends both),
// its stdout and stderr to out. postgres gets a restricted token when
// this process is an administrator's (token_*.go).
//
// The output goes through a pipe this process reads, and the child's
// exit is waited for apart from it: postgres' backends inherit the pipe
// and may outlive the postmaster for a moment (E34 ruling 2's lesson).
func startChild(argv, env []string, dir string, out io.WriteCloser, postgres bool) (*proc, error) {
	if len(argv) == 0 {
		_ = out.Close()
		return nil, errors.New("no program to start")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append([]string{}, env...)
	cmd.Dir = dir
	// out is closed on every path: by the output copy once started, here
	// on a failure (a log writer always ends with its Close).
	fail := func(err error) (*proc, error) {
		_ = out.Close()
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	cmd.Stdout, cmd.Stderr = w, w
	attr, release, err := childAttr(postgres)
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		return fail(err)
	}
	cmd.SysProcAttr = attr
	err = cmd.Start()
	release()
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return fail(err)
	}
	p := &proc{cmd: cmd, pid: cmd.Process.Pid, done: make(chan error, 1), outDone: make(chan struct{})}
	// The child starts suspended where its tree must hold it from its
	// first instruction (Windows): in its tree, then resumed. A child that
	// cannot be resumed is killed, and the start failed.
	p.tree, p.treeErr = ownTree(cmd.Process)
	if err := resumeChild(cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		p.tree.close()
		_ = r.Close()
		return fail(fmt.Errorf("resuming it (pid %d) failed, killed: %w", cmd.Process.Pid, err))
	}
	if tm.wrapOutput != nil {
		out = tm.wrapOutput(out)
	}
	go func() {
		defer close(p.outDone)
		_, _ = io.Copy(out, r)
		_ = r.Close()
		_ = out.Close()
	}()
	go func() {
		err := cmd.Wait()
		p.tree.close()
		p.done <- err
	}()
	return p, nil
}

// kill ends the child and everything it started.
func (p *proc) kill() error { return p.tree.kill(p.cmd.Process) }

// How a stop ended.
type stopOutcome int

const (
	stopExited      stopOutcome = iota // the child exited on the interrupt
	stopKilled                         // still running after the wait: killed
	stopNoInterrupt                    // no interrupt possible (no console): killed at once
)

// stop interrupts the child's group (CTRL_BREAK on Windows, SIGTERM
// elsewhere), kills its tree after wait, and returns how it ended and the
// child's exit.
func (p *proc) stop(wait time.Duration) (stopOutcome, error) {
	if err := tm.interrupt(p.cmd.Process); err != nil {
		_ = p.kill()
		return stopNoInterrupt, <-p.done
	}
	select {
	case err := <-p.done:
		return stopExited, err
	case <-time.After(wait):
	}
	_ = p.kill()
	return stopKilled, <-p.done
}

// waitOutput waits up to d for the child's last output to reach its log.
func (p *proc) waitOutput(d time.Duration) {
	select {
	case <-p.outDone:
	case <-time.After(d):
	}
}

// runTool runs a short tool (pg_isready, pg_ctl stop) with exactly env
// until it exits or ctx ends; its output goes to out (nil: discarded).
func runTool(ctx context.Context, argv, env []string, out io.WriteCloser) error {
	if len(argv) == 0 {
		return errors.New("no program to run")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append([]string{}, env...)
	cmd.SysProcAttr = toolAttr()
	cmd.WaitDelay = time.Second
	if out != nil {
		defer func() { _ = out.Close() }()
		cmd.Stdout, cmd.Stderr = out, out
	}
	return cmd.Run()
}

// readEnvFile is server.env's pairs: the API's environment besides Env.
func readEnvFile(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseEnv(raw), nil
}

// ParseEnv reads the KEY=value lines of an environment file the native
// runtime's Render wrote, without the single quotes it puts around a
// value; comments and other lines are skipped.
func ParseEnv(raw []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(l, "=")
		if !ok || l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = v[1 : len(v)-1]
		}
		out = append(out, k+"="+v)
	}
	return out
}
