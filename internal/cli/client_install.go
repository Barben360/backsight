package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr // warnings on stderr never corrupt the JSON
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("claude %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()+" "+stdout.String()))
	}
	return stdout.Bytes(), nil
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
	did := func(changed bool) string {
		switch {
		case plan && changed:
			return "planned"
		case changed:
			return "done"
		}
		return "unchanged"
	}
	if !plan {
		l, _, err := state.Acquire(filepath.Join(c.dirs.LocksDir(), "client-install.lock"), "client install")
		if err != nil {
			return res, &output.Error{Code: output.Blocked, Message: "another client install is running"}
		}
		defer func() { _ = l.Release() }()
	}
	before, _ := state.Load(c.dirs.StateFile())

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
	shim := filepath.Join(c.dirs.Data, "bin", plugin.ShimName(runtime.GOOS))
	target, err := shimTarget(c.dirs.Data)
	if err != nil {
		return res, blocked(err)
	}
	current, _ := filepath.EvalSymlinks(shim)
	wantTarget, _ := filepath.EvalSymlinks(target)
	shimChanged := runtime.GOOS == "windows" || current == "" || current != wantTarget
	if !plan {
		if err := installShim(target, shim); err != nil {
			return res, blocked(err)
		}
		pruneBinaries(c.dirs.Data, target, before.ShimTarget)
	}
	add("shim", did(shimChanged), shim+" → "+target)

	// 3. The plugin, generated whole in a local marketplace (E1).
	mkt := filepath.Join(c.dirs.Data, "marketplace")
	pversion := plugin.Version(buildinfo.Version, shim)
	pluginChanged := true
	if !plan {
		if pluginChanged, err = plugin.Generate(mkt, shim, buildinfo.Version); err != nil {
			return res, blocked(err)
		}
	}
	add("plugin", did(pluginChanged), mkt+" (version "+pversion+")")

	// 4. Marketplace and plugin registered with Claude Code; a marketplace
	// registered at another path (moved data directory) is re-added.
	failAfterWrites := func(err error) error { // the shim and plugin may already be rewritten
		if plan {
			return blocked(err)
		}
		return partialError(err)
	}
	known, err := listed(ctx, env, "id", "plugin", "list", "--json")
	if err != nil {
		return res, failAfterWrites(err)
	}
	marketPath, err := marketplacePath(ctx, env)
	if err != nil {
		return res, failAfterWrites(err)
	}
	// A marketplace at another path that still exists is another install's:
	// two installs cannot share a profile (the plugin id is backsight's).
	// A path that is gone is this install's data directory, moved.
	if marketPath != "" && marketPath != "?" && marketPath != mkt {
		if _, err := os.Stat(filepath.Join(marketPath, ".claude-plugin")); err == nil {
			return res, &output.Error{Code: output.Blocked, Message: "this Claude Code profile (" + c.loc.Dir + ") is wired to another backsight install, whose marketplace is " +
				marketPath + ": give this install its own profile (CLAUDE_CONFIG_DIR or --config-dir), or uninstall the other one first"}
		}
	}
	var steps [][]string
	switch {
	case marketPath == "":
		steps = append(steps, []string{"plugin", "marketplace", "add", mkt})
	case marketPath != mkt:
		steps = append(steps, []string{"plugin", "marketplace", "remove", plugin.Marketplace}, []string{"plugin", "marketplace", "add", mkt})
	default:
		steps = append(steps, []string{"plugin", "marketplace", "update", plugin.Marketplace})
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
		add("claude", did(true), strings.Join(args, " "))
	}

	// 5. The one settings change: allow the three memory tools (SPEC §5).
	edit := claudecfg.Edit{Target: c.loc.Settings, Apply: permissionRule}
	if plan {
		fr, err := claudecfg.Plan([]claudecfg.Edit{edit})
		if err != nil {
			return res, editError(err)
		}
		add("permission rule", did(len(fr[0].Changes) > 0), c.loc.Settings)
		return res, nil
	}
	run, files, err := c.runner("client install").Apply([]claudecfg.Edit{edit})
	if err != nil {
		return res, partialError(err)
	}
	add("permission rule", did(files[0].Written), strings.TrimSpace(c.loc.Settings+" "+run))

	profile := ""
	if configDir != "" {
		profile, _ = filepath.Abs(configDir)
	}
	if err := state.Update(c.dirs, "client install", func(s *state.State) error {
		if s.ShimTarget != "" && s.ShimTarget != target {
			if s.Client == nil {
				s.Client = &state.Client{}
			}
			s.Client.ShimPrevious = s.ShimTarget
		}
		s.PluginVersion, s.ShimTarget = pversion, target
		if s.Client == nil {
			s.Client = &state.Client{}
		}
		s.Client.Profile = profile
		return nil
	}); err != nil {
		return res, partialError(err)
	}
	return res, nil
}

