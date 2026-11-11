package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Barben360/backsight/internal/codexcfg"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/state"
)

// Codex's home and profile, detection and version, install, uninstall and
// drift: block 3 (https://github.com/Barben360/backsight/issues/278),
// 0085 D1 (home), D2 (the two plugins, their install commands), D5 (hook
// trust), D8 (the TOML editor), D9 (minimum version). The generated tree
// is codex_plugin.go's; config.toml's edits are internal/codexcfg's, run by
// the commands under the config editor's rules (they need its journal).

// CodexMinVersion is the oldest Codex every fact of 0085 was observed on
// (D9); setup and doctor refuse an older one.
const CodexMinVersion = "0.167.0"

// codexHomeVar moves Codex's home; Codex itself requires it to name an
// existing directory.
const codexHomeVar = "CODEX_HOME"

func (Codex) MinVersion() string { return CodexMinVersion }

// ProfileVar is "": the Codex home is resolved once (ShellHome) and
// recorded, never read from a shell afterwards, so the shared
// wiredProfile must find no variable to read (0085 D1).
func (Codex) ProfileVar() string { return "" }

// IsDefaultProfile reports that profile ("" included) is ~/.codex, also
// when named another way (a symlink; on Windows another case).
func (Codex) IsDefaultProfile(home, profile string) bool {
	if profile == "" {
		return true
	}
	def := filepath.Join(home, ".codex")
	a, errA := os.Stat(profile)
	b, errB := os.Stat(def)
	if errA == nil && errB == nil {
		return os.SameFile(a, b)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(profile), def)
	}
	return filepath.Clean(profile) == def
}

// Locate is Codex's files for env's profile: config.toml and sessions/ in
// the home, "" being ~/.codex.
func (Codex) Locate(env Env) Location {
	dir := env.Profile
	if dir == "" {
		dir = filepath.Join(env.Home, ".codex")
	}
	return Location{Dir: dir, Settings: filepath.Join(dir, "config.toml"), Conversations: filepath.Join(dir, "sessions")}
}

// ProfileEnv is what points the codex command at profile: CODEX_HOME for
// any home but ~/.codex, nothing for it (the runner drops the shell's).
func (c Codex) ProfileEnv(home, profile string) []string {
	if c.IsDefaultProfile(home, profile) {
		return nil
	}
	return []string{codexHomeVar + "=" + profile}
}

func (c Codex) run(ctx context.Context, env Env, args ...string) ([]byte, error) {
	if env.Run == nil {
		return nil, MissingError{Command: "codex"}
	}
	return env.Run(ctx, c.ProfileEnv(env.Home, env.Profile), args...)
}

