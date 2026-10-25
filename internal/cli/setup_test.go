package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// fakeSteps stands in for the commands setup chains.
type fakeSteps struct {
	calls [][]string
	fail  map[string]*output.Error // by command ("client install")
	real  map[string]bool          // commands run for real (config-editor runs to undo)
	home  string
}

func (f *fakeSteps) run(_ context.Context, args ...string) subResult {
	f.calls = append(f.calls, args)
	key := strings.Join(args[:min(2, len(args))], " ")
	if args[0] == "verify" {
		key = "verify"
	}
	if e := f.fail[key]; e != nil {
		return subResult{Err: e}
	}
	if f.real[key] {
		return runSubcommand(context.Background(), args...)
	}
	var data any
	switch key {
	case "server install":
		data = installResult{URL: "http://127.0.0.1:8888", UI: "http://127.0.0.1:9999", Version: "1.1.0"}
	case "client install":
		// a real install edits settings.json through the config editor
		run := filepath.Join(pathsOf(f.home).PreImagesDir(), "20261025T120000Z")
		_ = os.MkdirAll(run, 0o700)
		_ = os.WriteFile(filepath.Join(run, "manifest.json"), []byte(`{"ts":"20261025T120000Z","files":[]}`), 0o600)
		_ = state.Update(pathsOf(f.home), "client install", func(s *state.State) error { s.PluginVersion = "1.0.0"; return nil })
		data = clientInstallResult{Profile: filepath.Join(f.home, ".claude"), Steps: []installStep{{Name: "plugin", Status: "done"}}}
	case "client retention":
		data = retentionResult{Target: 3650}
	case "jobs install":
		_ = state.Update(pathsOf(f.home), "jobs install", func(s *state.State) error { s.Scheduler = &state.Scheduler{Kind: "systemd"}; return nil })
		data = jobsInstallResult{Kind: "systemd"}
	case "verify":
		data = verifyResult{Checks: []verifyCheck{{ID: "mcp", Status: "ok"}}}
	case "retain backfill":
		data = backfillResult{Total: 2, Queued: true}
	}
	raw, _ := json.Marshal(data)
	return subResult{Data: raw}
}

func (f *fakeSteps) commands() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

// setupMachine isolates a home with healthy probes and fake steps.
func setupMachine(t *testing.T) (string, *fakeSteps) {
	t.Helper()
	home := isolate(t)
	withSys(t, "linux") // the reference count asks systemctl about the retro timer
	withClaude(t)       // the client step asks claude which plugins a profile has
	f := &fakeSteps{fail: map[string]*output.Error{}, real: map[string]bool{"client retire": true}, home: home}
	oldSub, oldProbes, oldUp := subcommand, preflightProbes, serverAnswers
	subcommand = f.run
	serverAnswers = func(context.Context, config.Config) bool { return false } // never a real server
	preflightProbes = func(context.Context, setupEnv, map[string]string) setup.Probes {
		return setup.Probes{GOOS: "linux", GOARCH: "amd64", APIPort: 8888, UIPort: 9999, MinClaude: minClaude, AtLeast: atLeast,
			Docker:    func(context.Context) (string, error) { return "28.1.1", nil },
			Compose:   func(context.Context) (string, error) { return "2.39.1", nil },
			PortFree:  func(int) bool { return true },
			FreeBytes: func() (uint64, error) { return 50 << 30, nil },
			Claude:    func(context.Context) (string, error) { return "2.1.400", nil }}
	}
	t.Cleanup(func() { subcommand, preflightProbes, serverAnswers = oldSub, oldProbes, oldUp })
	return home, f
}

type setupEnvelope struct {
	OK    bool          `json:"ok"`
	Data  setupResult   `json:"data"`
	Error *output.Error `json:"error"`
}

func parseSetup(t *testing.T, out string) setupEnvelope {
	t.Helper()
	var env setupEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not one envelope: %v\n%s", err, out)
	}
	return env
}

func withLLMKey(t *testing.T, home string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(secretsFile(home)), 0o700)
	if err := config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error { return s.Set("llm_api_key", "fakellmkey") }); err != nil {
		t.Fatal(err)
	}
}

func TestSetupQuestionsListWithoutTouchingAnything(t *testing.T) {
	home, f := setupMachine(t)
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false) // the backfill estimate reads it
	code, out, _ := run(t, "setup", "--questions", "--json")
	env := parseSetup(t, out)
	if code != output.OK || len(env.Data.Questions) == 0 || env.Data.Questions[0].ID != "provider" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, `"asked": true`) || !strings.Contains(out, `"recommended": "deepseek"`) {
		t.Errorf("the schema lacks asked/recommended:\n%s", out)
	}
	if _, err := os.Stat(pathsOf(home).ConfigFile()); err == nil || len(f.calls) > 0 {
		t.Error("--questions wrote a config or ran a step")
	}
	if _, err := os.Stat(pathsOf(home).State); err == nil {
		t.Error("--questions wrote into the state directory")
	}
	if !strings.Contains(out, `"id": "backfill"`) {
		t.Errorf("no backfill question for an existing session:\n%s", out)
	}
}

