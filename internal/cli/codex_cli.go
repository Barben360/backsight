package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/harness"
)

// codexRunner runs the codex command for the Codex adapter (through
// toolRunner); tests replace it.
var codexRunner claudeCLI = codexCLI{}

var errCodexMissing error = harness.MissingError{Command: "codex"}

// codexCLI runs codex from PATH, with the shell's CODEX_HOME dropped and
// the home the caller names (harness.Codex.ProfileEnv) instead: a shell
// set to another Codex home never redirects a call meant for the recorded
// one (0085 D1), as claudeEnviron does for CLAUDE_CONFIG_DIR.
type codexCLI struct{}

func (codexCLI) Run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return nil, errCodexMissing
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = codexEnviron(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr // its warnings on stderr never corrupt the JSON
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("codex %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()+" "+stdout.String()))
	}
	return stdout.Bytes(), nil
}

// codexEnviron is this process's environment without CODEX_HOME, then env.
func codexEnviron(env []string) []string {
	out := make([]string, 0, len(os.Environ())+len(env))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CODEX_HOME" || (runtime.GOOS == "windows" && strings.EqualFold(k, "CODEX_HOME")) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, env...)
}
