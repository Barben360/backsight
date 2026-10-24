package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/state"
)

// minClaude is the oldest Claude Code the plugin was verified on (E1).
const minClaude = "2.1.309"

// claudeCLI runs the claude command; tests replace it.
type claudeCLI interface {
	Run(ctx context.Context, env []string, args ...string) ([]byte, error)
}

var errClaudeMissing = errors.New("the claude command was not found")

type execClaude struct{}

func (execClaude) Run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return nil, errClaudeMissing
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("claude %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

var claudeRunner claudeCLI = execClaude{}

type installStep struct {
	Name   string `json:"name"`
	Status string `json:"status"` // done | skipped | planned
	Detail string `json:"detail,omitempty"`
}

type clientInstallResult struct {
	Profile string        `json:"profile"`
	Steps   []installStep `json:"steps"`
	Plan    bool          `json:"plan"`
}

func newClientInstallCmd(e *env, configDir *string) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Wire Claude Code to backsight: the plugin (hooks and memory tools) and its permission rule",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := clientInstall(cmd.Context(), *configDir, plan)
			return e.report("client install", res, func() string { return installText(res) }, err)
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show what would be done without doing it")
	return cmd
}

func clientInstall(ctx context.Context, configDir string, plan bool) (clientInstallResult, error) {
	res := clientInstallResult{Plan: plan, Steps: []installStep{}}
	c, err := loadClientEnv(configDir)
	if err != nil {
		return res, err
	}
	res.Profile = c.loc.Dir
	var env []string
	if configDir != "" {
		env = []string{"CLAUDE_CONFIG_DIR=" + configDir}
	}
	add := func(name, status, detail string) { res.Steps = append(res.Steps, installStep{name, status, detail}) }
	status := map[bool]string{true: "planned", false: "done"}[plan]

	// 1. A recent enough claude CLI (SPEC §5: missing or too old → exit 10).
	v, err := claudeRunner.Run(ctx, env, "--version")
	if err != nil {
		return res, needsClaude("the claude command is missing or failed: " + err.Error())
	}
	version := strings.Fields(string(v) + " ?")[0]
	if !atLeast(version, minClaude) {
		return res, needsClaude(fmt.Sprintf("Claude Code %s is older than %s, the oldest version backsight was verified on", version, minClaude))
	}
	add("claude", "done", version)

	// 2. The stable shim every generated file points at (SPEC §3 Paths).
	shim := filepath.Join(c.dirs.Data, "bin", "current")
	target, err := shimTarget(c.dirs.Data)
	if err != nil {
		return res, blocked(err)
	}
	if !plan {
		if err := installShim(target, shim); err != nil {
			return res, blocked(err)
		}
	}
	add("shim", status, shim+" → "+target)

	// 3. The plugin, generated whole in a local marketplace (E1).
	mkt := filepath.Join(c.dirs.Data, "marketplace")
	pversion := plugin.Version(buildinfo.Version, shim)
	if !plan {
		if _, err := plugin.Generate(mkt, shim, buildinfo.Version); err != nil {
			return res, blocked(err)
		}
	}
	add("plugin", status, mkt+" (version "+pversion+")")

	// 4. Marketplace and plugin registered with Claude Code.
	known, err := listed(ctx, env, "id", "plugin", "list", "--json")
	if err != nil {
		return res, blocked(err)
	}
	markets, err := listed(ctx, env, "name", "plugin", "marketplace", "list", "--json")
	if err != nil {
		return res, blocked(err)
	}
	steps := [][]string{{"plugin", "marketplace", "add", mkt}}
	if markets[plugin.Marketplace] {
		steps[0] = []string{"plugin", "marketplace", "update", plugin.Marketplace}
	}
	if known[plugin.PluginID()] {
		steps = append(steps, []string{"plugin", "update", plugin.PluginID()})
	} else {
		steps = append(steps, []string{"plugin", "install", plugin.PluginID(), "--scope", "user"})
	}
	for _, args := range steps {
		if !plan {
			if _, err := claudeRunner.Run(ctx, env, args...); err != nil {
				return res, partialError(err)
			}
		}
		add("claude "+strings.Join(args[:min(3, len(args))], " "), status, "")
	}

	// 5. The one settings change: allow the three memory tools (SPEC §5).
	edit := claudecfg.Edit{Target: c.loc.Settings, Apply: func(doc []byte) ([]byte, []claudecfg.Change, error) {
		var changes []claudecfg.Change
		for _, tool := range plugin.ToolNames {
			out, ch, err := claudecfg.EnsureInArray(doc, []string{"permissions", "allow"}, tool)
			if err != nil {
				return nil, nil, err
			}
			doc, changes = out, append(changes, ch)
		}
		return doc, changes, nil
	}}
	if plan {
		fr, err := claudecfg.Plan([]claudecfg.Edit{edit})
		if err != nil {
			return res, editError(err)
		}
		add("permission rule", map[bool]string{true: "planned", false: "skipped"}[len(fr[0].Changes) > 0], c.loc.Settings)
		return res, nil
	}
	run, files, err := c.runner("client install").Apply([]claudecfg.Edit{edit})
	if err != nil {
		return res, partialError(err)
	}
	add("permission rule", map[bool]string{true: "done", false: "skipped"}[files[0].Written], strings.TrimSpace(c.loc.Settings+" "+run))

	if err := state.Update(c.dirs, "client install", func(s *state.State) error {
		s.PluginVersion, s.ShimTarget = pversion, target
		return nil
	}); err != nil {
		return res, partialError(err)
	}
	return res, nil
}

