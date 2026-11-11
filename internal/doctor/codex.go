package doctor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/stats"
)

// Codex's checks (block 8, https://github.com/Barben360/backsight/issues/283;
// decision 0085 D3, D5, D9, D10). They run only once client install wired
// Codex (its state entry): a machine without Codex reads no line of them.

// CodexProbes are what the Codex checks ask outside backsight's files.
type CodexProbes struct {
	// CodexRun runs the codex command (nil: the checks that need it are
	// skipped).
	CodexRun harness.Runner
	// CodexTools are the annotations of the memory tools the server serves
	// to Codex, by tool (nil: not checked).
	CodexTools func(ctx context.Context) (map[string]map[string]any, error)
}

// Heartbeat margins: a session younger than heartbeatGrace is not judged
// yet (Codex runs SessionStart at the first prompt, not when the thread
// starts); a SessionStart up to heartbeatSlack before the thread's start
// still counts as its own (the two clocks are Codex's and backsight's).
const (
	heartbeatGrace = 10 * time.Minute
	heartbeatSlack = time.Minute
)

// codexChecks are Codex's checks, nil when Codex is not wired here.
func codexChecks(ctx context.Context, env Env, now time.Time) []Check {
	st, err := state.Load(env.Dirs.StateFile())
	if err != nil {
		return nil
	}
	entry := st.ClientOf(harnessid.Codex)
	if entry == nil || entry.PluginVersion == "" {
		return nil
	}
	codex := harness.Codex{}
	henv := harness.Env{Home: env.Home, Profile: entry.Profile, Run: env.CodexRun}
	dir := codex.Locate(henv).Dir
	shim := filepath.Join(env.Dirs.Data, "bin", plugin.ShimName(runtime.GOOS))

	version := checkCodexVersion(ctx, codex, henv, dir, env.CodexRun != nil)
	out := []Check{version}
	usable := version.Status == OK || version.Status == Error // an older codex still answers
	skip := func(id string) Check {
		why := "the codex command is not usable"
		if env.CodexRun == nil {
			why = "the codex command is not asked here"
		}
		return Check{ID: id, Status: Info, Summary: "skipped: " + why}
	}
	if usable {
		out = append(out, checkCodexPlugins(ctx, codex, henv))
	} else {
		out = append(out, skip("codex-plugins"))
	}
	out = append(out, checkCodexDrift(codex, henv, entry, st.ShimTarget, env.Dirs.Data, shim))
	var feats map[string]bool
	featsErr := errors.New("not asked")
	if usable {
		feats, featsErr = codex.Features(ctx, henv)
	}
	run, ran := state.LastHookRun(env.Dirs, harnessid.Codex)
	out = append(out, checkCodexHooks(codex, henv, entry.WiredAt, run, ran, feats, featsErr, now))
	if env.CodexTools != nil {
		out = append(out, checkCodexTools(ctx, env.CodexTools))
	}
	out = append(out, checkCodexRollouts(env.Dirs.TelemetryDir(), now))
	if usable {
		out = append(out, checkCodexMemories(feats, featsErr), checkCodexName(ctx, codex, henv, shim))
	} else {
		out = append(out, skip("codex-memories"), skip("codex-mcp-name"))
	}
	out = append(out, checkCodexShim(shim, entry.ShimSHA256))
	return out
}

// checkCodexVersion: the codex command answers with a version at least
// the oldest one backsight was verified on (0085 D9).
func checkCodexVersion(ctx context.Context, codex harness.Codex, env harness.Env, dir string, asked bool) Check {
	c := Check{ID: "codex"}
	if !asked {
		c.Status, c.Summary = Info, "Codex wired in "+dir+"; the codex command is not asked here"
		return c
	}
	v, err := codex.Detect(ctx, env)
	switch {
	case errors.Is(err, harness.ErrMissing):
		c.Status, c.Summary = Warn, "backsight is wired into Codex ("+dir+") but the codex command is not found"
		c.Fix = "install Codex (npm install -g @openai/codex) or put codex on the PATH"
	case err != nil:
		c.Status, c.Summary, c.Detail = Warn, "cannot run codex --version", err.Error()
	case !versionAtLeast(v, codex.MinVersion()):
		c.Status, c.Summary = Error, fmt.Sprintf("Codex %s is older than %s, the oldest version backsight was verified on", v, codex.MinVersion())
		c.Detail = "Codex " + dir
		c.Fix = "npm install -g @openai/codex@latest"
	default:
		c.Status, c.Summary = OK, "Codex "+v+" in "+dir
	}
	return c
}

