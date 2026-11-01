package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/selfcheck"
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
	cmd.Env = claudeEnviron(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr // warnings on stderr never corrupt the JSON
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("claude %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()+" "+stdout.String()))
	}
	return stdout.Bytes(), nil
}

var claudeRunner claudeCLI = execClaude{}

// claudeEnviron is the environment a claude call runs with: this
// process's without CLAUDE_CONFIG_DIR, then env. The caller names the
// profile (profileEnv), none meaning ~/.claude: a shell set to another
// profile never redirects a call meant for ~/.claude (0067 block 3 review).
func claudeEnviron(env []string) []string {
	out := make([]string, 0, len(os.Environ())+len(env))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDE_CONFIG_DIR" || (runtime.GOOS == "windows" && strings.EqualFold(k, "CLAUDE_CONFIG_DIR")) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, env...)
}

// selfExe is the running binary; tests replace it.
var selfExe = os.Executable

type installStep struct {
	Name   string `json:"name"`
	Status string `json:"status"` // done | skipped | planned | unchanged | failed | deferred
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
	_, home, err := machine()
	if err != nil {
		return res, err
	}
	// The profile located from --config-dir or the shell, named to claude.
	env := profileEnv(home, c.loc.Dir)
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
	}
	add("shim", did(shimChanged), shim+" → "+target)
	// The command on PATH runs the shim (decision 0067 block review), and
	// is made when absent, as install.sh makes it (M5 review rules, PATH).
	if link, older := olderPathLink(c.dirs, shim); older || absentPathLink(c.dirs) {
		if !plan {
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				return res, blocked(err)
			}
			if err := pointShim(shim, link); err != nil {
				return res, blocked(err)
			}
		}
		add("path", did(true), link+" → "+shim)
	}
	if runtime.GOOS == "windows" && c.dirs.Bin != "" {
		// The PATH copy is found through the user's PATH (HKCU, no admin).
		added := false
		if !plan {
			if added, err = ensureUserPath(c.dirs.Bin); err != nil {
				return res, blocked(fmt.Errorf("adding %s to the user PATH: %w", c.dirs.Bin, err))
			}
		} else {
			added = true // planned: reported as a possible change
		}
		add("userpath", did(added), c.dirs.Bin+" (user PATH, takes effect in new terminals)")
	}
	if !plan {
		pruneBinaries(c.dirs.Data, c.dirs.Bin, target, before.ShimTarget)
	}

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
	known, err := pluginIDs(ctx, env)
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
		schedulerStep(ctx, true, add)
		bankStep(ctx, true, add)
		return res, nil
	}
	run, files, err := c.runner("client install").Apply([]claudecfg.Edit{edit})
	if err != nil {
		return res, partialError(err)
	}
	add("permission rule", did(files[0].Written), strings.TrimSpace(c.loc.Settings+" "+run))

	// The profile this install wired, from --config-dir or the shell's
	// CLAUDE_CONFIG_DIR: a scheduled job has neither and reads the recorded
	// one. "" is ~/.claude.
	profile := ""
	if filepath.Clean(c.loc.Dir) != filepath.Join(home, ".claude") {
		profile, _ = filepath.Abs(c.loc.Dir)
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
	schedulerStep(ctx, false, add)
	bankStep(ctx, false, add)
	return res, nil
}

// schedulerStep refreshes the installed scheduler entries (issue 222): the
// new binary's client install is how an update apply brings them to its
// templates, an older binary's update apply included. The entries are
// independent of the plugin, so a failure is reported (and doctor flags
// the entries) without failing the install, nor an update with it.
func schedulerStep(ctx context.Context, plan bool, add func(name, status, detail string)) {
	r, err := refreshScheduler(ctx, plan)
	switch {
	case err != nil:
		add("scheduler", "failed", err.Error()+" (backsight jobs refresh retries)")
	case r.Status == "none":
	case r.Status == "unchanged":
		add("scheduler", "unchanged", r.Kind+" entries up to date")
	default:
		add("scheduler", r.Status, refreshDetail(r))
	}
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

// pluginIDs is the set of plugin ids `claude plugin list` reports.
func pluginIDs(ctx context.Context, env []string) (map[string]bool, error) {
	args := []string{"plugin", "list", "--json"}
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
		if v, ok := it["id"].(string); ok {
			out[v] = true
		}
	}
	return out, nil
}

// shimTarget is where this binary is installed: <data>/bin/<version>-<hash>/,
// so every distinct build (a rebuilt dev or dirty binary included) gets its
// own directory and the shim always runs the binary that installed it.
func shimTarget(data string) (string, error) {
	exe, err := selfExe()
	if err != nil {
		return "", err
	}
	h, err := fsutil.HashFile(exe)
	if err != nil {
		return "", err
	}
	return binaryPath(data, buildinfo.Version, h), nil
}

