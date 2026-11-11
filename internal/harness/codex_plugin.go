package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/plugin"
)

// Codex's generated plugins (block 3, https://github.com/Barben360/backsight/issues/278;
// 0085 D2): a marketplace of its own, <data>/codex-marketplace/, so a Claude
// Code update never touches it, holding two plugins:
//
//   - backsight@backsight: the memory server (.mcp.json) and the skills;
//   - backsight-hooks@backsight: the hooks only (hooks/hooks.json), its
//     manifest's version a hash of the hook set, so that its folder's bytes,
//     which Codex's hook trust pins whole, change only when a hook does (a
//     new binary or a changed skill leaves the hooks trusted, D5).

// Codex's marketplace and plugin names.
const (
	CodexMarketplace = plugin.Marketplace // "backsight"
	CodexMainPlugin  = plugin.Name        // "backsight"
	CodexHooksPlugin = plugin.Name + "-hooks"
	// codexLayout changes whenever the generated tree changes shape, so a
	// development build gets a new plugin version.
	codexLayout = "1"
)

// CodexPluginID is a plugin's id under Codex: <plugin>@<marketplace>.
func CodexPluginID(name string) string { return name + "@" + CodexMarketplace }

// CodexPluginIDs are both plugins' ids, the main one first.
func CodexPluginIDs() []string {
	return []string{CodexPluginID(CodexMainPlugin), CodexPluginID(CodexHooksPlugin)}
}

// CodexMarketplaceDir is where the Codex marketplace is generated.
func CodexMarketplaceDir(data string) string { return filepath.Join(data, "codex-marketplace") }

// codexGOOS is the system the plugins are generated for; tests replace it.
var codexGOOS = runtime.GOOS

// codexHook is one handler of hooks.json, in the field order Codex's
// review shows (no args: Codex ignores them, C14 §1).
type codexHook struct {
	Type           string `json:"type"`
	Command        string `json:"command"`
	CommandWindows string `json:"commandWindows,omitempty"`
	Timeout        int    `json:"timeout"`
}

type codexGroup struct {
	Matcher string      `json:"matcher,omitempty"`
	Hooks   []codexHook `json:"hooks"`
}

// codexHooksJSON is hooks/hooks.json for shim: the rows of Events grouped
// by Codex's event name, in order (SessionStart has two). Each handler
// runs the shim quoted for sh ("command") and for cmd /C
// ("commandWindows"), with its timeout. A shim cmd cannot read back is
// refused on Windows; elsewhere its commandWindows, never run, is left out.
func codexHooksJSON(shim string) ([]byte, error) {
	c := Codex{}
	hooks := map[string][]codexGroup{}
	for _, e := range c.Events() {
		cmd, err := c.HookCommand(shim, e.Event, false)
		if err != nil {
			return nil, err
		}
		win, err := c.HookCommand(shim, e.Event, true)
		if err != nil {
			if codexGOOS == "windows" {
				return nil, err
			}
			win = ""
		}
		hooks[e.Name] = append(hooks[e.Name], codexGroup{Matcher: e.Matcher, Hooks: []codexHook{{Type: "command", Command: cmd, CommandWindows: win, Timeout: e.Timeout}}})
	}
	return marshalDoc(map[string]any{"hooks": hooks})
}

func marshalDoc(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func shortHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:6])
}

// CodexHooksVersion is the hooks plugin's version: a hash of its
// hooks.json, nothing else (the binary's version is not in it).
func codexHooksVersion(hooksJSON []byte) string { return "1.0.0+" + shortHash(string(hooksJSON)) }

// CodexVersion is the main plugin's version for binary and shim: the
// binary's, plus a hash of what the generated files depend on (the shim,
// the layout), as Claude Code's plugin version is made.
func CodexVersion(binary, shim string) string {
	h := shortHash(codexLayout, shim)
	switch {
	case binary == "" || binary == "dev":
		return "0.0.0-dev+" + h
	case strings.Contains(binary, "+"):
		return binary + "." + h
	}
	return binary + "+" + h
}

