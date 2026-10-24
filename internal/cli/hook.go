package cli

import (
	"context"
	"io"
	"os"
	"os/exec"

	"github.com/Barben360/backsight/internal/buildinfo"
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
	return exec.CommandContext(ctx, exe, "hook-child", event)
}

// runHook is `backsight hook <event>`, the supervisor's parent. It never
// fails: whatever happens, Claude Code gets exit 0 and at most a valid hook
// JSON object (SPEC §6.1).
func runHook(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	event := ""
	if len(args) > 0 {
		event = args[0]
	}
	input, err := io.ReadAll(io.LimitReader(stdin, hook.MaxInput+1))
	if err != nil {
		return 0
	}
	sup := hook.Supervisor{Command: hookChildCommand, Deadline: hook.ChildDeadline}
	out, failure := sup.Run(ctx, event, input)
	if failure != nil {
		if d, err := paths.Default(); err == nil {
			telemetry.New(d.State, telemetry.Common{V: instructions.Version, BinaryVersion: buildinfo.Version}).
				Emit(telemetry.HookFailure{Hook: event, Kind: failure.Kind, MS: failure.MS})
		}
		return 0
	}
	_, _ = stdout.Write(out)
	return 0
}

// runHookChild is the hidden `backsight hook-child <event>`.
func runHookChild(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) int {
	event := ""
	if len(args) > 0 {
		event = args[0]
	}
	return hook.RunChild(ctx, event, stdin, stdout, handleHook)
}

// handlers answer each event (M2 block 2); an event without one prints
// nothing.
var handlers = map[string]hook.Handler{}

func handleHook(ctx context.Context, event string, input []byte) ([]byte, error) {
	if h, ok := handlers[event]; ok {
		return h(ctx, event, input)
	}
	return nil, nil
}
