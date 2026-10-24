// Package plugin generates backsight's Claude Code integration: a local
// marketplace with one plugin whose hooks and MCP server exec the stable
// shim (SPEC §5, E1). The plugin is generated whole on every install or
// update; Claude Code copies it into its own cache.
package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/instructions"
)

// Names fixed by SPEC §5: tools are mcp__plugin_backsight_memory__<tool>.
const (
	Marketplace = "backsight"
	Name        = "backsight"
	Server      = "memory"
	// layout changes whenever the generated files change shape, so dev
	// builds get a new plugin version.
	layout = "3"
)

// ToolNames are the three memory tools as Claude Code names them.
var ToolNames = []string{
	"mcp__plugin_" + Name + "_" + Server + "__memory_search",
	"mcp__plugin_" + Name + "_" + Server + "__memory_read",
	"mcp__plugin_" + Name + "_" + Server + "__memory_store",
}

// event is one hooks.json entry: Claude Code event, matcher, backsight
// event, timeout in seconds (SPEC §6.2; above the supervisor's deadline).
type event struct {
	name, matcher, arg string
	timeout            int
}

var events = []event{
	{"SessionStart", "startup", "session-start", 10},
	{"SessionStart", "resume|clear|compact|fork", "session-start", 5},
	{"UserPromptSubmit", "", "user-prompt-submit", 25},
	{"PreToolUse", "AskUserQuestion", "pre-tool-use", 10},
	{"PostToolUse", "mcp__plugin_" + Name + "_" + Server + "__.*", "post-tool-use", 2},
	{"SubagentStart", "", "subagent-start", 5},
	{"SubagentStop", "", "subagent-stop", 5},
	{"Stop", "", "stop", 5},
	{"SessionEnd", "", "session-end", 1}, // 1.5 s total, not raisable from a plugin (C06)
}

// Version is the plugin version: the binary's, plus a hash of what the
// generated files depend on (the shim path), so any change to them is an
// update Claude Code applies (a moved data directory included).
func Version(binary, shim string) string {
	sum := sha256.Sum256([]byte(layout + "\n" + shim))
	h := hex.EncodeToString(sum[:6])
	if binary == "" || binary == "dev" {
		return "0.0.0-dev+" + h
	}
	if strings.Contains(binary, "+") {
		return binary + "." + h
	}
	return binary + "+" + h
}

// ShimName is the shim's file name: Windows exec form needs a real .exe.
func ShimName(goos string) string {
	if goos == "windows" {
		return "current.exe"
	}
	return "current"
}

// files is the generated tree, relative to the marketplace directory.
func files(shim, version string) (map[string][]byte, error) {
	type hookCmd struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Timeout int      `json:"timeout"`
	}
	type group struct {
		Matcher string    `json:"matcher,omitempty"`
		Hooks   []hookCmd `json:"hooks"`
	}
	hooks := map[string][]group{}
	for _, e := range events {
		hooks[e.name] = append(hooks[e.name], group{Matcher: e.matcher, Hooks: []hookCmd{{"command", shim, []string{"hook", e.arg}, e.timeout}}})
	}
	docs := map[string]any{
		".claude-plugin/marketplace.json": map[string]any{
			"name":        Marketplace,
			"description": "backsight's generated plugin: long-term memory for Claude Code",
			"owner":       map[string]string{"name": "backsight"},
			"plugins":     []map[string]string{{"name": Name, "source": "./plugins/" + Name, "description": "Long-term memory for Claude Code on your own Hindsight server"}},
		},
		"plugins/" + Name + "/.claude-plugin/plugin.json": map[string]any{
			"name": Name, "version": version, "author": map[string]string{"name": "backsight"},
			"description": "Long-term memory for Claude Code on your own Hindsight server: recall, briefing, memory tools",
		},
		"plugins/" + Name + "/hooks/hooks.json": map[string]any{"hooks": hooks},
		"plugins/" + Name + "/.mcp.json": map[string]any{"mcpServers": map[string]any{
			Server: map[string]any{"command": shim, "args": []string{"mcp"}, "alwaysLoad": true},
		}},
	}
	out := map[string][]byte{}
	for _, sk := range instructions.Skills(shim) {
		out["plugins/"+Name+"/skills/"+sk.Name+"/SKILL.md"] = []byte(instructions.SkillFile(sk))
	}
	for p, d := range docs {
		raw, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return nil, err
		}
		out[p] = append(raw, '\n')
	}
	return out, nil
}

// owned are the top-level entries of the marketplace directory the
// generator replaces whole.
var owned = []string{".claude-plugin", "plugins"}

// Generate writes the marketplace into dir; changed says whether anything
// differed. The new tree is written beside dir and swapped in, so Claude
// Code (which reads the plugin from this folder) never sees half of it.
func Generate(dir, shim, binaryVersion string) (bool, error) {
	want, err := files(shim, Version(binaryVersion, shim))
	if err != nil {
		return false, err
	}
	have, err := current(dir)
	if err != nil {
		return false, err
	}
	if same(have, want) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".new-")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := fsutil.WriteAtomic(filepath.Join(tmp, filepath.FromSlash(p)), want[p], 0o644); err != nil {
			return false, err
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return false, err
	}
	old := ""
	if _, err := os.Stat(dir); err == nil {
		old = tmp + ".old"
		if err := os.Rename(dir, old); err != nil {
			return false, err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		if old != "" {
			_ = os.Rename(old, dir)
		}
		return false, err
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return true, nil
}

func current(dir string) (map[string][]byte, error) {
	have := map[string][]byte{}
	for _, o := range owned {
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

func same(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for p, v := range a {
		if w, ok := b[p]; !ok || !bytes.Equal(v, w) {
			return false
		}
	}
	return true
}

// PluginID is the install id Claude Code uses.
func PluginID() string { return Name + "@" + Marketplace }