func TestSetupWithoutRequiredAnswersNeedsTheUser(t *testing.T) {
	home, f := setupMachine(t)
	code, out, _ := run(t, "setup", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.NeedsUser || env.Error == nil || !strings.Contains(env.Error.Message, "provider, consent") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, `"questions"`) || !strings.Contains(out, `"id": "provider"`) {
		t.Errorf("needs_user lacks the question schema:\n%s", out)
	}
	if _, err := os.Stat(pathsOf(home).ConfigFile()); err == nil || len(f.calls) > 0 {
		t.Error("setup acted before its answers")
	}
}

func TestSetupRefusesSecretsAsAnswers(t *testing.T) {
	setupMachine(t)
	code, _, errOut := run(t, "setup", "--answer", "llm_api_key=sk-x", "--non-interactive")
	if code != output.Usage || !strings.Contains(errOut, "secret set llm_api_key") || strings.Contains(errOut, "sk-x") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestSetupDeclinedChangesNothing(t *testing.T) {
	home, f := setupMachine(t)
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=no", "--non-interactive", "--json")
	if env := parseSetup(t, out); code != output.OK || !env.Data.Declined {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(pathsOf(home).ConfigFile()); err == nil || len(f.calls) > 0 {
		t.Error("a declined setup changed something")
	}
}

func TestSetupPreflightFailureStopsBeforeAnyChange(t *testing.T) {
	home, f := setupMachine(t)
	inner := preflightProbes
	preflightProbes = func(ctx context.Context, s setupEnv, x map[string]string) setup.Probes {
		p := inner(ctx, s, x)
		p.Docker = func(context.Context) (string, error) {
			return "", errors.New("permission denied on /var/run/docker.sock")
		}
		return p
	}
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.NeedsUser || env.Error == nil || len(env.Error.Actions) != 1 || env.Error.Actions[0].Kind != "relogin" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(pathsOf(home).ConfigFile()); err == nil || len(f.calls) > 0 {
		t.Error("setup acted despite a failed pre-flight")
	}
}

func TestSetupStopsAtTheMissingKeyThenResumes(t *testing.T) {
	home, f := setupMachine(t)
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	oldSession(t, home, "s2", time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC), false)
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--answer", "backups=off", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.NeedsUser || env.Error == nil || len(env.Error.Secrets) != 1 || env.Error.Secrets[0] != "llm_api_key" || env.Error.Resume != setupResume {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(pathsOf(home).ConfigFile()); err == nil || len(f.calls) > 0 {
		t.Fatalf("setup wrote config or ran steps without the key: %v", f.commands())
	}

	withLLMKey(t, home)
	code, out, errOut := run(t, "setup", "--resume", "--non-interactive", "--json")
	env = parseSetup(t, out)
	if code != output.OK {
		t.Fatalf("resume: exit %d\n%s\n%s", code, out, errOut)
	}
	want := []string{"server install", "client install", "client retention", "jobs install", "verify", "retain backfill --queue --projects-dir " + filepath.Join(home, ".claude", "projects")}
	got := f.commands()
	if len(got) != len(want) {
		t.Fatalf("steps %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d: %q, want %q", i, got[i], want[i])
		}
	}
	if !env.Data.RestartRequired || env.Data.TryFirst == "" {
		t.Errorf("summary: %+v", env.Data)
	}
	rec, _ := setup.Load(pathsOf(home).SetupFile())
	if rec == nil || !rec.Finished || !rec.Undo.CreatedConfig || len(rec.Undo.ClientProfiles) != 1 || rec.Undo.ClientProfiles[0] != filepath.Join(home, ".claude") ||
		!rec.Undo.Scheduler || !rec.Undo.ServerStarted ||
		len(rec.Undo.ConfigRuns) != 1 || rec.Undo.ConfigRuns[0] != "20261025T120000Z" {
		t.Errorf("record: %+v", rec)
	}
	if rec != nil && rec.Answers["backups"] != "off" {
		t.Errorf("resume lost the saved answers: %v", rec.Answers)
	}
	if raw, _ := os.ReadFile(pathsOf(home).ConfigFile()); !strings.Contains(string(raw), `provider = "deepseek"`) || !strings.Contains(string(raw), "enabled = false") {
		t.Errorf("config not written from the saved answers:\n%s", raw)
	}
	for _, s := range []string{`"event":"progress"`, `"step":"server"`} {
		if !strings.Contains(errOut, s) {
			t.Errorf("no progress line %s on stderr:\n%s", s, errOut)
		}
	}
	if j, _ := os.ReadFile(pathsOf(home).JournalFile()); !strings.Contains(string(j), `"command":"setup","run":"`+rec.Run+`","status":"done","step":"scheduler"`) {
		t.Errorf("journal lacks the steps:\n%s", j)
	}
	if strings.Contains(out+errOut, "fakellmkey") {
		t.Error("a secret reached the output")
	}

	// A second resume of a finished run only verifies again.
	f.calls = nil
	if code, _, _ := run(t, "setup", "--resume", "--non-interactive"); code != output.OK || strings.Join(f.commands(), ",") != "verify" {
		t.Errorf("resume of a finished run: exit %d, steps %v", code, f.commands())
	}
}

func TestSetupFailureAfterChangesIsPartialAndResumable(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	f.fail["jobs install"] = &output.Error{Code: output.Blocked, Message: "systemctl failed"}
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.Partial || env.Error == nil || env.Error.Resume != setupResume || !strings.Contains(env.Error.Message, "scheduler: systemctl failed") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	delete(f.fail, "jobs install")
	f.calls = nil
	if code, out, _ := run(t, "setup", "--resume", "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("resume: exit %d\n%s", code, out)
	}
	if got := strings.Join(f.commands(), ","); got != "jobs install,verify" {
		t.Errorf("resume redid finished steps: %v", got)
	}
}

