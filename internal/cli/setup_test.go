package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	agelib "filippo.io/age"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/guide"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/reference"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// fakeSteps stands in for the commands setup chains.
type fakeSteps struct {
	slow  time.Duration // how long server install takes
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
	if key == "server install" {
		time.Sleep(f.slow)
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
	case "guide index":
		data = guide.IndexResult{Bank: "claude-memory-docs", Sections: 40, Sent: 40}
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
	// Never the real Docker: the server step asks it what to download.
	withEngine(t, &engineDocker{info: "28.1.1\n"})
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
	want := []string{"server install", "guide index", "client install", "client retention", "jobs install", "verify", "retain backfill --queue --projects-dir " + filepath.Join(home, ".claude", "projects"), "jobs start tick"}
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

// The docs step indexes the docs bank after the server step; when it
// fails, setup goes on: guide search falls back to its keyword search.
func TestSetupIndexesTheDocsAndGoesOnWhenItFails(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.OK || strings.Join(f.commands()[:2], ",") != "server install,guide index" || !hasStep(env.Data.Steps, "docs", "done", "40 documentation section(s) in bank claude-memory-docs") {
		t.Fatalf("exit %d %v\n%s", code, f.commands(), out)
	}

	home, f = setupMachine(t)
	withLLMKey(t, home)
	f.fail["guide index"] = &output.Error{Code: output.Blocked, Message: "server unreachable"}
	code, out, _ = run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	env = parseSetup(t, out)
	if code != output.OK || !hasStep(env.Data.Steps, "docs", "skipped", "not indexed (server unreachable)") || !strings.Contains(out, "keyword search") {
		t.Errorf("a docs failure must not stop setup: exit %d\n%s", code, out)
	}
	if !strings.Contains(strings.Join(f.commands(), ","), "client install") {
		t.Errorf("setup stopped after the docs step: %v", f.commands())
	}
}

func hasStep(steps []setup.Step, name, status, detail string) bool {
	for _, s := range steps {
		if s.Name == name && s.Status == status && strings.Contains(s.Detail, detail) {
			return true
		}
	}
	return false
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
			if got := f.commands()[3]; got != want {
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
	if !strings.Contains(string(raw), `provider = "openai"`) || !strings.Contains(string(raw), `model = "gpt-6-luna"`) || !strings.Contains(string(raw), "enabled = false") {
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
	return referenceSetupWith(t, referenceSettings, "enabled")
}

// referenceSetupWith is referenceSetup over settingsJSON, with this
// profile's retro timer answering is-enabled with timer.
func referenceSetupWith(t *testing.T, settingsJSON, timer string) (home, settings, state string, f *fakeSteps, claude *fakeClaude, sys *fakeSys) {
	t.Helper()
	home, f = setupMachine(t)
	withLLMKey(t, home)
	claude = claudeRunner.(*fakeClaude)
	sys = sysRunner.(*fakeSys)
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	settings, state = filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude.json")
	_ = os.WriteFile(settings, []byte(settingsJSON), 0o600)
	_ = os.WriteFile(state, []byte(referenceClaudeJSON), 0o600)
	sys.out = retroTimer(os.Getenv("HOME"))
	sys.out["systemctl --user is-enabled "+reference.RetroTimer] = timer + "\n"
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
	if a, _ := os.ReadFile(settings); string(a) != `{"a": 2, "edited": true}` {
		t.Errorf("a refused undo wrote the file: %s", a)
	}
	if rec, _ := setup.Load(d.SetupFile()); rec == nil || len(rec.Undo.ConfigRuns) != 1 || rec.Undo.ConfigRuns[0] != ts {
		t.Fatalf("a refused undo struck its run: %+v", rec)
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
	if len(rec.Undo.ConfigRuns) != 0 || len(rec.Undo.Timers) != 1 || !rec.Undo.Scheduler {
		t.Fatalf("done actions must be struck, the failed one and those after it kept: %+v", rec.Undo)
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
	sys.out = retroTimer(os.Getenv("HOME"))
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
	if !strings.Contains(string(raw), `provider = "openai"`) || !strings.Contains(string(raw), `model = "gpt-6-luna"`) {
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
	cmds := f.commands()
	last := cmds[len(cmds)-2] // then jobs start tick
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

// A whole-file run edited since is planned as refused, and nothing written.
func TestSetupUndoPlanShowsAWholeFileRunRefused(t *testing.T) {
	home, _ := setupMachine(t)
	d := pathsOf(home)
	settings := filepath.Join(home, ".claude", "settings.json")
	_ = os.MkdirAll(filepath.Dir(settings), 0o700)
	_ = os.WriteFile(settings, []byte(`{"a": 1}`), 0o600)
	r := claudecfg.Runner{PreImages: d.PreImagesDir(), Journal: d.JournalFile(), Command: "some future step", Now: now}
	ts, _, err := r.Apply([]claudecfg.Edit{{Target: settings, Apply: func(doc []byte) ([]byte, []claudecfg.Change, error) {
		out, ch, err := claudecfg.Set(doc, []string{"a"}, 2)
		return out, []claudecfg.Change{ch}, err
	}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = (&setup.Record{SchemaVersion: 1, Run: "r", Undo: setup.Undo{ConfigRuns: []string{ts}}}).Save(d.SetupFile())
	status := func() string {
		t.Helper()
		code, out, _ := run(t, "setup", "--undo", "--plan", "--json")
		var env struct {
			Data undoResult `json:"data"`
		}
		_ = json.Unmarshal([]byte(out), &env)
		if code != output.OK || len(env.Data.Actions) != 1 {
			t.Fatalf("plan: exit %d\n%s", code, out)
		}
		return env.Data.Actions[0].Status
	}
	if got := status(); got != "planned" {
		t.Errorf("an untouched file: %q, want planned", got)
	}
	_ = os.WriteFile(settings, []byte(`{"a": 2, "edited": true}`), 0o600)
	if got := status(); got != "refused" {
		t.Errorf("an edited file: %q, want refused", got)
	}
	if a, _ := os.ReadFile(settings); string(a) != `{"a": 2, "edited": true}` {
		t.Errorf("--plan wrote the file: %s", a)
	}
}

// The user lengthened the retention after setup wrote it: undo keeps the
// user's value (removing it would let Claude Code fall back to 30 days and
// delete transcripts) and says so.
func TestSetupUndoKeepsARetentionChangedSince(t *testing.T) {
	_, settings, _, _, claude, _ := referenceSetup(t)
	if _, _, raw := claudeSettings(t, settings); fmt.Sprint(raw["cleanupPeriodDays"]) != "3650" {
		t.Fatalf("setup wrote cleanupPeriodDays %v", raw["cleanupPeriodDays"])
	}
	sj, _ := os.ReadFile(settings)
	_ = os.WriteFile(settings, []byte(strings.Replace(string(sj), "3650", "365", 1)), 0o600)
	claude.plugins = `[{"id":"` + plugin.PluginID() + `"}]`
	code, out, errOut := run(t, "setup", "--undo", "--json")
	if code != output.OK {
		t.Fatalf("undo: exit %d\n%s\n%s", code, out, errOut)
	}
	if _, _, raw := claudeSettings(t, settings); fmt.Sprint(raw["cleanupPeriodDays"]) != "365" {
		t.Errorf("cleanupPeriodDays after undo %v, want the user's 365", raw["cleanupPeriodDays"])
	}
	if !strings.Contains(out, "cleanupPeriodDays = 365 kept") {
		t.Errorf("the kept value is not reported:\n%s", out)
	}
}

// What was there before setup comes back: the user's retention, and a
// memory-tool rule the user had already allowed.
func TestSetupUndoPutsBackTheValuesThatWereThere(t *testing.T) {
	pre := strings.Replace(referenceSettings, `"Bash(git *)"]`, `"Bash(git *)", "`+plugin.ToolNames[0]+`"]`, 1)
	pre = strings.Replace(pre, `"enabledPlugins"`, `"cleanupPeriodDays": 90, "enabledPlugins"`, 1)
	_, settings, _, _, claude, _ := referenceSetupWith(t, pre, "enabled")
	if _, _, raw := claudeSettings(t, settings); fmt.Sprint(raw["cleanupPeriodDays"]) != "3650" {
		t.Fatalf("setup wrote cleanupPeriodDays %v", raw["cleanupPeriodDays"])
	}
	claude.plugins = `[{"id":"` + plugin.PluginID() + `"}]`
	if code, out, errOut := run(t, "setup", "--undo", "--json"); code != output.OK {
		t.Fatalf("undo: exit %d\n%s\n%s", code, out, errOut)
	}
	_, allow, raw := claudeSettings(t, settings)
	if fmt.Sprint(raw["cleanupPeriodDays"]) != "90" {
		t.Errorf("cleanupPeriodDays after undo %v, want 90 as before setup", raw["cleanupPeriodDays"])
	}
	if !contains(allow, plugin.ToolNames[0]) || contains(allow, plugin.ToolNames[1]) {
		t.Errorf("allow after undo %v: the rule the user had must stay, the ones setup added go", allow)
	}
}

// A retro timer already disabled is not this setup's to stop, nor undo's
// to start.
func TestSetupLeavesADisabledRetroTimerAlone(t *testing.T) {
	home, _, state, _, claude, sys := referenceSetupWith(t, referenceSettings, "disabled")
	if a, _ := os.ReadFile(state); strings.Contains(string(a), "user-memory") {
		t.Fatal("the reference was not retired")
	}
	if contains(sys.calls, "systemctl --user disable --now "+reference.RetroTimer) {
		t.Errorf("a disabled timer was stopped: %v", sys.calls)
	}
	if rec, _ := setup.Load(pathsOf(home).SetupFile()); rec == nil || len(rec.Undo.Timers) != 0 {
		t.Fatalf("undo would start a timer setup never stopped: %+v", rec)
	}
	claude.plugins = `[{"id":"` + plugin.PluginID() + `"}]`
	sys.calls = nil
	if code, out, _ := run(t, "setup", "--undo", "--json"); code != output.OK {
		t.Fatalf("undo: exit %d\n%s", code, out)
	}
	for _, c := range sys.calls {
		if strings.Contains(c, "enable") && !strings.Contains(c, "is-enabled") {
			t.Errorf("undo started the timer: %v", sys.calls)
		}
	}
}

// A timer that will not start again stops the undo only after backsight
// is unwired; one whose unit file the user deleted is lost, not a reason
// to stop.
func TestSetupUndoGetsPastAGoneTimer(t *testing.T) {
	home, _, _, f, claude, sys := referenceSetupWith(t, referenceSettings, "enabled")
	claude.plugins = `[{"id":"` + plugin.PluginID() + `"}]`
	claude.marketplaces = `[{"name":"` + plugin.Marketplace + `","path":"/x"}]`
	sys.fail = "systemctl --user enable --now " + reference.RetroTimer
	sys.out["systemctl --user show -p LoadState "+reference.RetroTimer] = "LoadState=loaded\n"
	f.calls = nil
	if code, out, _ := run(t, "setup", "--undo", "--json"); code != output.Partial || !strings.Contains(out, `"failed"`) {
		t.Fatalf("a timer that fails to start: exit %d\n%s", code, out)
	}
	if got := strings.Join(f.commands(), ","); got != "jobs uninstall,server stop" {
		t.Errorf("backsight left wired behind a failing timer: %q", got)
	}
	if !strings.Contains(strings.Join(mutating(claude), ","), "plugin uninstall "+plugin.PluginID()) {
		t.Errorf("plugin not removed before the timer: %v", mutating(claude))
	}
	if rec, _ := setup.Load(pathsOf(home).SetupFile()); rec == nil || len(rec.Undo.Timers) != 1 {
		t.Fatalf("the failed timer must stay to retry: %+v", rec)
	}

	sys.out["systemctl --user show -p LoadState "+reference.RetroTimer] = "LoadState=not-found\n"
	code, out, _ := run(t, "setup", "--undo", "--json")
	if code != output.Partial || !strings.Contains(out, `"lost"`) || !strings.Contains(out, "unit files are gone") {
		t.Fatalf("a deleted unit: exit %d\n%s", code, out)
	}
	if code, out, _ := run(t, "setup", "--undo"); code != output.OK || !strings.Contains(out, "changed nothing") {
		t.Errorf("a lost timer must not block later undos: exit %d\n%s", code, out)
	}
}

// Undo of the default profile leaves the plugin state alone when the state
// records another profile, wired since.
func TestSetupUndoKeepsThePluginStateOfAnotherProfile(t *testing.T) {
	home, _ := setupMachine(t)
	withLLMKey(t, home)
	claude := claudeRunner.(*fakeClaude)
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	dev := filepath.Join(home, "dev")
	_ = state.Update(pathsOf(home), "client install", func(s *state.State) error {
		s.Client = &state.Client{Profile: dev}
		return nil
	})
	claude.plugins = `[{"id":"` + plugin.PluginID() + `"}]`
	if code, out, _ := run(t, "setup", "--undo"); code != output.OK {
		t.Fatalf("undo: exit %d\n%s", code, out)
	}
	if st, _ := stateLoad(home); st.PluginVersion != "1.0.0" || st.Client == nil || st.Client.Profile != dev {
		t.Errorf("undo of ~/.claude cleared the dev profile's plugin state: %+v %+v", st.PluginVersion, st.Client)
	}
}

// Run by an agent with no terminal and no --json/--non-interactive, setup
// must not hang: it exits 10 and names the agent path.
func TestSetupWithoutTerminalNamesTheAgentPath(t *testing.T) {
	_, _ = setupMachine(t)
	oldIn, oldOut := stdinIsTerminal, stdoutIsTerminal
	stdinIsTerminal, stdoutIsTerminal = func() bool { return false }, func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal, stdoutIsTerminal = oldIn, oldOut })
	code, out, errOut := run(t, "setup")
	if code != output.NeedsUser {
		t.Fatalf("exit %d, want %d\n%s\n%s", code, output.NeedsUser, out, errOut)
	}
	for _, want := range []string{"no terminal", "backsight guide agent-guide", "backsight setup --questions --json"} {
		if !strings.Contains(out+errOut, want) {
			t.Errorf("message lacks %q:\n%s\n%s", want, out, errOut)
		}
	}
}

// freePort is a loopback port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// The documented recovery from a mistyped LLM key, in order: setup's
// server step starts the server, its LLM check is refused and the server
// keeps running on its ports; the failure says once what failed, the
// command that fixes it and how to go on; secret set says how to go on;
// setup --resume takes the running server for its own and finishes.
func TestSetupRecoversFromARejectedKeyWithSecretSetAndResume(t *testing.T) {
	real := preflightProbes
	home, f := setupMachine(t)
	withLLMKey(t, home) // the mistyped key
	// The real probes, on a fake Docker: the ports and the own server are
	// what this test is about.
	engine := &engineDocker{info: "26.1.5 name=seccomp\n"}
	withEngine(t, engine)
	preflightProbes = func(ctx context.Context, s setupEnv, x map[string]string) setup.Probes {
		p := real(ctx, s, x)
		p.Claude = func(context.Context) (string, error) { return "2.1.400", nil }
		p.FreeBytes = func() (uint64, error) { return 50 << 30, nil }
		return p
	}
	// What server install answers when the provider refuses the key: the
	// real check against Hindsight's answer for a mistyped DeepSeek key.
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"detail":"Fact extraction rejected by provider authentication: Error code: 401 - {'error': {'message': 'Authentication Fails, Your api key: ****-key is invalid'}}"}`))
	}))
	defer hs.Close()
	_, checkErr := checkProvider(context.Background(), config.Config{Server: config.Server{URL: hs.URL, Bank: "claude-memory"}, LLM: config.LLM{Provider: "deepseek"}}, config.Secrets{})
	var rejected *output.Error
	if !errors.As(checkErr, &rejected) || rejected.Code != output.NeedsUser {
		t.Fatalf("provider check: %v", checkErr)
	}
	f.fail["server install"] = rejected

	api, ui := freePort(t), freePort(t)
	ports := []string{"--answer", fmt.Sprintf("api_port=%d", api), "--answer", fmt.Sprintf("ui_port=%d", ui)}
	code, out, errOut := run(t, append([]string{"setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive"}, ports...)...)
	if code != output.NeedsUser {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	text := out + errOut
	for _, want := range []string{"failed", "server", "the deepseek API rejected the key in llm_api_key", "backsight secret set llm_api_key", "then run: backsight setup --resume"} {
		if !strings.Contains(text, want) {
			t.Errorf("the failure does not say %q:\n%s", want, text)
		}
	}
	if n := strings.Count(text, "rejected the key"); n != 1 {
		t.Errorf("the failure is printed %d times:\n%s", n, text)
	}
	for _, raw := range []string{"HTTP 500", "server_error", `{"detail"`, "dry-run-extract"} {
		if strings.Contains(text, raw) {
			t.Errorf("raw response %q in:\n%s", raw, text)
		}
	}

	// The server it started keeps running on its ports.
	for _, p := range []int{api, ui} {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
	}
	engine.ps = `{"Service":"db","State":"running","Health":"healthy"}` + "\n" + `{"Service":"hindsight","State":"running","Health":"healthy"}` + "\n"

	withStdin(t, "the-right-key\n")
	code, out, errOut = run(t, "secret", "set", "llm_api_key")
	if code != output.OK || !strings.Contains(out, "then run: backsight setup --resume") {
		t.Fatalf("secret set: exit %d\n%s%s", code, out, errOut)
	}

	delete(f.fail, "server install")
	f.calls = nil
	code, out, errOut = run(t, "setup", "--resume", "--non-interactive")
	if code != output.OK {
		t.Fatalf("resume: exit %d\n%s\n%s", code, out, errOut)
	}
	if got := f.commands(); len(got) == 0 || got[0] != "server install" {
		t.Errorf("resume did not run the server step again: %v", got)
	}
	if !strings.Contains(out, "backsight is set up") {
		t.Errorf("resume output:\n%s", out)
	}
}

// Another program on the ports is still refused on a resume, and so is
// backsight's own server when the answers move it to another project: a
// second server would start beside it.
func TestSetupResumeRefusesPortsBacksightDoesNotHold(t *testing.T) {
	real := preflightProbes
	home, f := setupMachine(t)
	withLLMKey(t, home)
	engine := &engineDocker{info: "29.0.1 name=seccomp\n"}
	withEngine(t, engine)
	preflightProbes = func(ctx context.Context, s setupEnv, x map[string]string) setup.Probes {
		p := real(ctx, s, x)
		p.Claude = func(context.Context) (string, error) { return "2.1.400", nil }
		p.FreeBytes = func() (uint64, error) { return 50 << 30, nil }
		return p
	}
	f.fail["server install"] = &output.Error{Code: output.NeedsUser, Message: "the deepseek API rejected the key in llm_api_key", Secrets: []string{"llm_api_key"}}
	api, ui := freePort(t), freePort(t)
	ports := []string{"--answer", fmt.Sprintf("api_port=%d", api), "--answer", fmt.Sprintf("ui_port=%d", ui)}
	if code, out, errOut := run(t, append([]string{"setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive"}, ports...)...); code != output.NeedsUser {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	delete(f.fail, "server install")
	for _, p := range []int{api, ui} {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
	}
	// Nothing of backsight's runs: another program holds the ports.
	f.calls = nil
	if code, out, errOut := run(t, "setup", "--resume", "--non-interactive"); code != output.Blocked || !strings.Contains(out+errOut, "taken by another program") || len(f.calls) > 0 {
		t.Errorf("another program's ports: exit %d %v\n%s%s", code, f.commands(), out, errOut)
	}
	// backsight's server runs, but the answers name another project.
	engine.ps = `{"Service":"hindsight","State":"running","Health":"healthy"}` + "\n"
	if code, out, errOut := run(t, append([]string{"setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--answer", "project=other", "--non-interactive"}, ports...)...); code != output.Blocked || len(f.calls) > 0 {
		t.Errorf("another project on our ports: exit %d %v\n%s%s", code, f.commands(), out, errOut)
	}
}

func fastBeats(t *testing.T) {
	t.Helper()
	oldJ, oldT, oldE := jsonBeat, textBeat, stepElapsed
	jsonBeat, textBeat = 5*time.Millisecond, 5*time.Millisecond
	// A millisecond of the test is about five seconds of a real download.
	stepElapsed = func(t0 time.Time) time.Duration { return time.Since(t0) * 5000 }
	t.Cleanup(func() { jsonBeat, textBeat, stepElapsed = oldJ, oldT, oldE })
}

// On a machine that never ran the server, setup says what it downloads
// and how much, shows Docker's pull progress, then keeps showing that the
// server's first start (the model download) still works.
func TestSetupShowsTheDownloadAndTheFirstStart(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	fastBeats(t)
	engine := &engineDocker{info: "28.1.1\n", fresh: true}
	withEngine(t, engine)
	f.slow = 60 * time.Millisecond
	code, out, errOut := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{
		"downloading 2 server image(s), then the embedding models at its first start: about 2.4 GB",
		"Downloading [=====>",
		"… starting the server; its first start downloads the embedding models (about 1.2 GB): ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	pulls := 0
	for _, c := range engine.calls {
		if strings.HasPrefix(c, "pull ") {
			pulls++
		}
	}
	if pulls != 2 {
		t.Errorf("%d pulls: %q", pulls, engine.calls)
	}
	// Everything cached: no download announced, no pull.
	home, _ = setupMachine(t)
	withLLMKey(t, home)
	cached := &engineDocker{info: "28.1.1\n"}
	withEngine(t, cached)
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive"); code != output.OK || strings.Contains(out, "downloading") {
		t.Errorf("cached: exit %d\n%s", code, out)
	}
	for _, c := range cached.calls {
		if strings.HasPrefix(c, "pull ") {
			t.Errorf("pulled a cached image: %q", c)
		}
	}
}

type progressEvent struct {
	Event  string `json:"event"`
	Step   string `json:"step"`
	Pct    int    `json:"pct"`
	ETA    int    `json:"eta_s"`
	Detail string `json:"detail"`
}

// With --json the progress keeps moving through the download and the
// first start, inside the server step's share, with nothing of Docker's
// output in the stream.
func TestSetupJSONProgressMovesDuringTheDownload(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	fastBeats(t)
	withEngine(t, &engineDocker{info: "28.1.1\n", fresh: true, pullTime: 40 * time.Millisecond})
	f.slow = 40 * time.Millisecond
	code, out, errOut := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	parseSetup(t, out)
	var server []progressEvent
	next := -1
	for _, line := range strings.Split(strings.TrimSpace(errOut), "\n") {
		var ev progressEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Event != "progress" {
			t.Fatalf("not a progress line on stderr: %q", line)
		}
		if ev.Step == "server" {
			server = append(server, ev)
		} else if len(server) > 0 && next < 0 {
			next = ev.Pct
		}
	}
	var downloading, starting int
	for i, ev := range server {
		if strings.Contains(ev.Detail, "downloading") {
			downloading++
		}
		if strings.Contains(ev.Detail, "starting the server") {
			starting++
		}
		if i > 0 && (ev.Pct < server[i-1].Pct || ev.ETA > server[i-1].ETA) {
			t.Errorf("progress went back: %+v then %+v", server[i-1], ev)
		}
		if ev.Pct >= next {
			t.Errorf("the server step reached the next step's %d%%: %+v", next, ev)
		}
	}
	if downloading < 2 || starting < 2 || server[len(server)-1].Pct <= server[0].Pct {
		t.Errorf("the progress did not move during the server step: %+v", server)
	}
	if strings.Contains(out+errOut, "Downloading [") {
		t.Error("Docker's output reached the JSON streams")
	}
}

// A pull that fails stops the server step with Docker's reason.
func TestSetupDownloadFailureSaysWhy(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	withEngine(t, &engineDocker{info: "28.1.1\n", fresh: true, pullErr: errors.New("exit status 1")})
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code == output.OK || env.Error == nil || !strings.Contains(env.Error.Message, "no such host") || env.Error.Resume != setupResume {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, c := range f.commands() {
		if c == "server install" {
			t.Error("server install ran after a failed download")
		}
	}
}

// Setup generates the backup identity when backups are on, as the docs
// say, never over one already there, and not when backups are off.
func TestSetupGeneratesTheBackupIdentity(t *testing.T) {
	home, _ := setupMachine(t)
	withLLMKey(t, home)
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.OK || !hasStep(env.Data.Steps, "scheduler", "done", "generated backup_identity") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	sec, err := config.LoadSecrets(secretsFile(home))
	if err != nil || !strings.HasPrefix(sec.BackupIdentity, "AGE-SECRET-KEY-1") {
		t.Fatalf("no identity after setup: %v", err)
	}
	if strings.Contains(out, sec.BackupIdentity) {
		t.Error("the identity reached the output")
	}

	// One already there is kept.
	home, _ = setupMachine(t)
	withLLMKey(t, home)
	id, _ := agelib.GenerateX25519Identity()
	mine := id.String()
	if err := config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error { return s.Set("backup_identity", mine) }); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if sec, _ := config.LoadSecrets(secretsFile(home)); sec.BackupIdentity != mine {
		t.Error("setup replaced the identity already there")
	}

	// Without the scheduler (a container), backups still need their key.
	home, _ = setupMachine(t)
	withLLMKey(t, home)
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "scheduler=off", "--answer", "consent=yes", "--non-interactive"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if sec, _ := config.LoadSecrets(secretsFile(home)); sec.BackupIdentity == "" {
		t.Error("no identity with scheduler=off")
	}

	// A failure before the server runs generates nothing: it changed
	// nothing.
	home, f := setupMachine(t)
	withLLMKey(t, home)
	f.fail["server install"] = &output.Error{Code: output.Blocked, Message: "docker gone"}
	_, _, _ = run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive")
	if sec, _ := config.LoadSecrets(secretsFile(home)); sec.BackupIdentity != "" {
		t.Error("an identity generated before the server ran")
	}

	// Backups off: nothing to encrypt for.
	home, _ = setupMachine(t)
	withLLMKey(t, home)
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "backups=off", "--answer", "consent=yes", "--non-interactive"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if sec, _ := config.LoadSecrets(secretsFile(home)); sec.BackupIdentity != "" {
		t.Error("an identity generated with backups off")
	}
}

// On a client of a shared server the server, its docs bank and its
// backups are the host's: setup skips the server and docs steps, plan and
// run, never runs guide index, and generates no backup identity.
func TestSetupOnAClientSkipsDocsAndBackupIdentity(t *testing.T) {
	home, f := setupMachine(t)
	_ = os.MkdirAll(filepath.Dir(pathsOf(home).ConfigFile()), 0o700)
	if err := os.WriteFile(pathsOf(home).ConfigFile(), []byte("schema_version = 1\n[server]\nmode = \"remote\"\nurl = \"http://100.101.1.2:8888\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error { return s.Set("api_key", "fakeapikey") }); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--plan", "--json")
	steps := parseSetup(t, out).Data.Steps
	if code != output.OK || !hasStep(steps, "server", "skipped", "100.101.1.2") || !hasStep(steps, "docs", "skipped", "the server's host indexes its docs") ||
		hasStep(steps, "scheduler", "planned", "backup identity") {
		t.Errorf("plan: exit %d\n%s", code, out)
	}
	code, out, errOut := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	steps = parseSetup(t, out).Data.Steps
	if code != output.OK || !hasStep(steps, "server", "skipped", "100.101.1.2") || !hasStep(steps, "docs", "skipped", "the server's host indexes its docs") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if cmds := strings.Join(f.commands(), ","); strings.Contains(cmds, "guide index") || strings.Contains(cmds, "server install") || !strings.Contains(cmds, "client install") {
		t.Errorf("steps run: %s", cmds)
	}
	if sec, err := config.LoadSecrets(secretsFile(home)); err != nil || sec.BackupIdentity != "" || hasStep(steps, "scheduler", "done", "backup_identity") {
		t.Errorf("a client generated a backup identity: %v\n%s", err, out)
	}
}

// A client extracts nothing: its [llm] only prices estimates (connect
// writes the host's). Setup there asks nothing about the provider, not
// even the subscription caveat of a claude-code host, and checks only the
// API key.
func TestSetupOnAClientAsksNothingAboutTheProvider(t *testing.T) {
	home, _ := setupMachine(t)
	_ = os.MkdirAll(filepath.Dir(pathsOf(home).ConfigFile()), 0o700)
	if err := os.WriteFile(pathsOf(home).ConfigFile(), []byte("schema_version = 1\n[server]\nmode = \"remote\"\nurl = \"http://100.101.1.2:8888\"\n[llm]\nprovider = \"claude-code\"\nmodel = \"sonnet\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error { return s.Set("api_key", "fakeapikey") }); err != nil {
		t.Fatal(err)
	}
	_, out, _ := run(t, "setup", "--questions", "--json")
	for _, q := range parseSetup(t, out).Data.Questions {
		if q.ID == "ack_subscription" || q.ID == "base_url" || ((q.ID == "provider" || q.ID == "model") && q.Asked()) {
			t.Errorf("a client is asked %s:\n%s", q.ID, out)
		}
	}
	code, out, _ := run(t, "setup", "--plan", "--json")
	if steps := parseSetup(t, out).Data.Steps; code != output.OK || !hasStep(steps, "secrets", "planned", "check api_key") || strings.Contains(out, "llm_api_key") {
		t.Errorf("plan: exit %d\n%s", code, out)
	}
	if code, out, errOut := run(t, "setup", "--answer", "consent=yes", "--non-interactive", "--json"); code != output.OK {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}

func atATerminal(t *testing.T, input string) {
	t.Helper()
	withStdin(t, input)
	stdinIsTerminal = func() bool { return true }
	oldOut := stdoutIsTerminal
	stdoutIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdoutIsTerminal = oldOut })
}

// A rerun at a terminal reuses the saved answers and says how to change
// them; --reconfigure asks again with the saved answers as defaults.
func TestSetupSaysHowToChangeAnAnswerAndReasksWithReconfigure(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	// The server step fails: the config stays setup's own, its answers open.
	f.fail["server install"] = &output.Error{Code: output.NeedsUser, Message: "the deepseek API rejected the key in llm_api_key", Secrets: []string{"llm_api_key"}}
	atATerminal(t, "1\n\n\n\ny\n") // deepseek, its model, retention, backups, go
	if code, out, errOut := run(t, "setup"); code != output.NeedsUser {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	delete(f.fail, "server install")

	// A rerun asks nothing, and says how to change an answer.
	atATerminal(t, "n\n")
	code, out, _ := run(t, "setup")
	if code != output.OK || strings.Contains(out, "Which LLM") || !strings.Contains(out, "backsight setup --reconfigure") {
		t.Fatalf("rerun: exit %d\n%s", code, out)
	}

	// --reconfigure asks again; Enter keeps the saved answer, a new one
	// replaces it: openai (3), its model, its base URL, retention kept,
	// backups off (2), go.
	atATerminal(t, "3\n\n\n\n2\ny\n")
	code, out, errOut := run(t, "setup", "--reconfigure")
	if code != output.OK || !strings.Contains(out, "Which LLM") {
		t.Fatalf("reconfigure: exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "choice [deepseek]") || !strings.Contains(out, "choice [3650]") || !strings.Contains(out, "choice [on]") {
		t.Errorf("the saved answers are not the defaults:\n%s", out)
	}
	raw, _ := os.ReadFile(pathsOf(home).ConfigFile())
	if !strings.Contains(string(raw), `provider = "openai"`) || !strings.Contains(string(raw), "enabled = false") {
		t.Errorf("new answers not applied:\n%s", raw)
	}

	// Once the server runs, the answers in config.toml are the server's:
	// --reconfigure says where to change them, and asks the rest (the
	// retention, then the go).
	atATerminal(t, "\nn\n")
	if code, out, _ := run(t, "setup", "--reconfigure"); code != output.OK || !strings.Contains(out, "kept from "+pathsOf(home).ConfigFile()+": provider=openai") {
		t.Errorf("finished install: exit %d\n%s", code, out)
	}

	// Without a terminal or with --resume, --reconfigure is refused.
	for _, args := range [][]string{{"--reconfigure", "--non-interactive"}, {"--reconfigure", "--json"}, {"--reconfigure", "--resume"}, {"--reconfigure", "--plan"}} {
		atATerminal(t, "")
		if code, out, errOut := run(t, append([]string{"setup"}, args...)...); code != output.Usage || !strings.Contains(out+errOut, "--reconfigure") {
			t.Errorf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
	}
	stdinIsTerminal = func() bool { return false }
	if code, out, errOut := run(t, "setup", "--reconfigure"); code != output.Usage || !strings.Contains(out+errOut, "--answer id=value") {
		t.Errorf("no terminal: exit %d\n%s%s", code, out, errOut)
	}
}

// Without a systemd user session, setup stops before writing anything
// with exit 10: the message names scheduler=off and what stops without
// it, and the action is the same command with that answer, which works.
func TestSetupWithoutAUserSessionAsksForSchedulerOff(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	sys := withSys(t, "linux")
	sys.noBus = true
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.NeedsUser || env.Error == nil {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, s := range []string{"scheduler=off", "jobs tick", "jobs maintain", "No medium found"} {
		if !strings.Contains(env.Error.Message, s) {
			t.Errorf("message lacks %q: %s", s, env.Error.Message)
		}
	}
	want := "backsight setup --answer provider=deepseek --answer consent=yes --non-interactive --json --answer scheduler=off"
	if len(env.Error.Actions) != 1 || env.Error.Actions[0].Cmd != want || env.Error.Resume != want {
		t.Errorf("actions %+v, resume %q, want %q", env.Error.Actions, env.Error.Resume, want)
	}
	if _, err := os.Stat(pathsOf(home).ConfigFile()); err == nil || len(f.calls) > 0 {
		t.Fatalf("setup acted without a user session: %v", f.commands())
	}
	args := strings.Fields(strings.TrimPrefix(want, "backsight "))
	if code, out, _ := run(t, args...); code != output.OK {
		t.Fatalf("with scheduler=off: exit %d\n%s", code, out)
	}
	if strings.Contains(strings.Join(f.commands(), ","), "jobs") {
		t.Errorf("scheduler=off touched the scheduler: %v", f.commands())
	}
}

// The plan shows the missing session as a failed check with its fix.
func TestSetupPlanShowsTheMissingUserSession(t *testing.T) {
	setupMachine(t)
	withSys(t, "linux").noBus = true
	code, out, _ := run(t, "setup", "--plan", "--answer", "provider=deepseek")
	if code != output.OK || !strings.Contains(out, "fail   scheduler") || !strings.Contains(out, "scheduler=off") {
		t.Errorf("exit %d\n%s", code, out)
	}
	withSys(t, "linux") // a user session answers
	code, out, _ = run(t, "setup", "--plan", "--answer", "provider=deepseek")
	if code != output.OK || !strings.Contains(out, "ok     scheduler   systemd user session reachable") {
		t.Errorf("with a session: exit %d\n%s", code, out)
	}
}

// A run whose scheduler step failed for want of a user session (an older
// binary) left its entries: a resume answering scheduler=off keeps the
// go and the finished steps, shows its progress, and removes them.
func TestSetupResumeWithSchedulerOffRemovesTheEntriesItWrote(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	sys := withSys(t, "linux")
	f.real["jobs install"], f.real["jobs uninstall"] = true, true
	sys.fail, sys.output = "systemctl --user daemon-reload", busError
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json"); code == output.OK {
		t.Fatalf("the scheduler step passed without a user session\n%s", out)
	}
	timer := filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")
	if st, _ := stateLoad(home); st.Scheduler == nil {
		t.Fatal("no entries recorded: nothing to test")
	}
	if _, err := os.Stat(timer); err != nil {
		t.Fatal("no entries written: nothing to test")
	}
	sys.fail, sys.noBus = "", true
	f.calls = nil
	code, out, errOut := run(t, "setup", "--resume", "--answer", "scheduler=off", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.OK {
		t.Fatalf("resume: exit %d\n%s\n%s", code, out, errOut)
	}
	if got := strings.Join(f.commands(), ","); got != "jobs uninstall,verify" {
		t.Errorf("steps %q, want the scheduler's removal then verify", got)
	}
	if !strings.Contains(errOut, `"step":"scheduler"`) {
		t.Errorf("no progress on stderr:\n%s", errOut)
	}
	if !hasStep(env.Data.Steps, "scheduler", "done", "removed") {
		t.Errorf("steps %+v", env.Data.Steps)
	}
	if _, err := os.Stat(timer); err == nil {
		t.Error("the entries stay on disk")
	}
	if st, _ := stateLoad(home); st.Scheduler != nil {
		t.Errorf("the record stays: %+v", st.Scheduler)
	}
	if rec, _ := setup.Load(pathsOf(home).SetupFile()); rec == nil || rec.Undo.Scheduler || rec.Answers["scheduler"] != "off" || !rec.Finished {
		t.Errorf("record %+v", rec)
	}
}

// A finished run turned to scheduler=off by a resume removes its working
// entries too, and keeps the scheduler step in its place.
func TestSetupResumeWithSchedulerOffAfterAFinishedRun(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	withSys(t, "linux")
	f.real["jobs install"], f.real["jobs uninstall"] = true, true
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	f.calls = nil
	code, out, _ := run(t, "setup", "--resume", "--answer", "scheduler=off", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.OK || strings.Join(f.commands(), ",") != "jobs uninstall,verify" {
		t.Fatalf("exit %d, steps %v\n%s", code, f.commands(), out)
	}
	if _, err := os.Stat(filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")); err == nil {
		t.Error("the entries stay")
	}
	i := slices.IndexFunc(env.Data.Steps, func(s setup.Step) bool { return s.Name == "scheduler" })
	if i < 0 || env.Data.Steps[i+1].Name != "verify" || env.Data.Steps[i].Status != "done" {
		t.Errorf("steps %+v", env.Data.Steps)
	}
}

// A resume reuses the go only when it does less: a new provider or port,
// even beside scheduler=off, asks for consent again (exit 10, nothing
// run); an explicit no declines; the saved provider repeated keeps it.
func TestSetupResumeConsentRules(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	f.fail["server install"] = &output.Error{Code: output.Blocked, Message: "docker gone"}
	if code, _, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive"); code != output.Partial {
		t.Fatalf("exit %d", code)
	}
	delete(f.fail, "server install")
	for _, extra := range [][]string{
		{"--answer", "provider=anthropic"},
		{"--answer", "scheduler=off", "--answer", "api_port=28888"},
	} {
		f.calls = nil
		code, out, _ := run(t, append(append([]string{"setup", "--resume"}, extra...), "--non-interactive", "--json")...)
		env := parseSetup(t, out)
		if code != output.NeedsUser || env.Error == nil || !strings.Contains(env.Error.Message, "consent") || len(f.calls) > 0 {
			t.Errorf("%v: exit %d, steps %v\n%s", extra, code, f.commands(), out)
		}
	}
	f.calls = nil
	code, out, _ := run(t, "setup", "--resume", "--answer", "consent=no", "--non-interactive", "--json")
	if env := parseSetup(t, out); code != output.OK || !env.Data.Declined || len(f.calls) > 0 {
		t.Errorf("consent=no: exit %d, steps %v\n%s", code, f.commands(), out)
	}
	f.calls = nil
	code, out, _ = run(t, "setup", "--resume", "--answer", "provider=deepseek", "--non-interactive", "--json")
	if code != output.OK || !strings.HasPrefix(strings.Join(f.commands(), ","), "server install,") {
		t.Errorf("the saved provider repeated: exit %d, steps %v\n%s", code, f.commands(), out)
	}
}

// Entries a manual jobs install made before setup are not setup's:
// scheduler=off leaves them in place.
func TestSetupSchedulerOffLeavesManualEntries(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	withSys(t, "linux")
	f.real["jobs uninstall"] = true
	if code, out, _ := run(t, "jobs", "install"); code != output.OK {
		t.Fatalf("jobs install: exit %d %s", code, out)
	}
	if code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--answer", "scheduler=off", "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(strings.Join(f.commands(), ","), "jobs uninstall") {
		t.Errorf("removed entries setup never wrote: %v", f.commands())
	}
	if _, err := os.Stat(filepath.Join(home, "xdg", "systemd", "user", "backsight-tick.timer")); err != nil {
		t.Error("the manual entries are gone")
	}
	if st, _ := stateLoad(home); st.Scheduler == nil {
		t.Error("the manual entries' record is gone")
	}
}

// A resume whose scheduler step is done does not probe the user session
// again: an su shell can finish it.
func TestSetupResumeAfterTheSchedulerSkipsTheProbe(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	sys := withSys(t, "linux")
	f.fail["verify"] = &output.Error{Code: output.Blocked, Message: "mcp down"}
	if code, _, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive"); code != output.Partial {
		t.Fatalf("exit %d", code)
	}
	delete(f.fail, "verify")
	sys.noBus = true
	code, out, _ := run(t, "setup", "--resume", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, c := range env.Data.Preflight {
		if c.ID == "scheduler" {
			t.Errorf("probed again: %+v", c)
		}
	}
	if slices.Contains(sys.calls, "systemctl --user show-environment") {
		t.Errorf("calls %q", sys.calls)
	}
}

// A user manager that does not answer in time is retried from a login
// session (exit 20), not answered away with scheduler=off alone.
func TestSetupSlowUserSessionSaysRetry(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	withSys(t, "linux").hang = true
	old := userSessionWait
	userSessionWait = 20 * time.Millisecond
	t.Cleanup(func() { userSessionWait = old })
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "did not answer") ||
		!strings.Contains(env.Error.Message, "retry, from a login session") || len(env.Error.Actions) != 0 || len(f.calls) > 0 {
		t.Errorf("exit %d, steps %v\n%s", code, f.commands(), out)
	}
}

// The import starts as setup ends: once past sessions are queued, setup
// starts the first tick through the scheduler instead of leaving it to the
// next 15-minute slot; without a scheduler it says what to run, and a start
// that fails leaves the timer to it without failing setup.
func TestSetupStartsTheImportAtOnce(t *testing.T) {
	for _, c := range []struct {
		name, scheduler string
		fail            bool
		started         bool
		detail          string
	}{
		{"scheduler", "", false, true, "the import started now"},
		{"start fails", "", true, true, "the timer starts it within 15 min"},
		{"no scheduler", "scheduler=off", false, false, "run backsight jobs tick"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home, f := setupMachine(t)
			withLLMKey(t, home)
			oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
			if c.fail {
				f.fail["jobs start"] = &output.Error{Code: output.Blocked, Message: "unit not found"}
			}
			args := []string{"setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--answer", "backfill=yes", "--non-interactive", "--json"}
			if c.scheduler != "" {
				args = append(args, "--answer", c.scheduler)
			}
			code, out, errOut := run(t, args...)
			if code != output.OK {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			cmds := f.commands()
			if last := cmds[len(cmds)-1]; (last == "jobs start tick") != c.started {
				t.Errorf("commands %q: started %v, want %v", cmds, last == "jobs start tick", c.started)
			}
			var detail string
			for _, st := range parseSetup(t, out).Data.Steps {
				if st.Name == "backfill" {
					detail = st.Detail
				}
			}
			if !strings.Contains(detail, c.detail) {
				t.Errorf("backfill step %q, want %q", detail, c.detail)
			}
		})
	}
}

// Only a network-shaped pull failure is blamed on the network (issue 60).
func TestDownloadErrorNamesTheCause(t *testing.T) {
	unpack := &server.PullError{Image: "pgvector/pgvector:pg18", Attempts: 3, Err: errors.New("exit status 1"),
		Last: "failed to extract layer (x) to overlayfs: failed to unmount /var/lib/containerd/tmpmounts/m: device or resource busy"}
	msg := downloadError(unpack).Error()
	if strings.Contains(msg, "network") || !strings.Contains(msg, "could not unpack it") || !strings.Contains(msg, "docker pull pgvector/pgvector:pg18") {
		t.Errorf("unpack: %s", msg)
	}
	dns := &server.PullError{Image: "i", Attempts: 3, Err: errors.New("exit status 1"), Last: "dial tcp: lookup ghcr.io: no such host"}
	if msg := downloadError(dns).Error(); !strings.Contains(msg, "check the network") {
		t.Errorf("network: %s", msg)
	}
}

// serverOnlyMachine is setupMachine for a host without Claude Code: the
// claude probe and the claude command fail the test when reached.
func serverOnlyMachine(t *testing.T) (string, *fakeSteps, *fakeClaude) {
	t.Helper()
	home, f := setupMachine(t)
	withLLMKey(t, home)
	claude := claudeRunner.(*fakeClaude)
	probes := preflightProbes
	preflightProbes = func(ctx context.Context, s setupEnv, x map[string]string) setup.Probes {
		p := probes(ctx, s, x)
		p.Claude = func(context.Context) (string, error) {
			t.Error("pre-flight asked for Claude Code on a server-only host")
			return "", errors.New("no claude")
		}
		return p
	}
	return home, f, claude
}

func TestSetupServerOnly(t *testing.T) {
	home, f, claude := serverOnlyMachine(t)
	code, out, errOut := run(t, "setup", "--role", "server", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	env := parseSetup(t, out)
	want := map[string]string{"secrets": "skipped", "config": "done", "server": "done", "docs": "done", "scheduler": "done",
		"client": "skipped", "retention": "skipped", "verify": "skipped", "reference": "skipped", "backfill": "skipped"}
	got := map[string]string{}
	for _, st := range env.Data.Steps {
		got[st.Name] = st.Status
		if want[st.Name] == "skipped" && st.Name != "secrets" && st.Detail != "server-only host" {
			t.Errorf("%s: detail %q, want server-only host", st.Name, st.Detail)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("steps %v\nwant  %v", got, want)
	}
	if cmds := strings.Join(f.commands(), ", "); cmds != "server install, guide index, jobs install" {
		t.Errorf("commands %s", cmds)
	}
	if len(claude.calls) > 0 {
		t.Errorf("claude ran on a server-only host: %v", claude.calls)
	}
	raw, _ := os.ReadFile(pathsOf(home).ConfigFile())
	cfg, err := config.Parse(raw, "config.toml", pathsOf(home), home)
	if err != nil || !cfg.ServerOnly() {
		t.Fatalf("config %+v %v\n%s", cfg.Server, err, raw)
	}
	for _, line := range append(env.Data.NextSteps, env.Data.TryFirst) {
		if strings.Contains(line, "Claude Code") {
			t.Errorf("a server-only host is told about Claude Code: %q", line)
		}
	}
	if !slices.ContainsFunc(env.Data.NextSteps, func(s string) bool { return strings.Contains(s, "backsight server expose") }) {
		t.Errorf("next steps do not say how to open the server to the other PCs: %v", env.Data.NextSteps)
	}

	// A rerun keeps the role; undo reverses what ran, without Claude Code.
	if code, out, _ := run(t, "setup", "--answer", "consent=yes", "--non-interactive", "--json"); code != output.OK || !hasStep(parseSetup(t, out).Data.Steps, "client", "skipped", "server-only host") {
		t.Errorf("rerun without --role: exit %d\n%s", code, out)
	}
	f.calls = nil
	if code, out, _ := run(t, "setup", "--undo", "--json"); code != output.OK {
		t.Fatalf("undo: exit %d\n%s", code, out)
	}
	if cmds := strings.Join(f.commands(), ", "); cmds != "jobs uninstall, server stop" {
		t.Errorf("undo commands %s", cmds)
	}
	if len(claude.calls) > 0 {
		t.Errorf("undo ran claude: %v", claude.calls)
	}
}

func TestSetupServerOnlyRoleAgainstAnExistingConfig(t *testing.T) {
	home, _, _ := serverOnlyMachine(t)
	_ = os.WriteFile(pathsOf(home).ConfigFile(), []byte("schema_version = 1\n[server]\nurl = \"http://127.0.0.1:18888\"\nui_port = 19999\n"), 0o600)
	code, _, errOut := run(t, "setup", "--role", "server", "--answer", "consent=yes", "--non-interactive")
	if code != output.Usage || !strings.Contains(errOut, "server.role") {
		t.Errorf("--role server over a both config: exit %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "setup", "--role", "client", "--non-interactive"); code != output.Usage {
		t.Errorf("--role client: exit %d %s", code, errOut)
	}
}

// The plan shows the Claude Code steps skipped, and a resume after the
// missing key keeps the role.
func TestSetupServerOnlyPlanAndResume(t *testing.T) {
	home, f := setupMachine(t)
	code, out, _ := run(t, "setup", "--role", "server", "--answer", "provider=deepseek", "--plan", "--json")
	if code != output.OK || !hasStep(parseSetup(t, out).Data.Steps, "verify", "skipped", "server-only host") {
		t.Errorf("plan: exit %d\n%s", code, out)
	}
	if code, _, _ := run(t, "setup", "--role", "server", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json"); code != output.NeedsUser {
		t.Fatalf("without the key: exit %d", code)
	}
	withLLMKey(t, home)
	if code, out, _ := run(t, "setup", "--resume", "--non-interactive", "--json"); code != output.OK {
		t.Fatalf("resume: exit %d\n%s", code, out)
	}
	raw, _ := os.ReadFile(pathsOf(home).ConfigFile())
	if !strings.Contains(string(raw), `role = "server"`) || strings.Contains(strings.Join(f.commands(), ","), "client install") {
		t.Errorf("resume lost the role: %v\n%s", f.commands(), raw)
	}
}