// marketplacePath is where Claude Code reads backsight's marketplace from,
// "" when it is not registered.
func marketplacePath(ctx context.Context, env []string) (string, error) {
	raw, err := claudeRunner.Run(ctx, env, "plugin", "marketplace", "list", "--json")
	if err != nil {
		return "", err
	}
	var items []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", fmt.Errorf("claude plugin marketplace list: unexpected output: %w", err)
	}
	for _, m := range items {
		if m.Name == plugin.Marketplace {
			if m.Path == "" {
				return "?", nil // registered, path unknown: re-add to be sure
			}
			return m.Path, nil
		}
	}
	return "", nil
}

func needsClaude(msg string) error {
	return &output.Error{Code: output.NeedsUser, Message: msg, Actions: []output.Action{
		{Kind: "install", Cmd: "npm install -g @anthropic-ai/claude-code", Why: "backsight wires itself into Claude Code through its plugin commands (or `claude update` for an old version)"},
		{Kind: "run_as_user", Cmd: "backsight client install", Why: "then wire Claude Code again"},
	}, Resume: "backsight client install"}
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

// shimTarget is where this binary is installed: <data>/bin/<version>-<hash>/,
// so every distinct build (a rebuilt dev or dirty binary included) gets its
// own directory and the shim always runs the binary that installed it.
func shimTarget(data string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	h, err := fsutil.HashFile(exe)
	if err != nil {
		return "", err
	}
	version := buildinfo.Version
	if version == "" {
		version = "dev"
	}
	version = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ' ' {
			return '_'
		}
		return r
	}, version)
	name := "backsight"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(data, "bin", version+"-"+h[:12], name), nil
}

// installShim copies this binary to target (unless an identical copy is
// there) and points the shim at it: a symlink swapped atomically, or on
// Windows a copy, the running one renamed aside first (Windows lets a
// running image be renamed, not replaced).
func installShim(target, shim string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	want, err := fsutil.HashFile(exe)
	if err != nil {
		return err
	}
	if got, _ := fsutil.HashFile(target); got != want {
		if err := copyFile(exe, target, 0o755); err != nil {
			return err
		}
	}
	if runtime.GOOS == "windows" {
		old := shim + ".old"
		_ = os.Remove(old) // fails while a previous session still runs it: fine
		if _, err := os.Stat(shim); err == nil {
			if err := os.Rename(shim, old); err != nil {
				return err
			}
		}
		return copyFile(target, shim, 0o755)
	}
	tmp, err := os.CreateTemp(filepath.Dir(shim), ".current-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(name)
	if err := os.Symlink(target, name); err != nil {
		return err
	}
	if err := os.Rename(name, shim); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// copyFile copies src to dst durably: unique temp file, fsync, rename.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = out.Close()
			_ = os.Remove(out.Name())
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Chmod(mode); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(out.Name(), dst); err != nil {
		return err
	}
	ok = true
	return nil
}

// keepBinaries: installed versions kept besides the current and previous.
const keepBinaries = 3

// pruneBinaries removes old <data>/bin/<version>/ directories, never the
// shim's current or previous target.
func pruneBinaries(data, current, previous string) {
	dir := filepath.Join(data, "bin")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type ver struct {
		path string
		mod  time.Time
	}
	var vs []ver
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if !e.IsDir() || strings.HasPrefix(current, p+string(filepath.Separator)) || (previous != "" && strings.HasPrefix(previous, p+string(filepath.Separator))) {
			continue
		}
		if fi, err := e.Info(); err == nil {
			vs = append(vs, ver{p, fi.ModTime()})
		}
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].mod.After(vs[j].mod) })
	for i, v := range vs {
		if i >= keepBinaries {
			_ = os.RemoveAll(v.path)
		}
	}
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
