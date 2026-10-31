// Package hook runs Claude Code hook events behind a supervisor (SPEC §6.1):
// a parent re-executes the binary as a child with a hard deadline, prints
// the child's stdout only when it exited 0 with a JSON object, and always
// exits 0 itself. Go exits 2 on an unrecovered panic, and exit 2 is
// Claude Code's "block" code (C06): a crash must never block a prompt.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// ChaosEnv makes the child fail on purpose (chaos tests, SPEC §16):
	// panic | fatal | hang | kill | exit2. Only a testhooks build honours
	// it (chaos_testhooks.go); a release ignores it.
	ChaosEnv = "BACKSIGHT_HOOK_CHAOS"
	// MaxInput caps the event JSON read from stdin.
	MaxInput  = 16 << 20
	maxOutput = 64 << 10 // Claude Code caps hook fields at 10 000 chars
	waitDelay = 500 * time.Millisecond
)

// Failure is why nothing was printed (telemetry hook_failure, A.4).
type Failure struct {
	Kind   string `json:"kind"` // panic | fatal | timeout | killed | exit | invalid_output | input_too_large | spawn
	MS     int    `json:"ms"`
	Detail string `json:"-"`
}

// Supervisor runs one event in a child process.
type Supervisor struct {
	Command  func(ctx context.Context, event string) *exec.Cmd
	Deadline func(event string, input []byte) time.Duration
}

// ChildDeadline is the child's hard deadline per event (SPEC §6.2), below
// the timeout declared in hooks.json.
func ChildDeadline(event string, input []byte) time.Duration {
	switch event {
	case "session-start":
		var in struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(input, &in)
		if in.Source == "startup" {
			return 8 * time.Second
		}
		return 3 * time.Second
	case "user-prompt-submit":
		return 20 * time.Second
	case "pre-tool-use":
		return 9 * time.Second
	case "post-tool-use":
		return 1500 * time.Millisecond
	case "post-tool-use-failure":
		return 8 * time.Second // one recall (6 s) and a session write
	case "session-end":
		return 800 * time.Millisecond
	}
	return 3 * time.Second // subagent-start, subagent-stop, stop, anything else
}

// Run feeds input to a child and returns what may be printed, or why not.
func (s Supervisor) Run(ctx context.Context, event string, input []byte) ([]byte, *Failure) {
	start := time.Now()
	fail := func(kind, detail string) ([]byte, *Failure) {
		return nil, &Failure{Kind: kind, MS: int(time.Since(start).Milliseconds()), Detail: detail}
	}
	if len(input) > MaxInput {
		return fail("input_too_large", "")
	}
	cctx, cancel := context.WithTimeout(ctx, s.Deadline(event, input))
	defer cancel()
	cmd := s.Command(cctx, event)
	cmd.Stdin = bytes.NewReader(input)
	out, errb := &capped{max: maxOutput}, &capped{max: maxOutput}
	cmd.Stdout, cmd.Stderr = out, errb
	cmd.WaitDelay = waitDelay
	err := cmd.Run()
	stderr := errb.String()
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		return fail("timeout", "")
	case err != nil && cmd.ProcessState == nil:
		return fail("spawn", err.Error())
	case err != nil && (ctx.Err() != nil || killedBy(cmd.ProcessState.ExitCode())):
		// The supervisor ended the child (its caller gave up), or someone
		// else did: a signal, or on Windows a TerminateProcess or Job kill.
		return fail("killed", err.Error())
	case err != nil && strings.Contains(stderr, "panic:"):
		return fail("panic", firstLine(stderr))
	case err != nil && strings.Contains(stderr, "fatal error:"):
		return fail("fatal", firstLine(stderr))
	case err != nil:
		return fail("exit", firstLine(stderr))
	case out.overflow:
		return fail("invalid_output", "output too large")
	}
	body := bytes.TrimSpace(out.Bytes())
	if len(body) == 0 {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return fail("invalid_output", "not a JSON object")
	}
	return body, nil
}

// Handler answers one event in the child: the JSON to print, or nothing.
type Handler func(ctx context.Context, event string, input []byte) ([]byte, error)

// RunChild is the child side: chaos first (a testhooks build only), then
// the handler. It returns the process exit code.
func RunChild(ctx context.Context, event string, stdin io.Reader, stdout io.Writer, h Handler) int {
	chaos()
	input, err := io.ReadAll(io.LimitReader(stdin, MaxInput))
	if err != nil {
		return childFailed
	}
	out, err := h(ctx, event, input)
	if err != nil {
		_, _ = os.Stderr.WriteString("backsight hook " + event + ": " + err.Error() + "\n")
		return childFailed
	}
	if len(out) > 0 {
		if _, err := stdout.Write(out); err != nil {
			return childFailed
		}
	}
	return 0
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// capped keeps at most max bytes and remembers it dropped some.
type capped struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); len(p) > room {
		c.overflow = true
		if room > 0 {
			c.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return c.Buffer.Write(p)
}
