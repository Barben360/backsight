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
	"runtime"
	"sort"
	"strings"

	"github.com/Barben360/backsight/internal/config"
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
	layout = "6"
)

// QuestionTool is Claude Code's tool for asking the user a question, which
// the question gate's PreToolUse hook matches.
const QuestionTool = "AskUserQuestion"

// ToolNames are the four memory tools as Claude Code names them.
var ToolNames = []string{
	"mcp__plugin_" + Name + "_" + Server + "__memory_search",
	"mcp__plugin_" + Name + "_" + Server + "__memory_read",
	"mcp__plugin_" + Name + "_" + Server + "__memory_store",
	"mcp__plugin_" + Name + "_" + Server + "__memory_credit",
}

// event is one hooks.json entry: Claude Code event, matcher, backsight
// event, timeout in seconds (SPEC §6.2; above the supervisor's deadline).
type event struct {
	name, matcher, arg string
	timeout            int
	failure            bool // wired only with the [recall] failure switch
}

// Wiring is what the generated hooks depend on beside the binary and the
// shim: the config switches that wire a hook or leave it out.
type Wiring struct {
	// FailurePitfalls is the [recall] failure switch: off, PostToolUseFailure
	// is not wired, so a failed tool call spawns no hook process (decision
	// 0083 C9). Flipping it takes a client install, which doctor asks for.
	FailurePitfalls bool
}

// WiringOf is the wiring cfg asks for.
func WiringOf(cfg config.Config) Wiring {
	return Wiring{FailurePitfalls: cfg.Recall.FailurePitfalls}
}

// wires says whether the plugin generated for w has e.
func (w Wiring) wires(e event) bool { return !e.failure || w.FailurePitfalls }

var events = []event{
	{"SessionStart", "startup", "session-start", 10, false},
	{"SessionStart", "resume|clear|compact|fork", "session-start", 5, false},
	{"UserPromptSubmit", "", "user-prompt-submit", 25, false},
	{"PreToolUse", QuestionTool, "pre-tool-use", 10, false},
	{"PostToolUse", "mcp__plugin_" + Name + "_" + Server + "__.*", "post-tool-use", 2, false},
	// A failed tool call: the pitfalls memory knows about it. Wired only
	// with the [recall] failure switch (off by default, see docs/configuration.md).
	{"PostToolUseFailure", "Bash|Edit|Write|MultiEdit|WebFetch|mcp__.*", "post-tool-use-failure", 10, true},
	{"SubagentStart", "", "subagent-start", 5, false},
	{"SubagentStop", "", "subagent-stop", 5, false},
	{"Stop", "", "stop", 5, false},
	{"SessionEnd", "", "session-end", 1, false}, // 1.5 s total, not raisable from a plugin (C06)
}

// Hook is one backsight hook command the plugin wires, with Claude Code's
// event name and its timeout in seconds.
type Hook struct {
	Event, Arg string
	Timeout    int
}

// Hooks lists each hook command the plugin generated for w wires, once
// (verify runs every one).
func Hooks(w Wiring) []Hook {
	var out []Hook
	seen := map[string]bool{}
	for _, e := range events {
		if w.wires(e) && !seen[e.arg] {
			seen[e.arg] = true
			out = append(out, Hook{Event: e.name, Arg: e.arg, Timeout: e.timeout})
		}
	}
	return out
}

// Version is the plugin version: the binary's, plus a hash of what the
// generated files depend on (the shim path, the wiring), so any change to
// them is an update Claude Code applies (a moved data directory or a
// flipped [recall] failure switch included).
func Version(binary, shim string, w Wiring) string {
	in := layout + "\n" + shim
	if w.FailurePitfalls {
		in += "\nPostToolUseFailure"
	}
	sum := sha256.Sum256([]byte(in))
	h := hex.EncodeToString(sum[:6])
	if binary == "" || binary == "dev" {
		return "0.0.0-dev+" + h
	}
	if strings.Contains(binary, "+") {
		return binary + "." + h
	}
	return binary + "+" + h
}

// targetOS is the system the plugin is generated for; tests replace it.
var targetOS = runtime.GOOS

// ShimName is the shim's file name: Windows exec form needs a real .exe.
func ShimName(goos string) string {
	if goos == "windows" {
		return "current.exe"
	}
	return "current"
}

// files is the generated tree, relative to the marketplace directory.
// Skills name the shim with forward slashes on Windows (skillShim).
func files(shim, version string, w Wiring) (map[string][]byte, error) {
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
		if !w.wires(e) {
			continue
		}
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
	for p, raw := range SkillFiles(shim) {
		out["plugins/"+Name+"/"+p] = raw
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

// SkillFiles are backsight's skills for shim, by path relative to the
// plugin's root (skills/<name>/SKILL.md).
func SkillFiles(shim string) map[string][]byte {
	out := map[string][]byte{}
	for _, sk := range instructions.Skills(skillShim(targetOS, shim)) {
		out["skills/"+sk.Name+"/SKILL.md"] = []byte(instructions.SkillFile(sk))
	}
	return out
}

// EventName is Claude Code's name of backsight's hook event arg (the
// argument of `backsight hook`), "" for an event the plugin never wires.
func EventName(arg string) string {
	for _, e := range events {
		if e.arg == arg {
			return e.name
		}
	}
	return ""
}

// skillShim is the shim as the skills' commands name it: on Windows with
// forward slashes (C:/Users/…/current.exe), which Claude Code's Bash tool
// (Git Bash) and PowerShell both run. The hooks and the MCP server, exec
// form, keep the native path.
func skillShim(goos, shim string) string {
	if goos == "windows" {
		return strings.ReplaceAll(shim, `\`, "/")
	}
	return shim
}

// owned are the top-level entries of the marketplace directory the
// generator replaces whole.
var owned = []string{".claude-plugin", "plugins"}

// Generate writes the marketplace into dir; changed says whether anything
// differed; w decides the optional hooks. The new tree is written beside
// dir and swapped in, so Claude Code (which reads the plugin from this
// folder) never sees half of it.
func Generate(dir, shim, binaryVersion string, w Wiring) (bool, error) {
	want, err := files(shim, Version(binaryVersion, shim, w), w)
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