// codexTree is the marketplace for shim, binary and skills (by path
// relative to the main plugin's root), relative to the marketplace's
// directory, slash-separated.
func codexTree(shim, binary string, skills map[string][]byte) (map[string][]byte, error) {
	hooksJSON, err := codexHooksJSON(shim)
	if err != nil {
		return nil, err
	}
	main, hooks := "plugins/"+CodexMainPlugin+"/", "plugins/"+CodexHooksPlugin+"/"
	docs := map[string]any{
		".claude-plugin/marketplace.json": map[string]any{
			"name":        CodexMarketplace,
			"description": "backsight's generated plugins: long-term memory for Codex",
			"owner":       map[string]string{"name": "backsight"},
			"plugins": []map[string]string{
				{"name": CodexMainPlugin, "source": "./" + strings.TrimSuffix(main, "/"), "description": "Long-term memory for Codex on your own Hindsight server: the memory tools and skills"},
				{"name": CodexHooksPlugin, "source": "./" + strings.TrimSuffix(hooks, "/"), "description": "backsight's hooks for Codex: recall, briefing, the end-of-turn check"},
			},
		},
		main + ".claude-plugin/plugin.json": map[string]any{
			"name": CodexMainPlugin, "version": CodexVersion(binary, shim), "author": map[string]string{"name": "backsight"},
			"description": "Long-term memory for Codex on your own Hindsight server: the memory tools and skills",
		},
		main + ".mcp.json": map[string]any{"mcpServers": map[string]any{
			plugin.Server: map[string]any{"command": shim, "args": Codex{}.MCPArgs(), "env_vars": Codex{}.MCPEnvVars()},
		}},
		hooks + ".claude-plugin/plugin.json": map[string]any{
			"name": CodexHooksPlugin, "version": codexHooksVersion(hooksJSON), "author": map[string]string{"name": "backsight"},
			"description": "backsight's hooks for Codex: recall, briefing, the end-of-turn check",
		},
	}
	out := map[string][]byte{hooks + "hooks/hooks.json": hooksJSON}
	for p, d := range docs {
		raw, err := marshalDoc(d)
		if err != nil {
			return nil, err
		}
		out[p] = raw
	}
	for p, raw := range skills {
		out[main+p] = raw
	}
	return out, nil
}

// codexChanged lists what differs between two trees: the plugins (by
// name) and "marketplace" for the marketplace's own manifest.
func codexChanged(have, want map[string][]byte) map[string]bool {
	out := map[string]bool{}
	mark := func(p string) {
		switch {
		case strings.HasPrefix(p, "plugins/"+CodexMainPlugin+"/"):
			out[CodexMainPlugin] = true
		case strings.HasPrefix(p, "plugins/"+CodexHooksPlugin+"/"):
			out[CodexHooksPlugin] = true
		default:
			out["marketplace"] = true
		}
	}
	for p, v := range want {
		if w, ok := have[p]; !ok || !bytes.Equal(v, w) {
			mark(p)
		}
	}
	for p := range have {
		if _, ok := want[p]; !ok {
			mark(p)
		}
	}
	return out
}

// codexOwned are the top-level entries of the marketplace the generator
// replaces whole.
var codexOwned = []string{".claude-plugin", "plugins"}

// readTree is the generated files under dir (none when it is missing).
func readTree(dir string) (map[string][]byte, error) {
	have := map[string][]byte{}
	for _, o := range codexOwned {
		root := filepath.Join(dir, o)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			have[filepath.ToSlash(rel)] = raw
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return have, nil
}

// writeTree writes want into dir whole, beside it first and swapped in,
// so Codex never copies half of it.
func writeTree(dir string, want map[string][]byte) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".new-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := fsutil.WriteAtomic(filepath.Join(tmp, filepath.FromSlash(p)), want[p], 0o644); err != nil {
			return err
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	old := ""
	if _, err := os.Stat(dir); err == nil {
		old = tmp + ".old"
		if err := os.Rename(dir, old); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		if old != "" {
			_ = os.Rename(old, dir)
		}
		return err
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return nil
}

// codexEventSnake is Codex's snake_case name of an event (SessionStart:
// session_start), as its hook trust keys write it.
func codexEventSnake(name string) string {
	var b strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// CodexTrustKeys are the [hooks.state."<key>"] keys under which Codex
// records its trust of each backsight hook: <plugin id>:hooks/hooks.json:
// <event>:<group>:<handler> (C14 §3).
func CodexTrustKeys() []string {
	var out []string
	group := map[string]int{}
	for _, e := range (Codex{}).Events() {
		out = append(out, CodexPluginID(CodexHooksPlugin)+":hooks/hooks.json:"+codexEventSnake(e.Name)+":"+strconv.Itoa(group[e.Name])+":0")
		group[e.Name]++
	}
	return out
}
