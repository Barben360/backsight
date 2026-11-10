package harness

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/plugin"
)

// Every field Claude Code sends reaches the decoded input, by its JSON name.
func TestClaudeDecodesEveryHookField(t *testing.T) {
	raw := `{"session_id":"s1","prompt_id":"p1","transcript_path":"/t.jsonl","cwd":"/w","source":"resume","prompt":"hi",
		"tool_name":"Bash","tool_input":{"command":"ls"},"tool_response":{"ok":true},"agent_id":"a1","agent_type":"Explore",
		"agent_transcript_path":"/a.jsonl","last_assistant_message":"done","reason":"exit","error":"boom","is_interrupt":true,"stop_hook_active":true}`
	got, err := Claude{}.DecodeHook(EventStop, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := HookInput{SessionID: "s1", PromptID: "p1", TranscriptPath: "/t.jsonl", Cwd: "/w", Source: "resume", Prompt: "hi",
		ToolName: "Bash", ToolInput: []byte(`{"command":"ls"}`), ToolResponse: []byte(`{"ok":true}`), AgentID: "a1", AgentType: "Explore",
		AgentTranscriptPath: "/a.jsonl", LastAssistantMessage: "done", Reason: "exit", Error: "boom", IsInterrupt: true, StopHookActive: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded\n%+v\nwant\n%+v", got, want)
	}
	if _, err := (Claude{}).DecodeHook(EventStop, []byte(`{"session_id":`)); err == nil {
		t.Error("a truncated event decoded")
	}
}

// The answers are byte for byte what Claude Code reads.
func TestClaudeEncodesHookAnswers(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	c := Claude{}
	for _, tc := range []struct {
		out  HookOutput
		want string
	}{
		{HookOutput{Event: EventSessionStart, Context: "guide", Banner: "backsight: on"},
			`{"hookSpecificOutput":{"additionalContext":"guide","hookEventName":"SessionStart"},"systemMessage":"backsight: on"}`},
		{HookOutput{Event: EventUserPromptSubmit, Context: "m"}, `{"hookSpecificOutput":{"additionalContext":"m","hookEventName":"UserPromptSubmit"}}`},
		{HookOutput{Event: EventSubagentStart, Banner: "b"}, `{"systemMessage":"b"}`},
		{HookOutput{Event: EventStop, Context: "ignored", Block: "declare"}, `{"decision":"block","reason":"declare"}`},
		{HookOutput{Event: EventPreToolUse, Deny: "no"},
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}`},
	} {
		if got := string(c.EncodeHook(tc.out)); got != tc.want {
			t.Errorf("%+v:\n got %s\nwant %s", tc.out, got, tc.want)
		}
	}
	if got := c.EncodeHook(HookOutput{Event: EventStop}); got != nil {
		t.Errorf("an empty answer printed %s", got)
	}
	t.Setenv("NO_COLOR", "")
	if got := string(c.EncodeHook(HookOutput{Event: EventSessionStart, Banner: "🧠 backsight on"})); !strings.Contains(got, `\u001b[`) {
		t.Errorf("the banner is not colored without NO_COLOR: %s", got)
	}
}

// backsight's event ids are the plugin's hook arguments, each with its
// Claude Code event name.
func TestEventIDsAreThePluginsHookArguments(t *testing.T) {
	ids := []string{EventSessionStart, EventUserPromptSubmit, EventPreToolUse, EventPostToolUse, EventPostToolUseFailure,
		EventSubagentStart, EventSubagentStop, EventStop, EventSessionEnd}
	var args []string
	for _, h := range plugin.Hooks(plugin.Wiring{FailurePitfalls: true}) {
		args = append(args, h.Arg)
		if plugin.EventName(h.Arg) != h.Event {
			t.Errorf("%s: Claude Code event %q, want %q", h.Arg, plugin.EventName(h.Arg), h.Event)
		}
	}
	slices.Sort(ids)
	slices.Sort(args)
	if !slices.Equal(ids, args) {
		t.Errorf("event ids %v, plugin hook arguments %v", ids, args)
	}
}

func TestClaudeIdentityAndTools(t *testing.T) {
	c := Claude{}
	if c.ID() != "claude-code" || c.Tag() != "harness:claude-code" || c.Name() != "Claude Code" {
		t.Errorf("%q %q %q", c.ID(), c.Tag(), c.Name())
	}
	names := c.ToolNames()
	if !slices.Equal(names, plugin.ToolNames) || len(names) != 4 {
		t.Fatalf("tools %v", names)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, c.ToolPrefix()) {
			t.Errorf("%s does not start with %s", n, c.ToolPrefix())
		}
	}
	names[0] = "changed"
	if c.ToolNames()[0] == "changed" {
		t.Error("ToolNames hands out the plugin's own slice")
	}
	// The skills are the plugin's, at the same paths under its root.
	skills := c.Skills("/data/bin/current")
	if _, ok := skills["skills/remember/SKILL.md"]; !ok {
		t.Fatalf("no remember skill in %d files", len(skills))
	}
	shimNamed := false
	for p, raw := range skills {
		if !strings.HasPrefix(p, "skills/") || !strings.HasSuffix(p, "/SKILL.md") || !strings.HasPrefix(string(raw), "---\nname: ") {
			t.Errorf("%s: %.80q", p, raw)
		}
		shimNamed = shimNamed || strings.Contains(string(raw), "/data/bin/current")
	}
	if !shimNamed {
		t.Error("no skill runs the shim it was rendered for")
	}
}

func TestClaudeLocatesAndNamesProfiles(t *testing.T) {
	home := t.TempDir()
	c := Claude{}
	def := c.Locate(Env{Home: home})
	if def.Dir != filepath.Join(home, ".claude") || def.Settings != filepath.Join(home, ".claude", "settings.json") ||
		def.Conversations != filepath.Join(home, ".claude", "projects") {
		t.Errorf("default %+v", def)
	}
	dev := filepath.Join(home, "dev-profile")
	if l := c.Locate(Env{Home: home, Profile: dev}); l.Dir != dev || l.Settings != filepath.Join(dev, "settings.json") {
		t.Errorf("profile %+v", l)
	}
	if st := c.Location(Env{Home: home, Profile: dev}).State; st != filepath.Join(dev, ".claude.json") {
		t.Errorf("state %s", st)
	}
	if env := c.ProfileEnv(home, ""); env != nil {
		t.Errorf("default profile env %v", env)
	}
	if env := c.ProfileEnv(home, filepath.Join(home, ".claude")); env != nil {
		t.Errorf("~/.claude named explicitly: env %v", env)
	}
	if env := c.ProfileEnv(home, dev); !slices.Equal(env, []string{"CLAUDE_CONFIG_DIR=" + dev}) {
		t.Errorf("profile env %v", env)
	}
	if c.ProfileVar() != "CLAUDE_CONFIG_DIR" {
		t.Error(c.ProfileVar())
	}
}

// Conversations are the sessions' transcripts, sorted; a subagent's,
// nested deeper, belongs to its session.
func TestClaudeConversations(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	for _, p := range []string{"b/s2.jsonl", "a/s1.jsonl", "a/s1/subagents/agent-x.jsonl", "a/notes.txt"} {
		p = filepath.Join(projects, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		_ = os.WriteFile(p, nil, 0o600)
	}
	got := Claude{}.Conversations(Env{Home: home})
	want := []string{filepath.Join(projects, "a", "s1.jsonl"), filepath.Join(projects, "b", "s2.jsonl")}
	if !slices.Equal(got, want) {
		t.Errorf("%v", got)
	}
	if got := (Claude{}).Conversations(Env{Home: home, Profile: filepath.Join(home, "other")}); len(got) != 0 {
		t.Errorf("another profile's conversations: %v", got)
	}
}

// A transcript is its own content stream, named after its session.
func TestClaudeTranscriptStream(t *testing.T) {
	p := filepath.Join(t.TempDir(), "8f1c2d.jsonl")
	_ = os.WriteFile(p, []byte("0123456789"), 0o600)
	c := Claude{}
	if s := c.SessionOf(p); s != "8f1c2d" {
		t.Errorf("session %q", s)
	}
	if n, err := c.Size(p); err != nil || n != 10 {
		t.Errorf("size %d %v", n, err)
	}
	r, err := c.Open(p, 4)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(r)
	_ = r.Close()
	if string(raw) != "456789" {
		t.Errorf("from 4: %q", raw)
	}
	if _, err := c.Open(filepath.Join(t.TempDir(), "gone.jsonl"), 0); err == nil {
		t.Error("a missing transcript opened")
	}
}

// Claude Code names a server's session once, in its environment: every
// call is that session's, whatever its _meta says.
func TestClaudeMCPCallSession(t *testing.T) {
	env := func(k string) string {
		if k == "CLAUDE_CODE_SESSION_ID" {
			return "s-env"
		}
		return ""
	}
	c := Claude{}
	if s := c.MCPCallSession(map[string]any{"sessionId": "s-meta"}, env); s != "s-env" {
		t.Errorf("with _meta: %q", s)
	}
	if s := c.MCPCallSession(nil, nil); s != "" {
		t.Errorf("no environment: %q", s)
	}
}

// fakeCLI answers claude's list commands and records the others.
type fakeCLI struct {
	version, plugins, marketplaces, failOn string
	missing                                bool
	calls                                  []string
	envs                                   [][]string
}

func (f *fakeCLI) run(_ context.Context, env []string, args ...string) ([]byte, error) {
	if f.missing {
		return nil, MissingError{Command: "claude"}
	}
	line := strings.Join(args, " ")
	f.calls, f.envs = append(f.calls, line), append(f.envs, env)
	switch {
	case f.failOn != "" && strings.HasPrefix(line, f.failOn):
		return nil, errors.New("failed: " + line)
	case line == "--version":
		return []byte(f.version), nil
	case line == "plugin list --json":
		return []byte(f.plugins), nil
	case line == "plugin marketplace list --json":
		return []byte(f.marketplaces), nil
	}
	return []byte("ok"), nil
}

func TestClaudeDetect(t *testing.T) {
	f := &fakeCLI{version: "2.1.310 (Claude Code)\n"}
	v, err := Claude{}.Detect(context.Background(), Env{Home: t.TempDir(), Run: f.run})
	if err != nil || v != "2.1.310" {
		t.Errorf("%q %v", v, err)
	}
	f.version = ""
	if v, _ := (Claude{}).Detect(context.Background(), Env{Home: t.TempDir(), Run: f.run}); v != "?" {
		t.Errorf("no output: %q", v)
	}
	f.missing = true
	if _, err := (Claude{}).Detect(context.Background(), Env{Home: t.TempDir(), Run: f.run}); !errors.Is(err, ErrMissing) {
		t.Errorf("missing: %v", err)
	}
	if _, err := (Claude{}).Detect(context.Background(), Env{Home: t.TempDir()}); !errors.Is(err, ErrMissing) {
		t.Errorf("no runner: %v", err)
	}
	if (Claude{}).MinVersion() != ClaudeMinVersion {
		t.Error(Claude{}.MinVersion())
	}
}

func integration(t *testing.T) Integration {
	data := t.TempDir()
	return Integration{Data: data, Shim: filepath.Join(data, "bin", "current"), BinaryVersion: "1.2.3", Config: config.Config{}}
}

// A fresh profile: the plugin generated, the marketplace added, the plugin
// installed, all in the profile Env names.
func TestClaudeInstallsInAFreshProfile(t *testing.T) {
	home := t.TempDir()
	dev := filepath.Join(home, "dev")
	in := integration(t)
	f := &fakeCLI{plugins: "[]", marketplaces: "[]"}
	res, err := Claude{}.Install(context.Background(), Env{Home: home, Profile: dev, Run: f.run}, in, false)
	if err != nil {
		t.Fatal(err)
	}
	mkt := filepath.Join(in.Data, "marketplace")
	if _, err := os.Stat(filepath.Join(mkt, ".claude-plugin", "marketplace.json")); err != nil {
		t.Errorf("plugin not generated: %v", err)
	}
	var steps []string
	for _, s := range res.Steps {
		steps = append(steps, s.Name+": "+s.Detail)
	}
	want := []string{"plugin: " + mkt + " (version " + res.Version + ")", "claude: plugin marketplace add " + mkt, "claude: plugin install backsight@backsight --scope user"}
	if !slices.Equal(steps, want) {
		t.Errorf("steps\n%v\nwant\n%v", steps, want)
	}
	if res.Version != plugin.Version("1.2.3", in.Shim, plugin.Wiring{}) {
		t.Errorf("version %s", res.Version)
	}
	for i, env := range f.envs {
		if !slices.Equal(env, []string{"CLAUDE_CONFIG_DIR=" + dev}) {
			t.Errorf("%s ran with %v", f.calls[i], env)
		}
	}
	// Again: nothing to generate, the registration updated.
	f.calls, f.plugins, f.marketplaces = nil, `[{"id":"backsight@backsight","enabled":true}]`, `[{"name":"backsight","path":"`+jsonPath(mkt)+`"}]`
	res, err = Claude{}.Install(context.Background(), Env{Home: home, Profile: dev, Run: f.run}, in, false)
	if err != nil || res.Steps[0].Changed || !slices.Contains(f.calls, "plugin update backsight@backsight") || slices.Contains(f.calls, "plugin install backsight@backsight --scope user") {
		t.Errorf("%v %+v %v", err, res.Steps, f.calls)
	}
}

func jsonPath(p string) string { return strings.ReplaceAll(p, `\`, `\\`) }

// A plan writes nothing and runs only the list commands.
func TestClaudeInstallPlan(t *testing.T) {
	in := integration(t)
	f := &fakeCLI{plugins: "[]", marketplaces: "[]"}
	res, err := Claude{}.Install(context.Background(), Env{Home: t.TempDir(), Run: f.run}, in, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(in.Data, "marketplace")); err == nil {
		t.Error("a plan generated the plugin")
	}
	if !slices.Equal(f.calls, []string{"plugin list --json", "plugin marketplace list --json"}) || len(res.Steps) != 3 || !res.Steps[0].Changed {
		t.Errorf("%v %+v", f.calls, res.Steps)
	}
}

// Each failure says how far the install went.
func TestClaudeInstallFailureStages(t *testing.T) {
	home := t.TempDir()
	other := filepath.Join(t.TempDir(), "marketplace")
	_ = os.MkdirAll(filepath.Join(other, ".claude-plugin"), 0o700)
	for _, tc := range []struct {
		name  string
		f     fakeCLI
		stage Stage
		steps int
	}{
		{"list fails", fakeCLI{failOn: "plugin list", marketplaces: "[]"}, Generated, 1},
		{"marketplace list unreadable", fakeCLI{plugins: "[]", marketplaces: "not json"}, Generated, 1},
		{"another install's marketplace", fakeCLI{plugins: "[]", marketplaces: `[{"name":"backsight","path":"` + jsonPath(other) + `"}]`}, Conflict, 1},
		{"install fails", fakeCLI{plugins: "[]", marketplaces: "[]", failOn: "plugin install"}, Registering, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			res, err := Claude{}.Install(context.Background(), Env{Home: home, Run: f.run}, integration(t), false)
			var ie *InstallError
			if !errors.As(err, &ie) || ie.Stage != tc.stage || len(res.Steps) != tc.steps {
				t.Fatalf("%v (%T) steps %+v", err, err, res.Steps)
			}
			if tc.stage == Conflict && (!strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), filepath.Join(home, ".claude"))) {
				t.Errorf("conflict text: %s", err)
			}
		})
	}
	// Nothing written yet: the data directory is a file.
	in := integration(t)
	_ = os.WriteFile(filepath.Join(in.Data, "marketplace"), nil, 0o600)
	f := &fakeCLI{plugins: "[]", marketplaces: "[]"}
	_, err := Claude{}.Install(context.Background(), Env{Home: home, Run: f.run}, Integration{Data: filepath.Join(in.Data, "marketplace"), Shim: in.Shim}, false)
	var ie *InstallError
	if !errors.As(err, &ie) || ie.Stage != Prepared || len(f.calls) != 0 {
		t.Errorf("%v %v", err, f.calls)
	}
}

func TestClaudeUninstallAndInstalled(t *testing.T) {
	home := t.TempDir()
	f := &fakeCLI{plugins: `[{"id":"backsight@backsight"}]`, marketplaces: `[{"name":"backsight","path":"/m"}]`}
	env := Env{Home: home, Run: f.run}
	if there, err := (Claude{}).Installed(context.Background(), env); err != nil || !there {
		t.Errorf("installed %v %v", there, err)
	}
	if len(f.calls) != 2 {
		t.Errorf("Installed ran more than the list commands: %v", f.calls)
	}
	if err := (Claude{}).Uninstall(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "plugin uninstall backsight@backsight --scope user") || !slices.Contains(f.calls, "plugin marketplace remove backsight") {
		t.Errorf("%v", f.calls)
	}
	f.calls, f.plugins, f.marketplaces = nil, "[]", "[]"
	if there, err := (Claude{}).Installed(context.Background(), env); err != nil || there {
		t.Errorf("installed %v %v", there, err)
	}
	f.calls = nil
	if err := (Claude{}).Uninstall(context.Background(), env); err != nil || !slices.Equal(f.calls, []string{"plugin list --json", "plugin marketplace list --json"}) {
		t.Errorf("%v %v", err, f.calls)
	}
	// Only the marketplace left: still installed, and removed.
	f.calls, f.marketplaces = nil, `[{"name":"backsight","path":"/m"}]`
	if there, _ := (Claude{}).Installed(context.Background(), env); !there {
		t.Error("a marketplace alone is not reported")
	}
	f.missing = true
	if err := (Claude{}).Uninstall(context.Background(), env); !errors.Is(err, ErrMissing) {
		t.Errorf("missing: %v", err)
	}
}

// Drift reads env's settings and, without a config, accepts the wiring
// installed.
func TestClaudeDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shim is a copy there; plugin's own tests cover it")
	}
	home, data := t.TempDir(), t.TempDir()
	target := filepath.Join(data, "bin", "v1", "backsight")
	_ = os.MkdirAll(filepath.Dir(target), 0o755)
	_ = os.WriteFile(target, []byte("bin"), 0o755)
	shim := filepath.Join(data, "bin", plugin.ShimName(runtime.GOOS))
	if err := os.Symlink(target, shim); err != nil {
		t.Skip("no symlinks")
	}
	dev := filepath.Join(home, "dev")
	_ = os.MkdirAll(dev, 0o700)
	allow := `{"permissions":{"allow":["` + strings.Join(plugin.ToolNames, `","`) + `"]}}`
	_ = os.WriteFile(filepath.Join(dev, "settings.json"), []byte(allow), 0o600)
	on := plugin.Wiring{FailurePitfalls: true}
	rec := Recorded{Version: plugin.Version("1.0.0", shim, on), ShimTarget: target, Data: data, BinaryVersion: "1.0.0"}
	c := Claude{}
	if d := c.Drift(Env{Home: home, Profile: dev}, rec, nil); len(d) != 0 {
		t.Errorf("no config, the installed wiring: %v", d)
	}
	cfg := config.Config{}
	if d := c.Drift(Env{Home: home, Profile: dev}, rec, &cfg); len(d) != 1 || !strings.Contains(d[0], "PostToolUseFailure") {
		t.Errorf("switch off in the config: %v", d)
	}
	if d := c.Drift(Env{Home: home}, rec, nil); len(d) != 1 || !strings.Contains(d[0], filepath.Join(home, ".claude", "settings.json")) {
		t.Errorf("the default profile has no rule: %v", d)
	}
}