func TestSetupNeedsUserFromAStepStaysNeedsUser(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	f.fail["client install"] = &output.Error{Code: output.NeedsUser, Message: "Claude Code is too old",
		Actions: []output.Action{{Kind: "run_as_user", Cmd: "claude update"}}, Resume: "backsight client install"}
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.NeedsUser || env.Error.Resume != setupResume || len(env.Error.Actions) != 1 {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSetupAdoptsAnExistingInstall(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	cfg := pathsOf(home).ConfigFile()
	_ = os.WriteFile(cfg, []byte("schema_version = 1\n[server]\nurl = \"http://127.0.0.1:18888\"\nui_port = 19999\n[client]\nretention_days = 3650\n"), 0o600)
	before, _ := os.ReadFile(cfg)

	code, _, errOut := run(t, "setup", "--answer", "provider=openai", "--answer", "consent=yes", "--non-interactive")
	if code != output.Usage || !strings.Contains(errOut, "llm.provider") {
		t.Fatalf("a contradicting answer must name the config key: exit %d %s", code, errOut)
	}
	code, out, _ := run(t, "setup", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	var cfgStep setup.Step
	for _, st := range env.Data.Steps {
		if st.Name == "config" {
			cfgStep = st
		}
	}
	if code != output.OK || cfgStep.Status != "skipped" || !strings.Contains(cfgStep.Detail, "adopted") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if after, _ := os.ReadFile(cfg); string(after) != string(before) {
		t.Error("adoption rewrote config.toml")
	}
	if rec, _ := setup.Load(pathsOf(home).SetupFile()); rec == nil || rec.Undo.CreatedConfig || rec.Answers["api_port"] != "18888" {
		t.Errorf("record %+v", rec)
	}
	if len(f.calls) == 0 || f.commands()[0] != "server install" {
		t.Errorf("steps %v", f.commands())
	}
}

func TestSetupRetentionAnswerPicksTheFlag(t *testing.T) {
	for answer, want := range map[string]string{"3650": "client retention", "keep": "client retention --keep", "90": "client retention --days 90"} {
		t.Run(answer, func(t *testing.T) {
			home, f := setupMachine(t)
			withLLMKey(t, home)
			code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--answer", "retention="+answer, "--non-interactive", "--json")
			if code != output.OK {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if got := f.commands()[2]; got != want {
				t.Errorf("%q, want %q", got, want)
			}
		})
	}
}

func TestSetupPassesTheClaudeProfile(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	prof := filepath.Join(home, "scratch-profile")
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	_ = os.MkdirAll(filepath.Join(prof, "projects", "-p"), 0o700)
	_ = os.WriteFile(filepath.Join(prof, "projects", "-p", "s2.jsonl"), []byte(`{"type":"user","uuid":"u","sessionId":"s2","cwd":"/p","timestamp":"2026-09-20T09:00:00Z","message":{"role":"user","content":"We pin Go 1.26 here."}}`+"\n"), 0o600)
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--answer", "backfill=yes", "--config-dir", prof, "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, c := range f.commands() {
		switch {
		case strings.HasPrefix(c, "client ") || strings.HasPrefix(c, "verify"):
			if !strings.HasSuffix(c, "--config-dir "+prof) {
				t.Errorf("%q lacks the profile", c)
			}
		case strings.HasPrefix(c, "retain backfill"):
			if !strings.Contains(c, "--projects-dir "+filepath.Join(prof, "projects")) {
				t.Errorf("%q reads another profile's transcripts", c)
			}
		}
	}
	if !strings.Contains(strings.Join(f.commands(), ","), "verify --config-dir") {
		t.Errorf("verify not run for the profile: %v", f.commands())
	}
	if rec, _ := setup.Load(pathsOf(home).SetupFile()); rec == nil || rec.Profile != prof {
		t.Errorf("record profile %v", rec)
	}
}

func TestSetupPlanChangesNothing(t *testing.T) {
	home, f := setupMachine(t)
	code, out, _ := run(t, "setup", "--plan", "--answer", "provider=ollama", "--json")
	env := parseSetup(t, out)
	if code != output.OK || !env.Data.Plan || len(env.Data.Steps) == 0 || env.Data.Steps[2].Status != "planned" || !strings.Contains(env.Data.Steps[2].Detail, "ollama/qwen3:8b") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if len(env.Data.Preflight) == 0 {
		t.Error("the plan lacks the pre-flight")
	}
	if _, err := os.Stat(pathsOf(home).ConfigFile()); err == nil || len(f.calls) > 0 {
		t.Error("--plan changed something")
	}
}

func TestSetupAsksAtATerminal(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	// provider: 3 (openai), model: Enter, base URL: Enter, retention: keep,
	// backups: 2 (off), backfill: no, then the go.
	withStdin(t, "3\n\n\nkeep\n2\nno\ny\n")
	stdinIsTerminal = func() bool { return true }
	oldOut := stdoutIsTerminal
	stdoutIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdoutIsTerminal = oldOut })
	code, out, errOut := run(t, "setup")
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	raw, _ := os.ReadFile(pathsOf(home).ConfigFile())
	if !strings.Contains(string(raw), `provider = "openai"`) || !strings.Contains(string(raw), `model = "gpt-5.4-mini"`) || !strings.Contains(string(raw), "enabled = false") {
		t.Errorf("answers not applied:\n%s", raw)
	}
	if !strings.Contains(out, "[recommended]") || !strings.Contains(out, "setup will:") || !strings.Contains(out, "backsight is set up") {
		t.Errorf("prompts or summary missing:\n%s", out)
	}
	for _, c := range f.commands() {
		if strings.HasPrefix(c, "retain backfill") {
			t.Errorf("backfill queued after a no: %v", f.commands())
		}
	}
}

func TestSetupAtATerminalDeclines(t *testing.T) {
	home, f := setupMachine(t)
	withStdin(t, "1\n\n\n\nn\n")
	stdinIsTerminal = func() bool { return true }
	oldOut := stdoutIsTerminal
	stdoutIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdoutIsTerminal = oldOut })
	if code, out, _ := run(t, "setup"); code != output.OK || !strings.Contains(out, "nothing changed") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(pathsOf(home).ConfigFile()); err == nil || len(f.calls) > 0 {
		t.Error("declining at the prompt changed something")
	}
}

