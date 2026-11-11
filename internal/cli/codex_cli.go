package cli

import (
	"context"
	"fmt"

	"github.com/Barben360/backsight/internal/harness"
)

// codexRunner runs the codex command for the Codex adapter (through
// toolRunner); tests replace it. Block 3
// (https://github.com/Barben360/backsight/issues/278) implements codexCLI:
// codex found on PATH, the shell's CODEX_HOME dropped and the recorded
// home named (as claudeEnviron does for CLAUDE_CONFIG_DIR). Until then it
// runs nothing.
var codexRunner claudeCLI = codexCLI{}

type codexCLI struct{}

func (codexCLI) Run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("codex command: %w", harness.ErrNotImplemented)
}
