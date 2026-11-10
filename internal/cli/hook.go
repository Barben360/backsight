package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/hook"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/telemetry"
)

// hookChildCommand starts the child of a hook event; tests replace it.
var hookChildCommand = func(ctx context.Context, event string) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	c := exec.CommandContext(ctx, exe, "hook-child", event)
	// The child records its session under Claude Code's pid, the parent of
	// this process, where the MCP server looks it up (decision 0056).
	c.Env = append(os.Environ(), claudePIDEnv+"="+strconv.Itoa(os.Getppid()))
	return c
}

// claudePIDEnv carries the Claude Code process id to the hook child.
const claudePIDEnv = "BACKSIGHT_CLAUDE_PID"

// hookHarnessEnv carries the harness id of a hook event to its child; the
// parent always sets it, so a value left in the shell never applies.
const hookHarnessEnv = "BACKSIGHT_HOOK_HARNESS"

// hookArgs splits `backsight hook [--harness <id>] <event>`: the harness
// a hook is wired for names itself, Claude Code's plugin names none.
func hookArgs(args []string) (adapter harness.Adapter, event string, ok bool) {
	id := harness.ClaudeCode
	switch {
	case len(args) > 0 && strings.HasPrefix(args[0], "--harness="):
		id, args = strings.TrimPrefix(args[0], "--harness="), args[1:]
	case len(args) > 1 && args[0] == "--harness":
		id, args = args[1], args[2:]
	}
	if len(args) > 0 {
		event = args[0]
	}
	adapter, ok = harness.ByID(id)
	return adapter, event, ok
}

// runHook is `backsight hook <event>`, the supervisor's parent. It never
// fails: whatever happens, the harness gets exit 0 and at most a valid
// hook answer (SPEC §6.1); a harness backsight does not know gets nothing.
func runHook(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	adapter, event, ok := hookArgs(args)
	if !ok {
		return 0
	}
	input, err := io.ReadAll(io.LimitReader(stdin, hook.MaxInput+1))
	if err != nil {
		return 0
	}
	command := func(ctx context.Context, event string) *exec.Cmd {
		c := hookChildCommand(ctx, event)
		if c.Env == nil {
			c.Env = os.Environ()
		}
		c.Env = append(c.Env, hookHarnessEnv+"="+adapter.ID())
		return c
	}
	sup := hook.Supervisor{Command: command, Deadline: hook.ChildDeadline}
	out, failure := sup.Run(ctx, event, input)
	if failure != nil {
		in, _ := adapter.DecodeHook(event, input)
		if d, err := paths.Default(); err == nil {
			telemetry.New(d.State, telemetry.Common{V: instructions.Version, Session: in.SessionID, BinaryVersion: buildinfo.Version}).
				Emit(telemetry.HookFailure{Hook: event, Kind: failure.Kind, MS: failure.MS})
		}
		return 0
	}
	_, _ = stdout.Write(out)
	return 0
}

// hookChildStart is when this process began as a hook child (zero when a
// handler runs in-process): its supervisor kills it at hook.KillAt, the
// time it passed or hook.ChildDeadline after this.
var hookChildStart time.Time

// runHookChild is the hidden `backsight hook-child <event>`.
func runHookChild(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) int {
	hookChildStart = time.Now()
	event := ""
	if len(args) > 0 {
		event = args[0]
	}
	// The parent named the harness; a child started otherwise is Claude
	// Code's, as before harnesses had ids.
	adapter := harness.Default()
	if id := os.Getenv(hookHarnessEnv); id != "" {
		var ok bool
		if adapter, ok = harness.ByID(id); !ok {
			return 0
		}
	}
	return hook.RunChild(harness.WithAdapter(ctx, adapter), event, stdin, stdout, handleHook)
}

// handlers answer each event (M2 block 2); an event without one prints
// nothing. They run for the harness harness.FromContext(ctx) names.
var handlers = map[string]hook.Handler{}

func handleHook(ctx context.Context, event string, input []byte) ([]byte, error) {
	if h, ok := handlers[event]; ok {
		return h(withHookEvent(ctx, event), event, input)
	}
	return nil, nil
}
