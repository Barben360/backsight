package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/state"
)

const codexFixtures = "../testdata/codex/0.167.0/"

// The recorded root thread (rollouts/root.jsonl), started 2026-11-10
// 20:34:44 UTC, and the subagent it spawned.
const (
	codexRootID     = "01a1c72b-9533-7bb0-9002-52dca295a387"
	codexSubagentID = "01a1c72b-ae08-75b3-a4db-8a06f93c0c83"
)

var (
	codexRootStart = time.Date(2026, 11, 10, 20, 34, 44, 665e6, time.UTC)
	codexNow       = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
)

// fakeCodexCLI answers the codex commands doctor and install run, from
// the recorded outputs where there are some.
type fakeCodexCLI struct {
	version  string // "" the recorded one
	missing  bool
	failOn   string            // a command prefix that fails
	root     string            // the marketplace added
	plugins  map[string]string // installed plugin → version
	disabled map[string]bool
	features string // "" the recorded list
	mcp      string // codex mcp list --json ("" none)
}

func (f *fakeCodexCLI) run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	if f.missing {
		return nil, harness.MissingError{Command: "codex"}
	}
	cmd := strings.Join(args, " ")
	if f.failOn != "" && strings.HasPrefix(cmd, f.failOn) {
		return nil, errors.New("codex " + cmd + ": exit status 1")
	}
	switch {
	case cmd == "--version":
		if f.version != "" {
			return []byte("codex-cli " + f.version + "\n"), nil
		}
		return os.ReadFile(codexFixtures + "config/codex-version.txt")
	case cmd == "features list":
		if f.features != "" {
			return []byte(f.features), nil
		}
		return os.ReadFile(codexFixtures + "config/features.txt")
	case cmd == "mcp list --json":
		if f.mcp == "" {
			return []byte("[]"), nil
		}
		return []byte(f.mcp), nil
	case cmd == "plugin marketplace list --json":
		if f.root == "" {
			return []byte(`{"marketplaces": []}`), nil
		}
		return json.Marshal(map[string]any{"marketplaces": []any{map[string]any{"name": "backsight", "root": f.root,
			"marketplaceSource": map[string]string{"sourceType": "local", "source": f.root}}}})
	case cmd == "plugin list --json":
		var list []any
		for _, id := range slices.Sorted(maps.Keys(f.plugins)) {
			name, _, _ := strings.Cut(id, "@")
			list = append(list, map[string]any{"pluginId": id, "version": f.plugins[id], "installed": true, "enabled": !f.disabled[id],
				"source": map[string]string{"source": "local", "path": filepath.Join(f.root, "plugins", name)}})
		}
		return json.Marshal(map[string]any{"installed": list, "available": []any{}})
	case len(args) == 4 && cmd == "plugin marketplace add "+args[3]:
		f.root = args[3]
		return nil, nil
	case len(args) == 3 && args[0] == "plugin" && args[1] == "add":
		name, _, _ := strings.Cut(args[2], "@")
		var m struct{ Version string }
		raw, err := os.ReadFile(filepath.Join(f.root, "plugins", name, ".claude-plugin", "plugin.json"))
		if err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &m)
		if f.plugins == nil {
			f.plugins = map[string]string{}
		}
		f.plugins[args[2]] = m.Version
		return nil, nil
	}
	return nil, errors.New("unexpected codex " + cmd)
}

// codexFix is a doctor fixture where client install wired Codex: the shim
// and the binary behind it, both plugins generated and added, every hook
// trusted, Codex's state entry recorded.
type codexFix struct {
	*fixture
	cli       *fakeCodexCLI
	codexHome string
	shim      string
}