func TestParseAnswersFile(t *testing.T) {
	dir := t.TempDir()
	js := filepath.Join(dir, "a.json")
	_ = os.WriteFile(js, []byte(`{"provider":"deepseek","api_port":18888}`), 0o600)
	kv := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(kv, []byte("# answers\nprovider=openai\nbackups = off\n"), 0o600)
	a, err := parseAnswers([]string{"provider=anthropic"}, js)
	if err != nil || a["provider"] != "anthropic" || a["api_port"] != "18888" {
		t.Errorf("json: %v %v (flags must win over the file)", a, err)
	}
	a, err = parseAnswers(nil, kv)
	if err != nil || a["provider"] != "openai" || a["backups"] != "off" {
		t.Errorf("lines: %v %v", a, err)
	}
	if _, err := parseAnswers([]string{"novalue"}, ""); output.CodeOf(err) != output.Usage {
		t.Errorf("malformed answer accepted: %v", err)
	}
}

func TestSetupVerifyFailureNamesTheFailingChecks(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	inner := f.run
	subcommand = func(ctx context.Context, args ...string) subResult {
		if args[0] == "verify" {
			raw, _ := json.Marshal(verifyResult{Failed: 1, Checks: []verifyCheck{{ID: "hook stop", Status: "ok"}, {ID: "round_trip", Status: "fail", Summary: "recall found nothing in the scratch bank"}}})
			return subResult{Data: raw, Err: &output.Error{Code: output.Blocked, Message: "1 check(s) failed"}}
		}
		return inner(ctx, args...)
	}
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.Partial || env.Error == nil || !strings.Contains(env.Error.Message, "verify: round_trip: recall found nothing") || strings.Contains(env.Error.Message, "hook stop") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// claudeSettings decodes settings.json's hooks as event → matcher → commands.
func claudeSettings(t *testing.T, path string) (hooks map[string][]string, allow []string, raw map[string]any) {
	t.Helper()
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("%s broken: %v\n%s", path, err, b)
	}
	hooks = map[string][]string{}
	h, _ := raw["hooks"].(map[string]any)
	for ev, gs := range h {
		for _, g := range gs.([]any) {
			gm := g.(map[string]any)
			for _, hk := range gm["hooks"].([]any) {
				hooks[ev] = append(hooks[ev], fmt.Sprint(gm["matcher"])+" | "+hk.(map[string]any)["command"].(string))
			}
		}
	}
	for ev := range hooks {
		sort.Strings(hooks[ev])
	}
	if p, ok := raw["permissions"].(map[string]any); ok {
		for _, r := range p["allow"].([]any) {
			allow = append(allow, r.(string))
		}
	}
	sort.Strings(allow)
	return hooks, allow, raw
}

// referenceSetup runs a full setup over a reference profile, with the
// client install, retention and retire steps for real (three config runs
// on settings.json, one on ~/.claude.json).
func referenceSetup(t *testing.T) (home, settings, state string, f *fakeSteps, claude *fakeClaude, sys *fakeSys) {
	t.Helper()
	home, f = setupMachine(t)
	withLLMKey(t, home)
	claude = claudeRunner.(*fakeClaude)
	sys = sysRunner.(*fakeSys)
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	settings, state = filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude.json")
	_ = os.WriteFile(settings, []byte(referenceSettings), 0o600)
	_ = os.WriteFile(state, []byte(referenceClaudeJSON), 0o600)
	retroScript(home)
	sys.out = map[string]string{"systemctl --user is-enabled memory-retro.timer": "enabled\n"}
	f.real["client install"], f.real["client retention"] = true, true
	code, out, errOut := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	if code != output.OK {
		t.Fatalf("setup: exit %d\n%s\n%s", code, out, errOut)
	}
	return home, settings, state, f, claude, sys
}