func needsClaude(msg string) error {
	return &output.Error{Code: output.NeedsUser, Message: msg, Actions: []output.Action{
		{Kind: "install", Cmd: "npm install -g @anthropic-ai/claude-code", Why: "backsight wires itself into Claude Code through its plugin commands"},
	}}
}

// listed runs a claude list command and returns the set of field values.
func listed(ctx context.Context, env []string, field string, args ...string) (map[string]bool, error) {
	raw, err := claudeRunner.Run(ctx, env, args...)
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("claude %s: unexpected output: %w", strings.Join(args, " "), err)
	}
	out := map[string]bool{}
	for _, it := range items {
		if v, ok := it[field].(string); ok {
			out[v] = true
		}
	}
	return out, nil
}

// shimTarget is where this binary is installed: <data>/bin/<version>/,
// a dev build under a content hash so every build gets its own directory.
func shimTarget(data string) (string, error) {
	version := buildinfo.Version
	if version == "" || version == "dev" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		h, err := fsutil.HashFile(exe)
		if err != nil {
			return "", err
		}
		version = "dev-" + h[:12]
	}
	name := "backsight"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(data, "bin", version, name), nil
}

// installShim copies this binary to target (if absent) and points the shim
// at it atomically (a copy on Windows, SPEC §3).
func installShim(target, shim string) error {
	if _, err := os.Stat(target); err != nil {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := copyFile(exe, target, 0o755); err != nil {
			return err
		}
	}
	if runtime.GOOS == "windows" {
		return copyFile(target, shim, 0o755)
	}
	tmp := shim + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, shim)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// atLeast compares dotted versions numerically.
func atLeast(v, minimum string) bool {
	a, b := strings.Split(v, "."), strings.Split(minimum, ".")
	for i := 0; i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x, _ = strconv.Atoi(a[i])
		}
		y, _ = strconv.Atoi(b[i])
		if x != y {
			return x > y
		}
	}
	return true
}

func installText(r clientInstallResult) string {
	var b strings.Builder
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "%-8s %s %s\n", s.Status, s.Name, s.Detail)
	}
	if !r.Plan && len(r.Steps) > 0 {
		b.WriteString("restart Claude Code (or /reload-plugins) to load the plugin\n")
	}
	return b.String()
}
