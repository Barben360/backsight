package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/transcript"
)

// ClaudeMinVersion is the oldest Claude Code the plugin was verified on
// (E1): client install and setup refuse an older one.
const ClaudeMinVersion = "2.1.309"

// claudeProfileVar moves Claude Code's directory, and then .claude.json
// into it (claudecode.Locate).
const claudeProfileVar = "CLAUDE_CONFIG_DIR"

// Claude is Claude Code's adapter. It wraps internal/claudecode (its
// files), internal/plugin (the generated plugin: hooks, memory server,
// skills), internal/transcript (its session transcripts) and the claude
// CLI's plugin commands, which it runs through Env.Run.
type Claude struct{}

var _ Adapter = Claude{}

func (Claude) ID() string   { return ClaudeCode }
func (Claude) Name() string { return "Claude Code" }
func (Claude) Tag() string  { return "harness:" + ClaudeCode }

func (Claude) Texts() Texts {
	return Texts{
		ProfileFlag: "Claude Code's directory (default: $CLAUDE_CONFIG_DIR, else ~/.claude)",
		InstallCmd:  "npm install -g @anthropic-ai/claude-code",
		InstallWhy:  "backsight wires itself into Claude Code through its plugin commands (or `claude update` for an old version)",
	}
}

func (Claude) ProfileVar() string { return claudeProfileVar }