// versionAtLeast compares dotted versions number by number.
func versionAtLeast(v, minimum string) bool {
	a, b := strings.Split(v, "."), strings.Split(minimum, ".")
	for i := range b {
		var x, y int
		if i < len(a) {
			x, _ = strconv.Atoi(strings.SplitN(a[i], "-", 2)[0])
		}
		y, _ = strconv.Atoi(b[i])
		if x != y {
			return x > y
		}
	}
	return true
}

// checkCodexPlugins: both of backsight's plugins installed and enabled
// (`codex plugin list --json`, 0085 D2).
func checkCodexPlugins(ctx context.Context, codex harness.Codex, env harness.Env) Check {
	c := Check{ID: "codex-plugins"}
	states, err := codex.PluginStates(ctx, env)
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot list Codex's plugins", err.Error()
		return c
	}
	var missing, disabled, versions []string
	for _, id := range harness.CodexPluginIDs() {
		s := states[id]
		switch {
		case !s.Installed:
			missing = append(missing, id)
		case !s.Enabled:
			disabled = append(disabled, id)
		default:
			versions = append(versions, id+" "+s.Version)
		}
	}
	switch {
	case len(missing) > 0:
		c.Status, c.Summary = Error, "backsight's plugins are not installed in Codex: "+strings.Join(missing, ", ")
		c.Detail = "without them Codex has no memory tools or no hooks"
		c.Fix = "backsight client install --codex"
	case len(disabled) > 0:
		c.Status, c.Summary = Warn, "backsight's plugins are disabled in Codex: "+strings.Join(disabled, ", ")
		c.Fix = "backsight client install --codex (adds them again, enabled)"
	default:
		c.Status, c.Summary = OK, "plugins installed and enabled: "+strings.Join(versions, ", ")
	}
	return c
}

// codexUntrustedLine starts the line Codex's drift adds for hooks with no
// trust recorded: the hooks check reports them.
const codexUntrustedLine = "Codex has not trusted "

// checkCodexDrift: the plugins, their files and the shim are what client
// install generated for this binary (SPEC §5 drift); shimTarget is the
// binary state.json records the shim points to.
func checkCodexDrift(codex harness.Codex, env harness.Env, entry *state.Client, shimTarget, data, shim string) Check {
	c := Check{ID: "codex-drift"}
	if _, err := os.Stat(shim); err != nil {
		c.Status, c.Summary = Error, "the shim "+shim+" is missing: Codex's hooks and memory server cannot start"
		c.Fix = "backsight client install --codex"
		return c
	}
	rec := harness.Recorded{Version: entry.PluginVersion, ShimTarget: shimTarget, Data: data, BinaryVersion: buildinfo.Version}
	var drift []string
	for _, d := range codex.Drift(env, rec, nil) {
		if !strings.HasPrefix(d, codexUntrustedLine) {
			drift = append(drift, d)
		}
	}
	if len(drift) > 0 {
		c.Status, c.Summary, c.Detail = Warn, "Codex wiring drifted", strings.Join(drift, "; ")
		c.Fix = "backsight client install --codex"
		return c
	}
	c.Status, c.Summary = OK, "Codex plugin "+entry.PluginVersion+" matches this binary, in "+harness.CodexMarketplaceDir(data)
	return c
}