func TestSetupRetiresTheReferenceAfterVerifyAndUndoPutsItBack(t *testing.T) {
	home, settings, state, f, claude, sys := referenceSetup(t)
	before, beforeAllow, _ := claudeSettings(t, writeTemp(t, referenceSettings))
	if cmds := strings.Join(f.commands(), ","); !strings.Contains(cmds, "verify,client retire") {
		t.Errorf("retire must follow verify: %s", cmds)
	}
	if a, _ := os.ReadFile(state); strings.Contains(string(a), "user-memory") {
		t.Error("the reference MCP server is still wired")
	}
	rec, _ := setup.Load(pathsOf(home).SetupFile())
	if rec == nil || len(rec.Undo.Timers) != 1 || len(rec.Undo.ConfigRuns) != 3 {
		t.Fatalf("record: %+v", rec)
	}
	for _, ts := range rec.Undo.ConfigRuns {
		if _, err := os.Stat(filepath.Join(pathsOf(home).PreImagesDir(), ts, "pinned")); err != nil {
			t.Errorf("run %s is not pinned against pruning", ts)
		}
	}

	// Claude Code restarts and rewrites ~/.claude.json; the user edits
	// settings.json. Undo must keep both and still put the reference back.
	cj, _ := os.ReadFile(state)
	cj = []byte(strings.Replace(string(cj), `"numStartups": 12`, `"numStartups": 13, "oauthAccount": {"emailAddress": "u@example.com"}`, 1))
	_ = os.WriteFile(state, cj, 0o600)
	sj, _ := os.ReadFile(settings)
	sj = []byte(strings.Replace(string(sj), `"enabledPlugins"`, `"theme": "dark", "enabledPlugins"`, 1))
	_ = os.WriteFile(settings, sj, 0o600)

	f.calls = nil
	if code, out, _ := run(t, "setup", "--undo", "--plan"); code != output.OK || !strings.Contains(out, "planned") || len(f.calls) > 0 {
		t.Fatalf("undo plan: exit %d, steps %v\n%s", code, f.commands(), out)
	}
	if a, _ := os.ReadFile(state); strings.Contains(string(a), "user-memory") {
		t.Error("--undo --plan restored a file")
	}

	claude.plugins = `[{"id":"` + plugin.PluginID() + `"}]`
	claude.marketplaces = `[{"name":"` + plugin.Marketplace + `","path":"/x"}]`
	code, out, errOut := run(t, "setup", "--undo", "--json")
	if code != output.OK {
		t.Fatalf("undo: exit %d\n%s\n%s", code, out, errOut)
	}
	if strings.Contains(out+errOut, "HINDSIGHT_API_TOKEN") {
		t.Error("undo printed a restored MCP entry's value")
	}
	after, afterAllow, raw := claudeSettings(t, settings)
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("hooks after undo:\n%v\nwant\n%v", after, before)
	}
	if strings.Join(afterAllow, ",") != strings.Join(beforeAllow, ",") {
		t.Errorf("permissions after undo %v, want %v (backsight's removed, the reference's back)", afterAllow, beforeAllow)
	}
	if raw["theme"] != "dark" || raw["cleanupPeriodDays"] != nil {
		t.Errorf("the user's edit lost or retention left: theme=%v cleanupPeriodDays=%v", raw["theme"], raw["cleanupPeriodDays"])
	}
	var cjAfter struct {
		NumStartups  int                        `json:"numStartups"`
		OAuthAccount map[string]any             `json:"oauthAccount"`
		MCPServers   map[string]json.RawMessage `json:"mcpServers"`
	}
	b, _ := os.ReadFile(state)
	_ = json.Unmarshal(b, &cjAfter)
	if cjAfter.NumStartups != 13 || cjAfter.OAuthAccount == nil {
		t.Errorf("undo dropped what Claude Code wrote since: %s", b)
	}
	if len(cjAfter.MCPServers) != 3 || !strings.Contains(string(cjAfter.MCPServers["hindsight"]), "HINDSIGHT_API_TOKEN") {
		t.Errorf("reference MCP servers not back whole: %s", b)
	}
	if !contains(sys.calls, "systemctl --user enable --now memory-retro.timer") {
		t.Errorf("the retro timer was not started again: %v", sys.calls)
	}
	if got := strings.Join(f.commands(), ","); got != "jobs uninstall,server stop" {
		t.Errorf("undo steps %q", got)
	}
	m := strings.Join(mutating(claude), ",")
	if !strings.Contains(m, "plugin uninstall "+plugin.PluginID()) || !strings.Contains(m, "plugin marketplace remove "+plugin.Marketplace) {
		t.Errorf("plugin not removed: %s", m)
	}
	if _, err := os.Stat(pathsOf(home).ConfigFile()); err != nil {
		t.Error("undo removed config.toml")
	}
	if st, _ := stateLoad(home); st.PluginVersion != "" {
		t.Error("state still says the plugin is installed")
	}
	if code, out, _ := run(t, "setup", "--undo"); code != output.OK || !strings.Contains(out, "changed nothing") {
		t.Errorf("a second undo: exit %d\n%s", code, out)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(p, []byte(content), 0o600)
	return p
}

func TestSetupUndoSkipsALostRunAndUndoesTheRest(t *testing.T) {
	home, _, state, f, _, _ := referenceSetup(t)
	rec, _ := setup.Load(pathsOf(home).SetupFile())
	retire := rec.Undo.ConfigRuns[len(rec.Undo.ConfigRuns)-1]
	_ = os.RemoveAll(filepath.Join(pathsOf(home).PreImagesDir(), retire))
	f.calls = nil
	code, out, _ := run(t, "setup", "--undo", "--json")
	if code != output.Partial || !strings.Contains(out, `"lost"`) || !strings.Contains(out, retire) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := strings.Join(f.commands(), ","); got != "jobs uninstall,server stop" {
		t.Errorf("a lost run stopped the rest of the undo: %q", got)
	}
	if a, _ := os.ReadFile(state); strings.Contains(string(a), "user-memory") {
		t.Error("a lost run's changes cannot come back, yet the file changed")
	}
	if code, out, _ := run(t, "setup", "--undo"); code != output.OK || !strings.Contains(out, "changed nothing") {
		t.Errorf("the lost run must not block later undos: exit %d\n%s", code, out)
	}
}