// IsDefaultProfile reports that profile ("" included) is ~/.claude, also
// when named another way (a symlink; on Windows another case or an 8.3
// name): the same directory when both exist, else the same cleaned path
// (case-insensitively on Windows).
func (Claude) IsDefaultProfile(home, profile string) bool {
	def := filepath.Join(home, ".claude")
	if profile == "" {
		return true
	}
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

// Locate follows Claude Code for env's profile: "" is ~/.claude, any
// other is the directory as named (as CLAUDE_CONFIG_DIR would be).
func (c Claude) Locate(env Env) Location {
	l := c.Location(env)
	return Location{Dir: l.Dir, Settings: l.Settings, Conversations: l.Projects}
}

// Location is claudecode's full Location of env's profile, for the code
// that reads Claude Code's own files (retention, the reference install).
func (Claude) Location(env Env) claudecode.Location {
	return claudecode.Locate(env.Home, func(k string) string {
		if k == claudeProfileVar {
			return env.Profile
		}
		return ""
	})
}

// ProfileEnv is the environment that points the claude CLI at profile:
// none for the default ~/.claude (CLAUDE_CONFIG_DIR would move
// .claude.json into it); the runner drops the shell's own.
func (c Claude) ProfileEnv(home, profile string) []string {
	if c.IsDefaultProfile(home, profile) {
		return nil
	}
	return []string{claudeProfileVar + "=" + profile}
}

func (c Claude) run(ctx context.Context, env Env, args ...string) ([]byte, error) {
	if env.Run == nil {
		return nil, MissingError{Command: "claude"}
	}
	return env.Run(ctx, c.ProfileEnv(env.Home, env.Profile), args...)
}

// Detect is `claude --version`'s first word ("?" when it prints none).
func (c Claude) Detect(ctx context.Context, env Env) (string, error) {
	v, err := c.run(ctx, env, "--version")
	if err != nil {
		return "", err
	}
	return strings.Fields(string(v) + " ?")[0], nil
}

func (Claude) MinVersion() string { return ClaudeMinVersion }

// Install generates the plugin in a local marketplace (E1) under
// in.Data, then registers marketplace and plugin with Claude Code; a
// marketplace registered at another path (a moved data directory) is
// added again. The permission rule and the recorded state are the
// caller's (they go through the config editor and the state file).
func (c Claude) Install(ctx context.Context, env Env, in Integration, plan bool) (InstallResult, error) {
	var res InstallResult
	mkt := filepath.Join(in.Data, "marketplace")
	// The [recall] failure switch wires PostToolUseFailure or leaves it out.
	wiring := plugin.WiringOf(in.Config)
	res.Version = plugin.Version(in.BinaryVersion, in.Shim, wiring)
	changed := true
	if !plan {
		var err error
		if changed, err = plugin.Generate(mkt, in.Shim, in.BinaryVersion, wiring); err != nil {
			return res, &InstallError{Stage: Prepared, Err: err}
		}
	}
	res.Steps = append(res.Steps, Step{Name: "plugin", Detail: mkt + " (version " + res.Version + ")", Changed: changed})

	known, err := c.pluginList(ctx, env)
	if err != nil {
		return res, &InstallError{Stage: Generated, Err: err}
	}
	marketPath, err := c.marketplacePath(ctx, env)
	if err != nil {
		return res, &InstallError{Stage: Generated, Err: err}
	}
	// A marketplace at another path that still exists is another install's:
	// two installs cannot share a profile (the plugin id is backsight's).
	// A path that is gone is this install's data directory, moved.
	if marketPath != "" && marketPath != "?" && marketPath != mkt {
		if _, err := os.Stat(filepath.Join(marketPath, ".claude-plugin")); err == nil {
			dir := env.Profile
			if dir == "" {
				dir = c.Locate(env).Dir
			}
			return res, &InstallError{Stage: Conflict, Err: errors.New("this Claude Code profile (" + dir + ") is wired to another backsight install, whose marketplace is " +
				marketPath + ": give this install its own profile (CLAUDE_CONFIG_DIR or --config-dir), or uninstall the other one first")}
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
	if enabled, installed := known[plugin.PluginID()]; installed {
		steps = append(steps, []string{"plugin", "update", plugin.PluginID()})
		if !enabled {
			// Disabled in Claude Code (/plugin, or by hand): doctor's
			// client check sends here, and an update does not enable it.
			steps = append(steps, []string{"plugin", "enable", plugin.PluginID(), "--scope", "user"})
		}
	} else {
		steps = append(steps, []string{"plugin", "install", plugin.PluginID(), "--scope", "user"})
	}
	for _, args := range steps {
		if !plan {
			if _, err := c.run(ctx, env, args...); err != nil {
				return res, &InstallError{Stage: Registering, Err: err}
			}
		}
		res.Steps = append(res.Steps, Step{Name: "claude", Detail: strings.Join(args, " "), Changed: true})
	}
	return res, nil
}

// Uninstall uninstalls backsight's plugin and marketplace from env's
// profile, when present.
func (c Claude) Uninstall(ctx context.Context, env Env) error {
	known, err := c.PluginIDs(ctx, env)
	if err != nil {
		return err
	}
	if known[plugin.PluginID()] {
		if _, err := c.run(ctx, env, "plugin", "uninstall", plugin.PluginID(), "--scope", "user"); err != nil {
			return err
		}
	}
	path, err := c.marketplacePath(ctx, env)
	if err != nil {
		return err
	}
	if path != "" {
		if _, err := c.run(ctx, env, "plugin", "marketplace", "remove", plugin.Marketplace); err != nil {
			return err
		}
	}
	return nil
}

// Installed reports whether backsight's plugin or marketplace is
// registered with env's profile (read only).
func (c Claude) Installed(ctx context.Context, env Env) (bool, error) {
	known, err := c.PluginIDs(ctx, env)
	if err != nil {
		return false, err
	}
	path, err := c.marketplacePath(ctx, env)
	if err != nil {
		return false, err
	}
	return known[plugin.PluginID()] || path != "", nil
}

// Drift is plugin.Drift for env's settings: the plugin version for this
// binary and the config's wiring, the permission rule, the shim target.
func (c Claude) Drift(env Env, rec Recorded, cfg *config.Config) []string {
	installed := plugin.Installed{Version: rec.Version, ShimTarget: rec.ShimTarget}
	var w plugin.Wiring
	if cfg != nil {
		w = plugin.WiringOf(*cfg)
	} else {
		w = plugin.InstalledWiring(rec.Data, installed, rec.BinaryVersion) // switch unknown: no drift
	}
	return plugin.Drift(rec.Data, c.Locate(env).Settings, installed, rec.BinaryVersion, w)
}

// marketplacePath is where Claude Code reads backsight's marketplace from,
// "" when it is not registered.
func (c Claude) marketplacePath(ctx context.Context, env Env) (string, error) {
	raw, err := c.run(ctx, env, "plugin", "marketplace", "list", "--json")
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

// PluginIDs is the set of plugin ids `claude plugin list` reports.
func (c Claude) PluginIDs(ctx context.Context, env Env) (map[string]bool, error) {
	list, err := c.pluginList(ctx, env)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for id := range list {
		out[id] = true
	}
	return out, nil
}

// pluginList maps the profile's installed plugins to whether Claude Code
// has them enabled (an entry without "enabled" counts as enabled).
func (c Claude) pluginList(ctx context.Context, env Env) (map[string]bool, error) {
	args := []string{"plugin", "list", "--json"}
	raw, err := c.run(ctx, env, args...)
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
			enabled, set := it["enabled"].(bool)
			out[v] = enabled || !set
		}
	}
	return out, nil
}

// claudeHookInput is the event JSON Claude Code sends (C06 §2, §4).
type claudeHookInput struct {
	SessionID            string          `json:"session_id"`
	PromptID             string          `json:"prompt_id"`
	TranscriptPath       string          `json:"transcript_path"`
	Cwd                  string          `json:"cwd"`
	Source               string          `json:"source"`
	Prompt               string          `json:"prompt"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	AgentID              string          `json:"agent_id"`
	AgentType            string          `json:"agent_type"`
	AgentTranscriptPath  string          `json:"agent_transcript_path"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	Reason               string          `json:"reason"`
	Error                string          `json:"error"`            // PostToolUseFailure
	IsInterrupt          bool            `json:"is_interrupt"`     // PostToolUseFailure
	StopHookActive       bool            `json:"stop_hook_active"` // Stop, SubagentStop: the turn goes on from a Stop hook's block
}

func (Claude) DecodeHook(_ string, raw []byte) (HookInput, error) {
	var in claudeHookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return HookInput{}, err
	}
	// The same fields in the same order: a field added to one and not the
	// other stops the build here.
	return HookInput(in), nil
}

// EncodeHook writes Claude Code's hook answer: a stop's block decision, a
// PreToolUse denial, or text for Claude (additionalContext) and a banner
// for the user (systemMessage), its brand word in color unless NO_COLOR
// is set (no-color.org).
func (Claude) EncodeHook(o HookOutput) []byte {
	switch {
	case o.Block != "":
		raw, _ := json.Marshal(map[string]string{"decision": "block", "reason": o.Block})
		return raw
	case o.Deny != "":
		raw, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
			"hookEventName": plugin.EventName(o.Event), "permissionDecision": "deny", "permissionDecisionReason": o.Deny}})
		return raw
	}
	out := map[string]any{}
	if o.Context != "" {
		out["hookSpecificOutput"] = map[string]string{"hookEventName": plugin.EventName(o.Event), "additionalContext": o.Context}
	}
	if o.Banner != "" {
		banner := o.Banner
		if os.Getenv("NO_COLOR") == "" {
			banner = instructions.Colored(banner)
		}
		out["systemMessage"] = banner
	}
	if len(out) == 0 {
		return nil
	}
	raw, _ := json.Marshal(out)
	return raw
}