// binaryPath is <data>/bin/<version>-<hash[:12]>/backsight, where a
// build of that version and SHA-256 is installed (client install, update
// apply).
func binaryPath(data, version, hash string) string {
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
	return filepath.Join(data, "bin", version+"-"+hash[:12], name)
}

// installShim copies this binary to target (unless an identical copy is
// there) and points the shim at it: a symlink swapped atomically, or on
// Windows a copy, the running one renamed aside first (Windows lets a
// running image be renamed, not replaced).
func installShim(target, shim string) error {
	exe, err := selfExe()
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
	// The signed checksums.txt install.sh kept next to the binary goes with
	// the copy, which checks itself against it (SPEC §11.1). A dev build
	// has none.
	if real, err := filepath.EvalSymlinks(exe); err == nil && filepath.Dir(real) != filepath.Dir(target) {
		for _, name := range []string{selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
			src := filepath.Join(filepath.Dir(real), name)
			if _, err := os.Stat(src); err != nil {
				continue
			}
			if err := copyFile(src, filepath.Join(filepath.Dir(target), name), 0o644); err != nil {
				return err
			}
		}
	}
	return pointShim(target, shim)
}

// pointShim makes the shim run target: a symlink swapped atomically, or
// on Windows a copy, the running one renamed aside under a unique name
// (pointShimWindows).
func pointShim(target, shim string) error {
	if runtime.GOOS == "windows" {
		return pointShimWindows(target, shim)
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
	// The same file under two names (an 8.3 short path and its long one, a
	// link) is already copied: replacing it with itself fails on Windows
	// (it is open for reading) and is a no-op elsewhere.
	if si, err := os.Stat(src); err == nil {
		if di, err := os.Stat(dst); err == nil && os.SameFile(si, di) {
			return nil
		}
	}
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

// pathCommand is the command on PATH: <bin>/backsight, on Windows the
// copy <bin>\backsight.exe (a symlink needs a privilege a standard user
// lacks, and a PATH lookup finds .exe files).
func pathCommand(bin string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(bin, "backsight.exe")
	}
	return filepath.Join(bin, "backsight")
}

// olderPathLink reports the command on PATH (<bin>/backsight) when it is
// a link to one of this install's versioned binaries, as install.sh made
// it before decision 0067's block review: it then runs that version
// whatever update apply installs, and dangles once pruning removes it.
// Anything else there (none, a file, a link elsewhere: a developer's
// build) is the user's and stays. On Windows the command is a copy of the
// shim that every install and update swaps, so one that exists is always
// ours to refresh.
func olderPathLink(d paths.Dirs, shim string) (string, bool) {
	if d.Bin == "" {
		return "", false
	}
	link := pathCommand(d.Bin)
	if runtime.GOOS == "windows" {
		fi, err := os.Lstat(link)
		return link, err == nil && fi.Mode().IsRegular()
	}
	dest, err := os.Readlink(link)
	if err != nil {
		return link, false
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(d.Bin, dest)
	}
	dest = filepath.Clean(dest)
	return link, dest != filepath.Clean(shim) && strings.HasPrefix(dest, filepath.Join(d.Data, "bin")+string(filepath.Separator))
}

// absentPathLink reports that nothing is at <bin>/backsight (not even a
// dangling link): client install then makes the PATH command, a link to
// the shim, on Windows a copy of it.
func absentPathLink(d paths.Dirs) bool {
	if d.Bin == "" {
		return false
	}
	_, err := os.Lstat(pathCommand(d.Bin))
	return errors.Is(err, fs.ErrNotExist)
}

// keepBinaries: installed versions kept besides the current and previous.
const keepBinaries = 3

// pruneBinaries removes old <data>/bin/<version>/ directories, never the
// shim's current or previous target, nor the binary the command on PATH
// (<bin>/backsight) runs.
func pruneBinaries(data, bin, current, previous string) {
	dir := filepath.Join(data, "bin")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keep := []string{current, previous}
	if p, err := filepath.EvalSymlinks(pathCommand(bin)); err == nil {
		keep = append(keep, p)
		// the same directory reached through a symlinked data directory
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			if rel, err := filepath.Rel(real, p); err == nil && !strings.HasPrefix(rel, "..") {
				keep = append(keep, filepath.Join(dir, rel))
			}
		}
	}
	type ver struct {
		path string
		mod  time.Time
	}
	var vs []ver
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		kept := false
		for _, k := range keep {
			kept = kept || (k != "" && strings.HasPrefix(k, p+string(filepath.Separator)))
		}
		if !e.IsDir() || kept {
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