func TestSetupUndoUnknownRunNeedsForceButNeverOnClaudeJSON(t *testing.T) {
	home, _ := setupMachine(t)
	d := pathsOf(home)
	settings := filepath.Join(home, ".claude", "settings.json")
	cj := filepath.Join(home, ".claude.json")
	_ = os.MkdirAll(filepath.Dir(settings), 0o700)
	_ = os.WriteFile(settings, []byte(`{"a": 1}`), 0o600)
	_ = os.WriteFile(cj, []byte(`{"b": 1}`), 0o600)
	r := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: "some future step", Now: now}
	set := func(k string, v int) claudecfg.Edit {
		return claudecfg.Edit{Target: settings, Apply: func(doc []byte) ([]byte, []claudecfg.Change, error) {
			out, ch, err := claudecfg.Set(doc, []string{k}, v)
			return out, []claudecfg.Change{ch}, err
		}}
	}
	ts, _, err := r.Apply([]claudecfg.Edit{set("a", 2)})
	if err != nil {
		t.Fatal(err)
	}
	rec := &setup.Record{SchemaVersion: 1, Run: "r", Undo: setup.Undo{ConfigRuns: []string{ts}}}
	_ = rec.Save(d.SetupFile())
	_ = os.WriteFile(settings, []byte(`{"a": 2, "edited": true}`), 0o600)
	if code, out, _ := run(t, "setup", "--undo", "--json"); code != output.Partial || !strings.Contains(out, "--force") {
		t.Fatalf("an edited file must be refused: exit %d\n%s", code, out)
	}
	if code, out, _ := run(t, "setup", "--undo", "--force", "--json"); code != output.OK {
		t.Fatalf("--force: exit %d\n%s", code, out)
	}
	if a, _ := os.ReadFile(settings); string(a) != `{"a": 1}` {
		t.Errorf("--force did not put the pre-image back: %s", a)
	}

	cr := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: "some future step", Now: now}
	ts, _, err = cr.Apply([]claudecfg.Edit{{Target: cj, Apply: func(doc []byte) ([]byte, []claudecfg.Change, error) {
		out, ch, err := claudecfg.Set(doc, []string{"b"}, 2)
		return out, []claudecfg.Change{ch}, err
	}}})
	if err != nil {
		t.Fatal(err)
	}
	rec = &setup.Record{SchemaVersion: 1, Run: "r2", Undo: setup.Undo{ConfigRuns: []string{ts}}}
	_ = rec.Save(d.SetupFile())
	_ = os.WriteFile(cj, []byte(`{"b": 2, "numStartups": 4}`), 0o600)
	if code, out, _ := run(t, "setup", "--undo", "--force", "--json"); code != output.Partial || !strings.Contains(out, "never puts back a whole ~/.claude.json") {
		t.Errorf("--force on ~/.claude.json: exit %d\n%s", code, out)
	}
	if a, _ := os.ReadFile(cj); string(a) != `{"b": 2, "numStartups": 4}` {
		t.Errorf("~/.claude.json overwritten: %s", a)
	}
}

func TestSetupUndoLeavesWhatWasThereBefore(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	claude := claudeRunner.(*fakeClaude)
	claude.plugins = `[{"id":"` + plugin.PluginID() + `"}]` // already installed in this profile
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error { s.Scheduler = &state.Scheduler{Kind: "systemd"}; return nil })
	serverAnswers = func(context.Context, config.Config) bool { return true }
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	rec, _ := setup.Load(pathsOf(home).SetupFile())
	if rec == nil || rec.Undo.ServerStarted || rec.Undo.Scheduler || len(rec.Undo.ClientProfiles) > 0 {
		t.Fatalf("undo would remove what was there before: %+v", rec.Undo)
	}
	f.calls, claude.calls = nil, nil
	if code, _, _ := run(t, "setup", "--undo"); code != output.OK || len(f.calls) > 0 || strings.Contains(strings.Join(mutating(claude), ","), "uninstall") {
		t.Errorf("undo touched pre-existing pieces: exit %d, steps %v, claude %v", code, f.commands(), mutating(claude))
	}
}

func TestSecondSetupKeepsTheFirstRunsUndo(t *testing.T) {
	home, _ := setupMachine(t)
	withLLMKey(t, home)
	claude := claudeRunner.(*fakeClaude)
	args := []string{"setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive"}
	if code, _, _ := run(t, args...); code != output.OK {
		t.Fatal("first setup failed")
	}
	claude.plugins = `[{"id":"` + plugin.PluginID() + `"}]` // now installed, by the first run
	if code, _, _ := run(t, args...); code != output.OK {
		t.Fatal("second setup failed")
	}
	rec, _ := setup.Load(pathsOf(home).SetupFile())
	if rec == nil || len(rec.Undo.ClientProfiles) != 1 || !rec.Undo.Scheduler || !rec.Undo.ServerStarted {
		t.Errorf("the second run forgot what the first must undo: %+v", rec.Undo)
	}
}