// MCPCallSession is CLAUDE_CODE_SESSION_ID, which Claude Code gives the
// MCP servers it starts, one per session: the same for every call, whose
// _meta it ignores.
func (Claude) MCPCallSession(_ map[string]any, getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	return getenv("CLAUDE_CODE_SESSION_ID")
}

// PairsBySession: on Windows, Claude Code's process tree may not make the
// hooks' grandparent the MCP server's parent (decision 0080).
func (Claude) PairsBySession(goos string) bool { return goos == "windows" }

// Conversations are projects/<project>/<session>.jsonl in env's profile;
// subagent transcripts nested deeper belong to their session.
func (c Claude) Conversations(env Env) []string {
	files, _ := filepath.Glob(filepath.Join(c.Locate(env).Conversations, "*", "*.jsonl"))
	sort.Strings(files)
	return files
}

// SessionOf is the transcript's file name without .jsonl: Claude Code
// names a session's transcript after its id.
func (Claude) SessionOf(path string) string { return transcript.SessionOf(path) }

// Size is the transcript file's size: Claude Code never compresses one.
func (Claude) Size(path string) (int64, error) { return transcript.Size(path) }

// Open is the transcript file from byte from on.
func (Claude) Open(path string, from int64) (io.ReadCloser, error) {
	return transcript.Open(path, from)
}

func (Claude) ReadConversation(r io.Reader, from int64) ([]transcript.Record, transcript.Stats, error) {
	return transcript.Read(r, from)
}

func (Claude) ToolPrefix() string {
	return "mcp__plugin_" + plugin.Name + "_" + plugin.Server + "__"
}

func (Claude) ToolNames() []string { return slices.Clone(plugin.ToolNames) }

func (Claude) Skills(shim string) map[string][]byte { return plugin.SkillFiles(shim) }
