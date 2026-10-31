package supervise

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// proc is a running child: done receives its exit once.
type proc struct {
	cmd  *exec.Cmd
	pid  int
	done chan error
}

// startChild starts argv with exactly env (nothing inherited), in a
// process group of its own, its stdout and stderr to out. postgres gets a
// restricted token when this process is an administrator's (token_*.go).
//
// The output goes through a pipe this process reads, and the child's
// exit is waited for apart from it: postgres' backends inherit the pipe
// and may outlive the postmaster for a moment (E34 ruling 2's lesson).
func startChild(argv, env []string, out io.WriteCloser, postgres bool) (*proc, error) {
	if len(argv) == 0 {
		return nil, errors.New("no program to start")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append([]string{}, env...)
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	attr, release, err := childAttr(postgres)
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, err
	}
	cmd.SysProcAttr = attr
	err = cmd.Start()
	release()
	_ = w.Close()
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	go func() {
		_, _ = io.Copy(out, r)
		_ = r.Close()
		_ = out.Close()
	}()
	p := &proc{cmd: cmd, pid: cmd.Process.Pid, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait() }()
	return p, nil
}

// stop interrupts the child's group (CTRL_BREAK on Windows, SIGTERM
// elsewhere), kills it after wait, and returns its exit.
func (p *proc) stop(wait time.Duration) error {
	if err := interrupt(p.cmd.Process); err != nil {
		_ = kill(p.cmd.Process)
	}
	select {
	case err := <-p.done:
		return err
	case <-time.After(wait):
	}
	_ = kill(p.cmd.Process)
	return <-p.done
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