func TestSetupUndoTargetsTheProfileSetupWired(t *testing.T) {
	home, _ := setupMachine(t)
	withLLMKey(t, home)
	claude := claudeRunner.(*fakeClaude)
	dev := filepath.Join(home, "dev-profile")
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--config-dir", dev, "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	claude.plugins = `[{"id":"` + plugin.PluginID() + `"}]`
	claude.calls, claude.envs = nil, nil
	if code, out, _ := run(t, "setup", "--undo"); code != output.OK {
		t.Fatalf("undo: exit %d\n%s", code, out)
	}
	for i, c := range claude.calls {
		if strings.Join(c, " ") == "plugin uninstall "+plugin.PluginID()+" --scope user" && strings.Join(claude.envs[i], " ") != "CLAUDE_CONFIG_DIR="+dev {
			t.Errorf("plugin removed with env %v, want the dev profile", claude.envs[i])
		}
	}
	if !strings.Contains(strings.Join(mutating(claude), ","), "plugin uninstall") {
		t.Error("the plugin was not removed")
	}
}

func TestSetupUndoFlagsAreExclusive(t *testing.T) {
	setupMachine(t)
	if code, _, _ := run(t, "setup", "--undo", "--answer", "consent=yes"); code != output.Usage {
		t.Errorf("--undo with answers: exit %d", code)
	}
	if code, _, _ := run(t, "setup", "--force"); code != output.Usage {
		t.Errorf("--force without --undo: exit %d", code)
	}
}

func stateLoad(home string) (state.State, error) { return state.Load(pathsOf(home).StateFile()) }

func TestAnInterruptedUndoContinuesWhereItStopped(t *testing.T) {
	home, _, _, f, claude, _ := referenceSetup(t)
	claude.plugins = `[{"id":"` + plugin.PluginID() + `"}]`
	f.fail["jobs uninstall"] = &output.Error{Code: output.Blocked, Message: "systemctl failed"}
	f.calls = nil
	code, out, _ := run(t, "setup", "--undo", "--json")
	if code != output.Partial || !strings.Contains(out, "backsight setup --undo") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	rec, _ := setup.Load(pathsOf(home).SetupFile())
	if len(rec.Undo.ConfigRuns) != 0 || len(rec.Undo.Timers) != 0 || !rec.Undo.Scheduler {
		t.Fatalf("done actions must be struck, the failed one kept: %+v", rec.Undo)
	}
	delete(f.fail, "jobs uninstall")
	f.calls = nil
	if code, out, _ := run(t, "setup", "--undo", "--json"); code != output.OK || strings.Count(out, `"undo client`) > 0 {
		t.Fatalf("resumed undo: exit %d\n%s", code, out)
	}
	if got := strings.Join(f.commands(), ","); got != "jobs uninstall,server stop" {
		t.Errorf("resumed undo redid or skipped actions: %q", got)
	}
}

func TestClientRetireTellsHowToRestartTheTimer(t *testing.T) {
	referenceProfile(t)
	sys := withSys(t, "linux")
	sys.out = map[string]string{"systemctl --user is-enabled memory-retro.timer": "enabled\n"}
	if code, out, _ := run(t, "client", "retire"); code != output.OK || !strings.Contains(out, "systemctl --user enable --now memory-retro.timer") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSetupKeepsItsOwnConfigOpenToNewAnswers(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	f.fail["server install"] = &output.Error{Code: output.Blocked, Message: "pull failed"}
	if code, _, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive"); code != output.Partial {
		t.Fatalf("exit %d", code)
	}
	delete(f.fail, "server install")
	// Another provider, no hand edit: the config is setup's own.
	if code, out, errOut := run(t, "setup", "--answer", "provider=openai", "--answer", "consent=yes", "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	raw, _ := os.ReadFile(pathsOf(home).ConfigFile())
	if !strings.Contains(string(raw), `provider = "openai"`) || !strings.Contains(string(raw), `model = "gpt-5.4-mini"`) {
		t.Fatalf("config not rewritten:\n%s", raw)
	}
	// Once edited by hand it is the user's: adopted, contradictions refused.
	_ = os.WriteFile(pathsOf(home).ConfigFile(), append(raw, []byte("\n# mine\n")...), 0o600)
	if code, _, errOut := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive"); code != output.Usage || !strings.Contains(errOut, "llm.provider") {
		t.Errorf("an edited config must be adopted: exit %d %s", code, errOut)
	}
}

func TestSetupResumeWithNewAnswersAsksTheGoAgain(t *testing.T) {
	home, f := setupMachine(t)
	f.fail["client install"] = &output.Error{Code: output.Blocked, Message: "x"}
	withLLMKey(t, home)
	if code, _, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive"); code != output.Partial {
		t.Fatalf("exit %d", code)
	}
	delete(f.fail, "client install")
	code, out, _ := run(t, "setup", "--resume", "--answer", "retention=90", "--non-interactive", "--json")
	if env := parseSetup(t, out); code != output.NeedsUser || !strings.Contains(env.Error.Message, "consent") {
		t.Errorf("a changed plan must not run on the old go: exit %d\n%s", code, out)
	}
	if code, _, _ := run(t, "setup", "--resume", "--non-interactive"); code != output.OK {
		t.Errorf("a plain resume keeps the go: exit %d", code)
	}
}

