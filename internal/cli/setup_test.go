package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		data = jobsInstallResult{Kind: "systemd"}
	case "verify":
		data = verifyResult{Checks: []verifyCheck{{ID: "mcp", Status: "ok"}}}
	case "retain backfill":
		data = backfillResult{Total: 2, Queued: true}
	case "client retire":
		return runSubcommand(context.Background(), args...) // the real one: pre-images to undo
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
	f := &fakeSteps{fail: map[string]*output.Error{}, home: home}
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
	raw, err := os.ReadFile(pathsOf(home).ConfigFile())
	if err != nil || !strings.Contains(string(raw), `provider = "deepseek"`) || !strings.Contains(string(raw), "enabled = false") {
		t.Fatalf("config not written from the answers: %v\n%s", err, raw)
	}
	if len(f.calls) > 0 {
		t.Fatalf("steps ran without the key: %v", f.commands())
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
	if rec == nil || !rec.Finished || !rec.Undo.CreatedConfig || !rec.Undo.Client || !rec.Undo.Scheduler || !rec.Undo.ServerStarted ||
		len(rec.Undo.ConfigRuns) != 1 || rec.Undo.ConfigRuns[0] != "20261025T120000Z" {
		t.Errorf("record: %+v", rec)
	}
	if rec != nil && rec.Answers["backups"] != "off" {
		t.Errorf("resume lost the saved answers: %v", rec.Answers)
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
	if code != output.OK || env.Data.Steps[0].Status != "skipped" || !strings.Contains(env.Data.Steps[0].Detail, "adopted") {
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
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--config-dir", prof, "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, c := range f.commands()[1:3] {
		if !strings.HasSuffix(c, "--config-dir "+prof) {
			t.Errorf("%q lacks the profile", c)
		}
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

func TestSetupRetiresTheReferenceAfterVerifyAndUndoPutsItBack(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	settings, state := filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude.json")
	_ = os.WriteFile(settings, []byte(referenceSettings), 0o600)
	_ = os.WriteFile(state, []byte(referenceClaudeJSON), 0o600)
	sys := sysRunner.(*fakeSys)
	sys.out = map[string]string{"systemctl --user is-enabled memory-retro.timer": "enabled\n"}
	claude := withClaude(t)

	_, out, _ := run(t, "setup", "--questions", "--json")
	if !strings.Contains(out, `"id": "reference"`) || !strings.Contains(out, "7 pieces") {
		t.Fatalf("no reference question for 6 file changes and the timer:\n%s", out)
	}
	code, out, errOut := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	cmds := strings.Join(f.commands(), ",")
	if !strings.Contains(cmds, "verify,client retire") {
		t.Errorf("retire must follow verify: %s", cmds)
	}
	if a, _ := os.ReadFile(state); strings.Contains(string(a), "user-memory") {
		t.Error("the reference MCP server is still wired")
	}
	rec, _ := setup.Load(pathsOf(home).SetupFile())
	if rec == nil || len(rec.Undo.Timers) != 1 || len(rec.Undo.ConfigRuns) != 2 {
		t.Fatalf("record: %+v", rec)
	}

	// --undo --plan changes nothing.
	f.calls = nil
	if code, out, _ := run(t, "setup", "--undo", "--plan"); code != output.OK || !strings.Contains(out, "planned") || len(f.calls) > 0 {
		t.Fatalf("undo plan: exit %d, steps %v\n%s", code, f.commands(), out)
	}
	if a, _ := os.ReadFile(state); strings.Contains(string(a), "user-memory") {
		t.Error("--undo --plan restored a file")
	}

	claude.plugins = `[{"id":"` + plugin.PluginID() + `"}]`
	claude.marketplaces = `[{"name":"` + plugin.Marketplace + `","path":"/x"}]`
	code, out, errOut = run(t, "setup", "--undo", "--json")
	if code != output.OK {
		t.Fatalf("undo: exit %d\n%s\n%s", code, out, errOut)
	}
	if a, _ := os.ReadFile(settings); string(a) != referenceSettings {
		t.Errorf("settings.json not restored byte for byte:\n%s", a)
	}
	if a, _ := os.ReadFile(state); string(a) != referenceClaudeJSON {
		t.Error("~/.claude.json not restored byte for byte")
	}
	if !contains(sys.calls, "systemctl --user enable --now memory-retro.timer") {
		t.Errorf("the retro timer was not started again: %v", sys.calls)
	}
	got := strings.Join(f.commands(), ",")
	if got != "jobs uninstall,server stop" {
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

func TestSetupUndoRefusesAFileEditedSince(t *testing.T) {
	home, _ := setupMachine(t)
	withLLMKey(t, home)
	withClaude(t)
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	settings := filepath.Join(home, ".claude", "settings.json")
	_ = os.WriteFile(settings, []byte(referenceSettings), 0o600)
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	edited, _ := os.ReadFile(settings)
	edited = append(edited[:len(edited)-2], []byte(`, "theme": "dark"}`+"\n")...)
	_ = os.WriteFile(settings, edited, 0o600)

	code, out, _ := run(t, "setup", "--undo", "--json")
	if code != output.Partial || !strings.Contains(out, "--force") || !strings.Contains(out, `"refused"`) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if a, _ := os.ReadFile(settings); string(a) != string(edited) {
		t.Error("a refused restore wrote the file")
	}
	if code, out, _ := run(t, "setup", "--undo", "--force", "--json"); code != output.OK {
		t.Fatalf("--force: exit %d\n%s", code, out)
	}
	if a, _ := os.ReadFile(settings); string(a) != referenceSettings {
		t.Errorf("--force did not put the pre-image back:\n%s", a)
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