func newCodexFix(t *testing.T) *codexFix {
	t.Helper()
	f := &codexFix{fixture: newFixture(t), cli: &fakeCodexCLI{}}
	f.codexHome = filepath.Join(f.home, ".codex")
	data := f.env.Dirs.Data
	target := filepath.Join(data, "bin", "1.0.0", plugin.ShimName(runtime.GOOS))
	put(t, target, "the binary", 0o755)
	f.shim = filepath.Join(data, "bin", plugin.ShimName(runtime.GOOS))
	if runtime.GOOS == "windows" {
		put(t, f.shim, "the binary", 0o755) // a copy there
	} else if err := os.Symlink(target, f.shim); err != nil {
		t.Fatal(err)
	}
	henv := harness.Env{Home: f.home, Run: f.cli.run}
	res, err := harness.Codex{}.Install(context.Background(), henv, harness.Integration{Data: data, Shim: f.shim, BinaryVersion: buildinfo.Version}, false)
	if err != nil {
		t.Fatal(err)
	}
	f.trust(harness.CodexTrustKeys()...)
	sum := sha256Hex(t, "the binary")
	if err := state.Save(f.env.Dirs.StateFile(), state.State{SchemaVersion: state.SchemaVersion, ShimTarget: target,
		Clients: map[string]*state.Client{harnessid.Codex: {PluginVersion: res.Version, ShimSHA256: sum, WiredAt: codexRootStart.Add(-time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	f.env.CodexRun = f.cli.run
	f.env.CodexTools = func(context.Context) (map[string]map[string]any, error) { return servedHints(), nil }
	f.env.Now = func() time.Time { return codexNow }
	return f
}

// servedHints are the annotations backsight's server serves (recorded in
// checks/4-approvals-with-hints).
func servedHints() map[string]map[string]any {
	return map[string]map[string]any{
		"memory_search": {"readOnlyHint": true}, "memory_read": {"readOnlyHint": true},
		"memory_store": {"destructiveHint": false, "openWorldHint": false, "readOnlyHint": false}, "memory_credit": {"destructiveHint": false, "openWorldHint": false, "readOnlyHint": false},
	}
}

func sha256Hex(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	put(t, p, content, 0o600)
	h, err := fsutil.HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// trust writes config.toml with a trusted_hash for each key.
func (f *codexFix) trust(keys ...string) {
	var b strings.Builder
	b.WriteString("model = \"fake-model\"\n\n")
	for _, k := range keys {
		b.WriteString("[hooks.state.\"" + k + "\"]\ntrusted_hash = \"sha256:00\"\n\n")
	}
	put(f.t, filepath.Join(f.codexHome, "config.toml"), b.String(), 0o600)
}

// rollout puts a recorded rollout in the Codex home under the name Codex
// gives it.
func (f *codexFix) rollout(fixture, name string) {
	raw, err := os.ReadFile(codexFixtures + "rollouts/" + fixture)
	if err != nil {
		f.t.Fatal(err)
	}
	put(f.t, filepath.Join(f.codexHome, "sessions", "2026", "11", "10", name), string(raw), 0o600)
}

func (f *codexFix) checks() map[string]Check {
	out := map[string]Check{}
	for _, c := range codexChecks(context.Background(), f.env, codexNow) {
		if _, dup := out[c.ID]; dup {
			f.t.Fatalf("%s twice", c.ID)
		}
		out[c.ID] = c
	}
	return out
}

func (f *codexFix) check(id string) Check {
	f.t.Helper()
	c, ok := f.checks()[id]
	if !ok {
		f.t.Fatalf("no %s check", id)
	}
	return c
}

var codexCheckIDs = []string{"codex", "codex-plugins", "codex-drift", "codex-hooks", "codex-tools", "codex-rollouts", "codex-memories", "codex-mcp-name", "codex-shim"}

// Codex's checks come after Claude Code's wiring, in their order, only on
// a machine where Codex is wired; a fully wired Codex passes every one.
func TestCodexChecksOnlyWhereCodexIsWired(t *testing.T) {
	plain := newFixture(t)
	plain.env.CodexRun = (&fakeCodexCLI{}).run
	for _, c := range plain.run().Checks {
		if strings.HasPrefix(c.ID, "codex") {
			t.Errorf("Codex not wired, yet %s: %s", c.ID, c.Summary)
		}
	}
	f := newCodexFix(t)
	var ids []string
	after := false
	for _, c := range f.run().Checks {
		after = after || c.ID == "client"
		if strings.HasPrefix(c.ID, "codex") {
			if !after {
				t.Errorf("%s before the client check", c.ID)
			}
			ids = append(ids, c.ID)
			if c.Status != OK {
				t.Errorf("%s: %s %s (%s)", c.ID, c.Status, c.Summary, c.Detail)
			}
		}
	}
	if !slices.Equal(ids, codexCheckIDs) {
		t.Errorf("checks %v, want %v", ids, codexCheckIDs)
	}
}

// codex: the recorded version passes; an older one fails, naming the
// minimum; no codex command warns.
func TestCodexVersionCheck(t *testing.T) {
	f := newCodexFix(t)
	expect(t, f.check("codex"), OK, "Codex 0.167.0 in "+f.codexHome)
	f.cli.version = "0.166.9"
	expect(t, f.check("codex"), Error, "older than 0.167.0")
	f.cli.version = "0.200.0"
	expect(t, f.check("codex"), OK, "Codex 0.200.0")
	f.cli.version, f.cli.missing = "", true
	c := f.check("codex")
	expect(t, c, Warn, "the codex command is not found")
	if c.Fix == "" {
		t.Error("no fix")
	}
	// The checks that run codex say they were skipped.
	for _, id := range []string{"codex-plugins", "codex-memories", "codex-mcp-name"} {
		expect(t, f.check(id), Info, "skipped")
	}
}

// codex-plugins: both installed and enabled; one disabled warns; one not
// installed fails, as in the recorded list (the main plugin alone).
func TestCodexPluginsCheck(t *testing.T) {
	f := newCodexFix(t)
	expect(t, f.check("codex-plugins"), OK, "backsight-hooks@backsight")
	f.cli.disabled = map[string]bool{"backsight-hooks@backsight": true}
	expect(t, f.check("codex-plugins"), Warn, "disabled in Codex: backsight-hooks@backsight")
	f.cli.disabled = nil
	delete(f.cli.plugins, "backsight-hooks@backsight")
	c := f.check("codex-plugins")
	expect(t, c, Error, "not installed in Codex: backsight-hooks@backsight")
	if c.Fix != "backsight client install --codex" {
		t.Errorf("fix %q", c.Fix)
	}
	f.cli.failOn = "plugin list"
	expect(t, f.check("codex-plugins"), Warn, "cannot list Codex's plugins")
}

// codex-drift: the generated plugins match this binary; a changed file
// warns (and the untrusted hooks are the hooks check's, not this one's); a
// missing shim fails.
func TestCodexDriftCheck(t *testing.T) {
	f := newCodexFix(t)
	f.trust() // nothing trusted: not a drift line
	expect(t, f.check("codex-drift"), OK, "matches this binary")
	mkt := harness.CodexMarketplaceDir(f.env.Dirs.Data)
	put(t, filepath.Join(mkt, "plugins", "backsight-hooks", "hooks", "hooks.json"), "{}", 0o600)
	c := f.check("codex-drift")
	expect(t, c, Warn, "drifted")
	if !strings.Contains(c.Detail, "plugin files") || strings.Contains(c.Detail, "trusted") {
		t.Errorf("detail %q", c.Detail)
	}
	if err := os.Remove(f.shim); err != nil {
		t.Fatal(err)
	}
	expect(t, f.check("codex-drift"), Error, "is missing")
}

// codex-hooks: every handler trusted and the latest session ran
// SessionStart passes; a handler with no trust warns; a session started
// since Codex was wired with no SessionStart fails, as do hooks turned off.
func TestCodexHooksCheck(t *testing.T) {
	f := newCodexFix(t)
	expect(t, f.check("codex-hooks"), OK, "no Codex session to judge them by")
	f.rollout("root.jsonl", "rollout-2026-11-10T20-34-44-"+codexRootID+".jsonl")
	c := f.check("codex-hooks")
	expect(t, c, Error, "started 2026-11-10 20:34 UTC ran none of backsight's hooks")
	if !strings.Contains(c.Detail, "none recorded") || !strings.Contains(c.Fix, "/hooks") {
		t.Errorf("detail %q fix %q", c.Detail, c.Fix)
	}
	// A SessionStart of that session: its hooks ran.
	state.RecordHookRun(f.env.Dirs, harnessid.Codex, codexRootID, codexRootStart.Add(20*time.Second))
	expect(t, f.check("codex-hooks"), OK, "they ran on the latest Codex session")
	// A newer subagent thread runs no SessionStart: not judged.
	f.rollout("subagent.jsonl", "rollout-2026-11-10T20-34-51-"+codexSubagentID+".jsonl")
	expect(t, f.check("codex-hooks"), OK, "they ran on the latest Codex session")
	// A session start older than the session, for another session: failed.
	state.RecordHookRun(f.env.Dirs, harnessid.Codex, "01a1c72b-0000-7000-8000-000000000000", codexRootStart.Add(-time.Hour))
	expect(t, f.check("codex-hooks"), Error, "ran none of backsight's hooks")
	// A session from before Codex was wired is not judged.
	st, _ := state.Load(f.env.Dirs.StateFile())
	st.ClientOf(harnessid.Codex).WiredAt = codexRootStart.Add(time.Minute)
	if err := state.Save(f.env.Dirs.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	expect(t, f.check("codex-hooks"), OK, "no Codex session to judge them by")
	// Wired by an earlier version, with no wiring time: the session that
	// ran no SessionStart may predate the hooks; not judged, never failed.
	st.ClientOf(harnessid.Codex).WiredAt = time.Time{}
	if err := state.Save(f.env.Dirs.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	c = f.check("codex-hooks")
	expect(t, c, Info, "not judged")
	if !strings.Contains(c.Fix, "client install --codex") {
		t.Errorf("fix %q", c.Fix)
	}
	// One handler without its trust.
	keys := harness.CodexTrustKeys()
	f.trust(keys[1:]...)
	c = f.check("codex-hooks")
	expect(t, c, Warn, "Codex has not trusted 1 of backsight's")
	if !strings.Contains(c.Detail, keys[0]) || !strings.Contains(c.Fix, "/hooks") {
		t.Errorf("detail %q fix %q", c.Detail, c.Fix)
	}
	put(t, filepath.Join(f.codexHome, "config.toml"), "[hooks\n", 0o600)
	expect(t, f.check("codex-hooks"), Warn, "cannot read which of backsight's hooks Codex trusts")
	f.trust(keys...)
	f.cli.features = "hooks  stable  false\nmemories  stable  false\n"
	expect(t, f.check("codex-hooks"), Error, "Codex's hooks are turned off")
}

// codex-tools: the hints served pass; a write tool without them fails; a
// server that cannot be listed warns. memory_credit is not served while
// feedback is off: not a failure.
func TestCodexToolsCheck(t *testing.T) {
	f := newCodexFix(t)
	expect(t, f.check("codex-tools"), OK, "carry the hints")
	hints := servedHints()
	delete(hints, "memory_credit")
	f.env.CodexTools = func(context.Context) (map[string]map[string]any, error) { return hints, nil }
	expect(t, f.check("codex-tools"), OK, "carry the hints")
	hints = servedHints()
	hints["memory_store"] = map[string]any{"readOnlyHint": false}
	f.env.CodexTools = func(context.Context) (map[string]map[string]any, error) { return hints, nil }
	c := f.check("codex-tools")
	expect(t, c, Error, "does not declare what Codex needs")
	if !strings.Contains(c.Detail, "memory_store destructiveHint=false") || !strings.Contains(c.Detail, "memory_store openWorldHint=false") {
		t.Errorf("detail %q", c.Detail)
	}
	f.env.CodexTools = func(context.Context) (map[string]map[string]any, error) { return nil, errors.New("closed") }
	expect(t, f.check("codex-tools"), Warn, "cannot list the memory server's tools")
}

// codexUnknown is a flush's count of unknown Codex rollout records.
func codexUnknown(at time.Time, client, types string) string {
	return `{"ts":"` + at.UTC().Format(time.RFC3339Nano) + `","event":"transcript_unknown","client":"` + client + `","types":` + types + "}"
}

// codex-rollouts: none passes; a type seen last week too is information; a
// new type warns. Claude Code's unknown records are not Codex's, and
// Codex's are not the transcripts check's.
func TestCodexRolloutsCheck(t *testing.T) {
	f := newCodexFix(t)
	dir := f.env.Dirs.TelemetryDir()
	tel := filepath.Join(dir, "events-2026-11.jsonl")
	put(t, tel, codexUnknown(codexNow.Add(-time.Hour), "claude-code", `{"odd":3}`)+"\n", 0o600)
	expect(t, f.check("codex-rollouts"), OK, "no unknown Codex rollout records")
	put(t, tel, codexUnknown(codexNow.Add(-time.Hour), "codex", `{"thread_rename":2}`)+"\n"+codexUnknown(codexNow.Add(-8*24*time.Hour), "codex", `{"thread_rename":2}`)+"\n", 0o600)
	expect(t, f.check("codex-rollouts"), Info, "2 unknown Codex rollout records this week (2 last week): thread_rename")
	for _, c := range checkUsage(dir, codexNow, false) {
		if c.ID == "transcripts" && c.Status != OK {
			t.Errorf("Codex's records in the transcripts check: %s", c.Summary)
		}
	}
	put(t, tel, codexUnknown(codexNow.Add(-time.Hour), "codex", `{"new_kind":1}`)+"\n", 0o600)
	c := f.check("codex-rollouts")
	expect(t, c, Warn, "new_kind")
	if !strings.Contains(c.Fix, "rollout format") {
		t.Errorf("fix %q", c.Fix)
	}
}

// codex-memories: off (the recorded list) passes; on warns, recommending
// to turn them off; a features list that cannot be read warns.
func TestCodexMemoriesCheck(t *testing.T) {
	f := newCodexFix(t)
	expect(t, f.check("codex-memories"), OK, "Codex's own memories are off")
	raw, err := os.ReadFile(codexFixtures + "config/features.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.cli.features = strings.Replace(string(raw), "memories                                 stable             false", "memories                                 stable             true", 1)
	if f.cli.features == string(raw) {
		t.Fatal("the recorded memories row moved")
	}
	c := f.check("codex-memories")
	expect(t, c, Warn, "Codex's own memories are on")
	if !strings.HasPrefix(c.Fix, "backsight client codex-memories off") {
		t.Errorf("fix %q", c.Fix)
	}
	f.cli.failOn = "features"
	expect(t, f.check("codex-memories"), Warn, "cannot tell whether Codex's own memories are on")
}

// codex-mcp-name: no other server of the memory server's name passes; one
// in config.toml or in codex mcp list warns, naming it; backsight's own
// (running the shim) is no clash; a list that fails warns.
func TestCodexNameCheck(t *testing.T) {
	f := newCodexFix(t)
	name := harness.CodexServer
	if name != "backsight" {
		t.Fatalf("the Codex memory server is named %q", name)
	}
	expect(t, f.check("codex-mcp-name"), OK, "no other MCP server named "+name)
	f.cli.mcp = `[{"name":"` + name + `","transport":{"type":"stdio","command":"` + filepath.ToSlash(f.shim) + `"}}]`
	if runtime.GOOS != "windows" {
		expect(t, f.check("codex-mcp-name"), OK, "no other MCP server")
	}
	f.cli.mcp = `[{"name":"` + name + `","transport":{"type":"stdio","command":"/usr/bin/other-memory"}}]`
	c := f.check("codex-mcp-name")
	expect(t, c, Warn, "/usr/bin/other-memory")
	if !strings.Contains(c.Fix, "codex mcp remove "+name) {
		t.Errorf("fix %q", c.Fix)
	}
	f.cli.mcp = ""
	raw, _ := os.ReadFile(filepath.Join(f.codexHome, "config.toml"))
	put(t, filepath.Join(f.codexHome, "config.toml"), string(raw)+"[mcp_servers."+name+"]\ncommand = \"x\"\n", 0o600)
	expect(t, f.check("codex-mcp-name"), Warn, "[mcp_servers."+name+"]")
	put(t, filepath.Join(f.codexHome, "config.toml"), string(raw), 0o600)
	f.cli.failOn = "mcp list"
	expect(t, f.check("codex-mcp-name"), Warn, "cannot list Codex's MCP servers")
}

// codex-shim: the program behind the shim has the hash recorded at
// install; another one warns; none recorded is information; no program
// fails.
func TestCodexShimCheck(t *testing.T) {
	f := newCodexFix(t)
	expect(t, f.check("codex-shim"), OK, "is the one recorded")
	target := filepath.Join(f.env.Dirs.Data, "bin", "1.0.0", plugin.ShimName(runtime.GOOS))
	put(t, target, "another binary", 0o755)
	if runtime.GOOS == "windows" {
		put(t, f.shim, "another binary", 0o755)
	}
	c := f.check("codex-shim")
	expect(t, c, Warn, "is not the one recorded")
	if !strings.Contains(c.Detail, "recorded "+sha256Hex(t, "the binary")[:12]) {
		t.Errorf("detail %q", c.Detail)
	}
	st, _ := state.Load(f.env.Dirs.StateFile())
	st.ClientOf(harnessid.Codex).ShimSHA256 = ""
	if err := state.Save(f.env.Dirs.StateFile(), st); err != nil {
		t.Fatal(err)
	}
	expect(t, f.check("codex-shim"), Info, "not recorded")
	if err := os.Remove(f.shim); err != nil {
		t.Fatal(err)
	}
	expect(t, f.check("codex-shim"), Error, "cannot read the program behind the shim")
}

// The headless check reads a Codex rollout's originator and directory, as
// retain's flush does, not a Claude Code transcript's fields (which a
// rollout has none of): a codex exec session is the default skip list's.
func TestHeadlessStartReadsCodexRollouts(t *testing.T) {
	raw, err := os.ReadFile(codexFixtures + "rollouts/root.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "sessions", "2026", "11", "10")
	p := filepath.Join(dir, "rollout-2026-11-10T20-34-44-"+codexRootID+".jsonl")
	put(t, p, string(raw), 0o600)
	if ep, cwd := headlessStart(conversationReaders(), p); ep != "codex-tui" || cwd != "/home/tester/work" {
		t.Errorf("interactive: %q %q", ep, cwd)
	}
	put(t, p, strings.Replace(string(raw), `"originator":"codex-tui"`, `"originator":"codex_exec"`, 1), 0o600)
	ep, _ := headlessStart(conversationReaders(), p)
	if ep != "codex_exec" || !(config.Privacy{SkipEntrypoints: config.DefaultSkipEntrypoints()}).SkipsEntrypoint(ep) {
		t.Errorf("codex exec: %q", ep)
	}
}