func TestSetupMissingAnswersAlsoNameMissingSecrets(t *testing.T) {
	setupMachine(t)
	code, out, _ := run(t, "setup", "--answer", "provider=anthropic", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.NeedsUser || len(env.Error.Secrets) != 1 || env.Error.Secrets[0] != "llm_api_key" || len(env.Error.Questions.([]any)) == 0 {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSetupResumeWithoutARunIsAUsageError(t *testing.T) {
	setupMachine(t)
	if code, _, errOut := run(t, "setup", "--resume", "--non-interactive"); code != output.Usage || !strings.Contains(errOut, "no setup run") {
		t.Errorf("exit %d %s", code, errOut)
	}
}

func TestSetupStepFailureBeforeAnyChangeIsBlocked(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	_ = os.WriteFile(pathsOf(home).ConfigFile(), []byte("schema_version = 1\n"), 0o600) // adopted: nothing written
	f.fail["server install"] = &output.Error{Code: output.Blocked, Message: "docker gone"}
	if code, out, _ := run(t, "setup", "--answer", "consent=yes", "--non-interactive", "--json"); code != output.Blocked || !strings.Contains(out, "server: docker gone") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestSetupSubscriptionLane(t *testing.T) {
	home, f := setupMachine(t)
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	base := []string{"setup", "--answer", "provider=claude-code", "--answer", "consent=yes", "--non-interactive", "--json"}
	code, out, _ := run(t, base...)
	if env := parseSetup(t, out); code != output.NeedsUser || !strings.Contains(env.Error.Message, "ack_subscription") {
		t.Fatalf("the subscription lane needs its acknowledgement: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, append(base, "--answer", "ack_subscription=")...)
	if env := parseSetup(t, out); code != output.NeedsUser || !strings.Contains(env.Error.Message, "ack_subscription") {
		t.Fatalf("an empty acknowledgement is no acknowledgement: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, append(base, "--answer", "ack_subscription=yes")...)
	env := parseSetup(t, out)
	if code != output.NeedsUser || len(env.Error.Actions) < 2 || env.Error.Actions[0].Cmd != "claude setup-token" {
		t.Fatalf("the token comes from claude setup-token: exit %d\n%s", code, out)
	}
	withLLMKey(t, home)
	if code, out, _ := run(t, append(base, "--answer", "ack_subscription=yes", "--answer", "backfill=yes")...); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	last := f.commands()[len(f.commands())-1]
	if !strings.HasPrefix(last, "retain backfill --queue") || !strings.HasSuffix(last, "--allow-subscription") {
		t.Errorf("an explicit backfill=yes on the subscription lane must allow it: %q", last)
	}
}

func TestSetupAsksAgainAfterAnInvalidAnswer(t *testing.T) {
	home, _ := setupMachine(t)
	withLLMKey(t, home)
	// provider 1, model Enter, retention Enter, backups "maybe" then 2, go.
	withStdin(t, "1\n\n\nmaybe\n2\ny\n")
	stdinIsTerminal = func() bool { return true }
	oldOut := stdoutIsTerminal
	stdoutIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdoutIsTerminal = oldOut })
	code, out, errOut := run(t, "setup")
	if code != output.OK || !strings.Contains(out, "backups=maybe is not one of") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if raw, _ := os.ReadFile(pathsOf(home).ConfigFile()); !strings.Contains(string(raw), "enabled = false") {
		t.Errorf("the second answer was not taken:\n%s", raw)
	}
}

func TestSetupProjectAnswerNamesTheServerAndJobs(t *testing.T) {
	home, _ := setupMachine(t)
	withLLMKey(t, home)
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--answer", "project=bs-fresh",
		"--answer", "api_port=28888", "--answer", "ui_port=29999", "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	raw, _ := os.ReadFile(pathsOf(home).ConfigFile())
	cfg, err := config.Parse(raw, "config.toml", pathsOf(home), home)
	if err != nil || cfg.Server.Project != "bs-fresh" || cfg.Server.URL != "http://127.0.0.1:28888" || cfg.Server.UIPort != 29999 {
		t.Fatalf("config %+v %v\n%s", cfg.Server, err, raw)
	}
	p, err := schedulerPlan(context.Background())
	if err != nil || !strings.Contains(strings.Join(p.Paths(), " "), "bs-fresh-tick.timer") {
		t.Errorf("scheduler entries %v %v", p.Paths(), err)
	}
	// The server runs from this config now: it is adopted.
	if code, _, errOut := run(t, "setup", "--answer", "project=other", "--answer", "consent=yes", "--non-interactive"); code != output.Usage || !strings.Contains(errOut, "server.project") {
		t.Errorf("a new project once the server runs: exit %d %s", code, errOut)
	}
}

func TestSetupSchedulerOffSkipsTheEntries(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--answer", "scheduler=off", "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(strings.Join(f.commands(), ","), "jobs install") {
		t.Errorf("scheduler=off installed entries: %v", f.commands())
	}
	if rec, _ := setup.Load(pathsOf(home).SetupFile()); rec == nil || rec.Undo.Scheduler {
		t.Errorf("undo would remove entries setup never wrote: %+v", rec)
	}
	if code, _, _ := run(t, "setup", "--answer", "scheduler=maybe", "--non-interactive"); code != output.Usage {
		t.Errorf("scheduler=maybe: exit %d", code)
	}
}