// checkCodexHooks: Codex runs backsight's hooks. Every wired handler has a
// trust recorded ([hooks.state."backsight-hooks@backsight:…"].trusted_hash),
// hooks are not turned off, and the heartbeat: the newest Codex session
// started since Codex was wired ran SessionStart (a session it did not run
// on means Codex ran none of backsight's hooks, trust recorded or not: a
// hook changed since its review is not trusted either, 0085 D5).
func checkCodexHooks(codex harness.Codex, env harness.Env, wiredAt time.Time, run state.HookRun, ran bool, feats map[string]bool, featsErr error, now time.Time) Check {
	c := Check{ID: "codex-hooks"}
	total := len(harness.CodexTrustKeys())
	untrusted, err := codex.UntrustedHooks(env)
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot read which of backsight's hooks Codex trusts", err.Error()
		return c
	}
	if featsErr == nil {
		if on, listed := feats["hooks"]; listed && !on {
			c.Status, c.Summary = Error, "Codex's hooks are turned off: no hook of backsight's runs"
			c.Detail = "codex features list shows hooks false"
			c.Fix = "remove hooks = false from [features] in " + codex.Locate(env).Settings + " (or the profile that sets it), or run codex features enable hooks"
			return c
		}
	}
	if len(untrusted) > 0 {
		c.Status = Warn
		c.Summary = fmt.Sprintf("Codex has not trusted %d of backsight's %d hooks: they do not run until you review them", len(untrusted), total)
		c.Detail = "no trust recorded in " + codex.Locate(env).Settings + " for " + strings.Join(untrusted, ", ")
		c.Fix = "open Codex and trust backsight's hooks in the review it shows at start (or /hooks, or the app's Settings)"
		return c
	}
	trusted := fmt.Sprintf("%d hooks trusted", total)
	meta, _, ok := codex.LatestRoot(env, now.Add(-heartbeatGrace))
	if !ok || (!wiredAt.IsZero() && meta.Started.Before(wiredAt)) {
		c.Status, c.Summary = OK, trusted+"; no Codex session to judge them by since Codex was wired"
		return c
	}
	started := meta.Started.UTC().Format("2006-01-02 15:04 UTC")
	if ran && (run.Session == meta.ID || !run.At.Before(meta.Started.Add(-heartbeatSlack))) {
		c.Status, c.Summary = OK, trusted+"; they ran on the latest Codex session (started "+started+")"
		return c
	}
	last := "none recorded"
	if ran {
		last = run.At.UTC().Format("2006-01-02 15:04 UTC")
	}
	c.Status = Error
	c.Summary = "the Codex session started " + started + " ran none of backsight's hooks: no memory there"
	c.Detail = "the last session start backsight recorded from Codex: " + last + "; a hook changed since you trusted it is not trusted any more"
	c.Fix = "open Codex and check /hooks: backsight's hooks must be trusted and on; then start a new session"
	return c
}

// codexHints are the annotations Codex runs each memory tool with, asking
// nothing (0085 D6): the two that write declare they destroy nothing and
// reach nothing outside, the two others are read-only.
var codexHints = map[string]map[string]any{
	"memory_search": {"readOnlyHint": true},
	"memory_read":   {"readOnlyHint": true},
	"memory_store":  {"destructiveHint": false, "openWorldHint": false},
	"memory_credit": {"destructiveHint": false, "openWorldHint": false},
}

// checkCodexTools: the memory server's tools/list carries the hints Codex
// needs to run them without asking, in codex exec too, where it refuses a
// tool without them (0085 D6).
func checkCodexTools(ctx context.Context, tools func(context.Context) (map[string]map[string]any, error)) Check {
	c := Check{ID: "codex-tools"}
	got, err := tools(ctx)
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot list the memory server's tools", err.Error()
		return c
	}
	var wrong []string
	for _, name := range slices.Sorted(maps.Keys(codexHints)) {
		if _, served := got[name]; !served && name == "memory_credit" {
			continue // served only while feedback is on
		}
		for _, k := range slices.Sorted(maps.Keys(codexHints[name])) {
			if v, ok := got[name][k]; !ok || v != codexHints[name][k] {
				wrong = append(wrong, fmt.Sprintf("%s %s=%v", name, k, codexHints[name][k]))
			}
		}
	}
	if len(wrong) > 0 {
		c.Status, c.Summary = Error, "the memory server does not declare what Codex needs to run its tools without asking"
		c.Detail = "missing: " + strings.Join(wrong, ", ") + "; Codex asks before each such call, and codex exec refuses it"
		c.Fix = "backsight update apply (or reinstall backsight), then backsight client install --codex"
		return c
	}
	c.Status, c.Summary = OK, "the memory tools carry the hints Codex runs them with, asking nothing"
	return c
}