// Detect is the version `codex --version` prints (codex-cli <x.y.z>).
func (c Codex) Detect(ctx context.Context, env Env) (string, error) {
	out, err := c.run(ctx, env, "--version")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "codex-cli" {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("codex --version: unexpected output %q", strings.TrimSpace(string(out)))
}

// codexPlugin is one entry of `codex plugin list --json`.
type codexPlugin struct {
	ID        string `json:"pluginId"`
	Version   string `json:"version"`
	Installed bool   `json:"installed"`
	Enabled   bool   `json:"enabled"`
	Source    struct {
		Path string `json:"path"`
	} `json:"source"`
}

// plugins are backsight's plugins `codex plugin list --json` reports
// installed, by id.
func (c Codex) plugins(ctx context.Context, env Env) (map[string]codexPlugin, error) {
	raw, err := c.run(ctx, env, "plugin", "list", "--json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Installed []codexPlugin `json:"installed"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("codex plugin list --json: unexpected output: %w", err)
	}
	out := map[string]codexPlugin{}
	for _, p := range list.Installed {
		for _, id := range CodexPluginIDs() {
			if p.ID == id && p.Installed {
				out[id] = p
			}
		}
	}
	return out, nil
}

// marketplaceRoot is where Codex reads backsight's marketplace from, ""
// when it is not added.
func (c Codex) marketplaceRoot(ctx context.Context, env Env) (string, error) {
	raw, err := c.run(ctx, env, "plugin", "marketplace", "list", "--json")
	if err != nil {
		return "", err
	}
	var list struct {
		Marketplaces []struct {
			Name   string `json:"name"`
			Root   string `json:"root"`
			Source struct {
				Type   string `json:"sourceType"`
				Source string `json:"source"`
			} `json:"marketplaceSource"`
		} `json:"marketplaces"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", fmt.Errorf("codex plugin marketplace list --json: unexpected output: %w", err)
	}
	for _, m := range list.Marketplaces {
		if m.Name != CodexMarketplace {
			continue
		}
		if t := m.Source.Type; t != "" && t != "local" {
			// A git or remote marketplace of the user's, named backsight.
			return foreignMarket + t + " " + m.Source.Source, nil
		}
		if m.Source.Source != "" {
			return m.Source.Source, nil
		}
		if m.Root == "" {
			return foreignMarket + "unknown source", nil // nothing says it is backsight's
		}
		return m.Root, nil
	}
	return "", nil
}

// foreignMarket starts the root marketplaceRoot gives for a marketplace
// named backsight that is not a local folder.
const foreignMarket = "\x00"

// ownMarket reports that root is a marketplace folder backsight generates
// for Codex (<data>/codex-marketplace/), here or at a moved data directory.
func ownMarket(root string) bool {
	return root != "" && !strings.HasPrefix(root, foreignMarket) && filepath.Base(filepath.Clean(root)) == filepath.Base(CodexMarketplaceDir(""))
}

// showMarket is root as the user reads it.
func showMarket(root string) string { return strings.TrimPrefix(root, foreignMarket) }

// Install generates the two plugins in <data>/codex-marketplace/ and adds
// them with Codex's own commands (0085 D2): the marketplace when it is not
// added from there, then each plugin that is not installed, is disabled,
// or whose files or version changed (Codex copies a local plugin only when
// it is added again). A step named "trust" says the hooks plugin was added
// (again): Codex asks the user to review its hooks before they run (D5).
func (c Codex) Install(ctx context.Context, env Env, in Integration, plan bool) (InstallResult, error) {
	var res InstallResult
	mkt := CodexMarketplaceDir(in.Data)
	want, err := codexTree(in.Shim, in.BinaryVersion, c.Skills(in.Shim))
	if err != nil {
		return res, &InstallError{Stage: Prepared, Err: err}
	}
	res.Version = CodexVersion(in.BinaryVersion, in.Shim)
	have, err := readTree(mkt)
	if err != nil {
		return res, &InstallError{Stage: Prepared, Err: err}
	}
	changed := codexChanged(have, want)
	if !plan && len(changed) > 0 {
		if err := writeTree(mkt, want); err != nil {
			return res, &InstallError{Stage: Prepared, Err: err}
		}
	}
	res.Steps = append(res.Steps, Step{Name: "plugin", Detail: mkt + " (version " + res.Version + ")", Changed: len(changed) > 0})

	root, err := c.marketplaceRoot(ctx, env)
	if err != nil {
		return res, &InstallError{Stage: Generated, Err: err}
	}
	installed, err := c.plugins(ctx, env)
	if err != nil {
		return res, &InstallError{Stage: Generated, Err: err}
	}
	var steps [][]string
	switch root {
	case mkt:
	case "":
		steps = append(steps, []string{"plugin", "marketplace", "add", mkt})
	default:
		// This install's own Claude Code marketplace (added by hand), or a
		// generated Codex marketplace whose folder is gone (this install's
		// data directory, moved), is replaced. Another generated one still
		// there is another install's (two installs cannot share a Codex
		// home), and any other marketplace named backsight is the user's:
		// both refused, never removed.
		_, statErr := os.Stat(filepath.Join(root, ".claude-plugin"))
		mine := filepath.Clean(root) == filepath.Join(in.Data, "marketplace") || (ownMarket(root) && statErr != nil)
		if !mine {
			why := "is wired to another backsight install, whose marketplace is " + root + ": give this install its own Codex home (CODEX_HOME or --codex-home), or uninstall the other one first"
			if !ownMarket(root) {
				why = "already has a marketplace named " + CodexMarketplace + " that backsight did not make (" + showMarket(root) + "): remove it (codex plugin marketplace remove " +
					CodexMarketplace + ") if it is no longer needed, or give backsight its own Codex home (CODEX_HOME or --codex-home)"
			}
			return res, &InstallError{Stage: Conflict, Err: errors.New("this Codex home (" + c.Locate(env).Dir + ") " + why)}
		}
		steps = append(steps, []string{"plugin", "marketplace", "remove", CodexMarketplace}, []string{"plugin", "marketplace", "add", mkt})
	}
	remarket := len(steps) > 0 // a marketplace added again: its plugins too
	hooksAdded := false
	for _, name := range []string{CodexMainPlugin, CodexHooksPlugin} {
		id := CodexPluginID(name)
		p, ok := installed[id]
		version := pluginVersion(want, name)
		again := !ok || !p.Enabled || changed[name] || p.Version != version || filepath.Clean(p.Source.Path) != filepath.Join(mkt, "plugins", name) || remarket
		if !again {
			continue
		}
		steps = append(steps, []string{"plugin", "add", id})
		hooksAdded = hooksAdded || name == CodexHooksPlugin
	}
	for _, args := range steps {
		if !plan {
			if _, err := c.run(ctx, env, args...); err != nil {
				return res, &InstallError{Stage: Registering, Err: err}
			}
		}
		res.Steps = append(res.Steps, Step{Name: "codex", Detail: strings.Join(args, " "), Changed: true})
	}
	if hooksAdded {
		res.Steps = append(res.Steps, Step{Name: "trust", Detail: CodexPluginID(CodexHooksPlugin) + " " + pluginVersion(want, CodexHooksPlugin) + ": Codex asks to review its hooks", Changed: true})
	}
	return res, nil
}

// pluginVersion is the version a generated plugin's manifest names.
func pluginVersion(tree map[string][]byte, name string) string {
	var m struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(tree["plugins/"+name+"/.claude-plugin/plugin.json"], &m)
	return m.Version
}

// Uninstall removes both plugins and the marketplace with Codex's own
// commands, then the plugin cache folder they leave, when empty (0085 D2,
// D8). The [hooks.state] tables they leave in config.toml are removed by
// the caller, through the config editor (codexcfg.RemoveTrust), which
// keeps a copy and a journal; Codex's .hook-trust/ files are its own.
func (c Codex) Uninstall(ctx context.Context, env Env) error {
	installed, err := c.plugins(ctx, env)
	if err != nil {
		return err
	}
	root, err := c.marketplaceRoot(ctx, env)
	if err != nil {
		return err
	}
	for _, id := range CodexPluginIDs() {
		// Only plugins of backsight's marketplace: a marketplace of the
		// user's named backsight, and what it installed, stay.
		if p, ok := installed[id]; ok && (ownMarket(root) || ownMarket(filepath.Dir(filepath.Dir(p.Source.Path)))) {
			if _, err := c.run(ctx, env, "plugin", "remove", id); err != nil {
				return err
			}
		}
	}
	if ownMarket(root) {
		if _, err := c.run(ctx, env, "plugin", "marketplace", "remove", CodexMarketplace); err != nil {
			return err
		}
	}
	removeEmptyDirs(filepath.Join(c.Locate(env).Dir, "plugins", "cache", CodexMarketplace))
	return nil
}

// removeEmptyDirs removes dir when it holds only empty directories.
func removeEmptyDirs(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() {
			return
		}
		removeEmptyDirs(filepath.Join(dir, e.Name()))
	}
	_ = os.Remove(dir) // fails, harmlessly, when something is still there
}

// Installed reports whether backsight's marketplace, or a plugin from it,
// is registered (`codex plugin list --json`, `codex plugin marketplace
// list --json`); a marketplace of the user's named backsight is not.
func (c Codex) Installed(ctx context.Context, env Env) (bool, error) {
	installed, err := c.plugins(ctx, env)
	if err != nil {
		return false, err
	}
	for _, p := range installed {
		if ownMarket(filepath.Dir(filepath.Dir(p.Source.Path))) {
			return true, nil
		}
	}
	root, err := c.marketplaceRoot(ctx, env)
	return ownMarket(root), err
}

// Drift lists what no longer matches the install (0085 D5): the plugin
// version for this binary, the generated files, the shim's target, and
// the hooks Codex has not recorded a trust for in config.toml (a hook
// trusted for an older declaration is not seen here: Codex hashes it). The
// config's switches wire nothing for Codex: cfg is unused. Cheap: a few
// small files and a link.
func (c Codex) Drift(env Env, rec Recorded, cfg *config.Config) []string {
	if rec.Version == "" {
		return nil // never installed: nothing to compare
	}
	var out []string
	shim := filepath.Join(rec.Data, "bin", plugin.ShimName(codexGOOS))
	if want := CodexVersion(rec.BinaryVersion, shim); want != rec.Version {
		out = append(out, "Codex plugin "+rec.Version+" is not this binary's ("+want+")")
	} else if want, err := codexTree(shim, rec.BinaryVersion, c.Skills(shim)); err == nil {
		have, _ := readTree(CodexMarketplaceDir(rec.Data))
		if len(codexChanged(have, want)) > 0 {
			out = append(out, "the Codex plugin files in "+CodexMarketplaceDir(rec.Data)+" are not this binary's")
		}
	}
	switch target, err := filepath.EvalSymlinks(shim); {
	case err != nil:
		out = append(out, "shim "+shim+" missing")
	case rec.ShimTarget == "", codexGOOS == "windows":
	default:
		want, werr := filepath.EvalSymlinks(rec.ShimTarget)
		if werr != nil || target != want {
			out = append(out, "shim points to "+target+", not "+rec.ShimTarget)
		}
	}
	if untrusted := c.Untrusted(env); len(untrusted) > 0 {
		out = append(out, fmt.Sprintf("Codex has not trusted %d of backsight's hooks yet: open Codex and review them (/hooks)", len(untrusted)))
	}
	return out
}

// Untrusted are the trust keys of backsight's hooks that config.toml
// records no trusted_hash for (CodexTrustKeys); nil when config.toml
// cannot be read (nothing is claimed then).
func (c Codex) Untrusted(env Env) []string {
	raw, err := os.ReadFile(c.Locate(env).Settings)
	if errors.Is(err, fs.ErrNotExist) {
		raw, err = nil, nil
	}
	if err != nil {
		return nil
	}
	var out []string
	for _, key := range CodexTrustKeys() {
		if _, ok, err := codexcfg.Lookup(raw, []string{"hooks", "state", key, "trusted_hash"}); err != nil {
			return nil
		} else if !ok {
			out = append(out, key)
		}
	}
	return out
}

// NameClash names an MCP server of the user's that is called memory, as
// backsight's is: its tools would be mcp__memory__… too, and backsight's
// PostToolUse hook would take them for its own (0085 D3). It reads
// config.toml ([mcp_servers.memory]) and `codex mcp list --json`, where
// backsight's own server (the one running shim) is not a clash. "" when
// there is none.
func (c Codex) NameClash(ctx context.Context, env Env, shim string) (string, error) {
	raw, err := os.ReadFile(c.Locate(env).Settings)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if there, err := codexcfg.MCPServerTable(raw, plugin.Server); err != nil {
		return "", fmt.Errorf("%s: %w", c.Locate(env).Settings, err)
	} else if there {
		return "[mcp_servers." + plugin.Server + "] in " + c.Locate(env).Settings, nil
	}
	out, err := c.run(ctx, env, "mcp", "list", "--json")
	if err != nil {
		return "", err
	}
	var servers []struct {
		Name      string `json:"name"`
		Transport struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			URL     string `json:"url"`
		} `json:"transport"`
	}
	if err := json.Unmarshal(out, &servers); err != nil {
		return "", fmt.Errorf("codex mcp list --json: unexpected output: %w", err)
	}
	for _, s := range servers {
		if s.Name != plugin.Server || (s.Transport.Command != "" && filepath.Clean(s.Transport.Command) == filepath.Clean(shim)) {
			continue
		}
		what := s.Transport.Command
		if what == "" {
			what = s.Transport.URL
		}
		return "the MCP server " + plugin.Server + " (" + strings.TrimSpace(s.Transport.Type+" "+what) + ") that codex mcp list shows", nil
	}
	return "", nil
}

// MemoriesOn reports whether Codex's own memories are on, as `codex
// features list` gives their effective state (row memories).
func (c Codex) MemoriesOn(ctx context.Context, env Env) (bool, error) {
	out, err := c.run(ctx, env, "features", "list")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "memories" {
			return f[len(f)-1] == "true", nil
		}
	}
	return false, errors.New("codex features list: no memories row")
}

// HookCommand is the hook handler's command string for backsight's event:
// the shim quoted for sh ("command") or, windows, for cmd /C
// ("commandWindows"), then HookArgs(event) (codex.go's contract).
func (c Codex) HookCommand(shim, event string, windows bool) (string, error) {
	quoted := QuoteSh(shim)
	if windows {
		q, err := QuoteCmd(shim)
		if err != nil {
			return "", fmt.Errorf("the shim %s %w", shim, err)
		}
		quoted = q
	}
	return quoted + " " + strings.Join(c.HookArgs(event), " "), nil
}

// ShellHome is the Codex home setup and client install resolve, once:
// flag when set (--codex-home), else $CODEX_HOME when set and non-empty,
// else ~/.codex (0085 D1), as an absolute path. A home named by the flag
// or the variable must be an existing directory, as Codex requires;
// ~/.codex is made by Codex at its first run.
func (Codex) ShellHome(home, flag string, getenv func(string) string) (string, error) {
	p, from := flag, "--codex-home"
	if p == "" && getenv != nil {
		p, from = getenv(codexHomeVar), codexHomeVar
	}
	if p == "" {
		if home == "" {
			return "", errors.New("no home directory to find ~/.codex in")
		}
		return filepath.Join(home, ".codex"), nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s names %s, which is not a directory (Codex needs it to exist)", from, abs)
	}
	return abs, nil
}

// RecordHome records codexHome in s's Codex entry,
// s.ClientFor(harnessid.Codex).Profile: "" is ~/.codex, any other must be
// absolute (the caller resolved it with ShellHome).
func (Codex) RecordHome(s *state.State, codexHome string) error {
	if codexHome != "" && !filepath.IsAbs(codexHome) {
		return fmt.Errorf("the Codex home %q is not absolute", codexHome)
	}
	s.ClientFor(harnessid.Codex).Profile = codexHome
	return nil
}

// RecordedHome is the Codex home client install recorded,
// st.ProfileOf(harnessid.Codex) ("" for ~/.codex): what every command
// after install acts on.
func (Codex) RecordedHome(st state.State) (string, error) {
	return st.ProfileOf(harnessid.Codex), nil
}