// checkCodexRollouts: record types of Codex's rollouts that backsight's
// reader does not know, this week against last (a format change, SPEC
// §14). Read from the telemetry of the flushes of Codex's conversations.
func checkCodexRollouts(dir string, now time.Time) Check {
	c := Check{ID: "codex-rollouts", Status: OK, Summary: "no unknown Codex rollout records this week"}
	week := 7 * 24 * time.Hour
	ev, err := stats.Load(dir, now.Add(-2*week), now)
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "telemetry unreadable", err.Error()
		return c
	}
	this, last := map[string]int{}, map[string]int{}
	for _, e := range ev {
		if e.Event != "transcript_unknown" || stats.ClientOf(e) != harnessid.Codex {
			continue
		}
		m := this
		if e.TS.Before(now.Add(-week)) {
			m = last
		}
		for t, n := range e.Types {
			m[t] += n
		}
	}
	sum := func(m map[string]int) int {
		n := 0
		for _, v := range m {
			n += v
		}
		return n
	}
	n, before := sum(this), sum(last)
	if n == 0 {
		return c
	}
	types := make([]string, 0, len(this))
	fresh := false
	for t := range this {
		types = append(types, t)
		fresh = fresh || last[t] == 0
	}
	sort.Strings(types)
	c.Status = Info
	c.Summary = fmt.Sprintf("%d unknown Codex rollout records this week (%d last week): %s", n, before, strings.Join(types, ", "))
	c.Detail = "they are left out of what is sent; the rest of each conversation is sent"
	if fresh || (n >= 2*before && n >= unknownMinRise) {
		c.Status, c.Fix = Warn, "Codex's rollout format may have changed: check that these types carry no conversation, then report it (backsight report)"
	}
	return c
}

// checkCodexMemories: Codex's own memories off. On, two memories answer
// the same questions (0085 D10): a warning recommending to turn them off.
func checkCodexMemories(feats map[string]bool, err error) Check {
	c := Check{ID: "codex-memories"}
	if err != nil {
		c.Status, c.Summary, c.Detail = Warn, "cannot tell whether Codex's own memories are on", err.Error()
		return c
	}
	on, listed := feats["memories"]
	switch {
	case !listed:
		c.Status, c.Summary = Info, "this Codex lists no memories feature"
	case on:
		c.Status, c.Summary = Warn, "Codex's own memories are on: two memories answer the same questions"
		c.Detail = "backsight's memory already holds what Codex's would; both cost tokens at each session"
		c.Fix = "backsight client codex-memories off (a copy of config.toml is kept; backsight client codex-memories on undoes it)"
	default:
		c.Status, c.Summary = OK, "Codex's own memories are off"
	}
	return c
}

// checkCodexName: no MCP server of the user's has the memory server's
// name, whose tools would be named as backsight's and taken for its own by
// its hooks (0085 D3). The name is the adapter's.
func checkCodexName(ctx context.Context, codex harness.Codex, env harness.Env, shim string) Check {
	c := Check{ID: "codex-mcp-name"}
	name := harness.CodexServer
	clash, err := codex.NameClash(ctx, env, shim)
	switch {
	case err != nil:
		c.Status, c.Summary, c.Detail = Warn, "cannot list Codex's MCP servers", err.Error()
	case clash != "":
		c.Status = Warn
		c.Summary = "Codex has another MCP server named " + name + ": " + clash
		c.Detail = "its tools are named as backsight's (" + codex.ToolPrefix() + "…), and backsight's hooks take its calls for memory calls"
		c.Fix = "rename or remove that server (codex mcp remove " + name + "): the name " + name + " is backsight's"
	default:
		c.Status, c.Summary = OK, "no other MCP server named "+name+" in Codex"
	}
	return c
}

// checkCodexShim: the program behind the shim is the one recorded when
// Codex was wired. Codex trusts backsight's hooks by their declaration,
// not by that program: one replaced since runs at every hook with no new
// review (0085 D5).
func checkCodexShim(shim, recorded string) Check {
	c := Check{ID: "codex-shim"}
	sum, err := fsutil.HashFile(shim)
	switch {
	case err != nil || sum == fsutil.Absent:
		why := "it is missing"
		if err != nil {
			why = err.Error()
		}
		c.Status, c.Summary, c.Detail = Error, "cannot read the program behind the shim "+shim, why
		c.Fix = "backsight client install --codex"
	case recorded == "":
		c.Status, c.Summary = Info, "the program behind the shim was not recorded when Codex was wired"
		c.Fix = "backsight client install --codex records it"
	case sum != recorded:
		c.Status = Warn
		c.Summary = "the program behind the shim " + shim + " is not the one recorded when Codex was wired"
		c.Detail = "sha256 " + short(sum) + ", recorded " + short(recorded) + "; Codex runs it at every hook without a new review"
		c.Fix = "if you replaced it yourself (an update, a build), backsight client install --codex records it; otherwise find out what changed it"
	default:
		c.Status, c.Summary = OK, "the program behind the shim is the one recorded when Codex was wired (sha256 "+short(sum)+")"
	}
	return c
}

// short is a hash's first 12 characters.
func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
