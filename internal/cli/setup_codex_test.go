package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/setup"
)

func setupQuestionIDs(qs []setup.Question) []string {
	var out []string
	for _, q := range qs {
		out = append(out, q.ID)
	}
	return out
}

func stepsByName(steps []setup.Step) map[string]setup.Step {
	out := map[string]setup.Step{}
	for _, s := range steps {
		out[s.Name] = s
	}
	return out
}

// Without codex, setup is as it was: no tools question, Claude Code's
// steps, and an answer asking for Codex refused.
func TestSetupWithoutCodexAsksNothingOfIt(t *testing.T) {
	setupMachine(t)
	code, out, _ := run(t, "setup", "--questions", "--json")
	env := parseSetup(t, out)
	if code != output.OK || slices.Contains(setupQuestionIDs(env.Data.Questions), "tools") || slices.Contains(setupQuestionIDs(env.Data.Questions), "codex_memories") {
		t.Errorf("exit %d, questions %v", code, setupQuestionIDs(env.Data.Questions))
	}
	if code, out, _ := run(t, "setup", "--answer", "tools=codex", "--questions", "--json"); code != output.Usage || !strings.Contains(out, "codex command was not found") {
		t.Errorf("tools=codex without codex: exit %d %s", code, out)
	}
}

// With Codex and Claude Code both there, the tools question defaults to
// both; Codex's memories on asks whether to turn them off, recommending it.
func TestSetupWithCodexAsksWhichTools(t *testing.T) {
	home, _ := setupMachine(t)
	f := withCodex(t, home)
	f.memories = true
	codexHomeWith(t, home, codexStart)
	code, out, _ := run(t, "setup", "--questions", "--json")
	env := parseSetup(t, out)
	ids := setupQuestionIDs(env.Data.Questions)
	if code != output.OK || !slices.Contains(ids, "tools") || !slices.Contains(ids, "codex_memories") || !slices.Contains(ids, "retention") {
		t.Fatalf("exit %d, questions %v", code, ids)
	}
	for _, q := range env.Data.Questions {
		switch q.ID {
		case "tools":
			if q.Default != setup.ToolsBoth {
				t.Errorf("tools default %q", q.Default)
			}
		case "codex_memories":
			if q.Default != "off" || q.Recommended != "off" {
				t.Errorf("codex_memories %+v", q)
			}
		}
	}
	// Codex alone: no Claude Code question.
	_, out, _ = run(t, "setup", "--answer", "tools=codex", "--questions", "--json")
	if ids := setupQuestionIDs(parseSetup(t, out).Data.Questions); slices.Contains(ids, "retention") || !slices.Contains(ids, "codex_memories") {
		t.Errorf("tools=codex: %v", ids)
	}
	// Memories already off: not asked.
	f.memories = false
	_, out, _ = run(t, "setup", "--questions", "--json")
	if ids := setupQuestionIDs(parseSetup(t, out).Data.Questions); slices.Contains(ids, "codex_memories") {
		t.Errorf("memories off, asked anyway: %v", ids)
	}
}

// Setup for Codex alone: Claude Code's steps skipped, Codex wired, its
// memories turned off after the import step, the trust to give said; the
// pre-flight checks Codex, not Claude Code. setup --undo then gives
// config.toml back byte for byte and removes the plugins.
func TestSetupWiresCodexAndUndoGivesItBack(t *testing.T) {
	home, steps := setupMachine(t)
	withLLMKey(t, home)
	steps.real["client install"], steps.real["client codex-memories"] = true, true
	f := withCodex(t, home)
	f.memories = true
	dir := codexHomeWith(t, home, codexStart)

	code, out, _ := run(t, "setup", "--plan", "--answer", "provider=deepseek", "--answer", "tools=codex", "--json")
	plan := parseSetup(t, out)
	if code != output.OK || stepsByName(plan.Data.Steps)["codex"].Status != "planned" || stepsByName(plan.Data.Steps)["codex_memories"].Status != "planned" ||
		stepsByName(plan.Data.Steps)["client"].Status != "skipped" || len(f.mutating()) != 0 {
		t.Fatalf("plan: exit %d %+v", code, plan.Data.Steps)
	}

	code, out, errOut := run(t, "setup", "--answer", "provider=deepseek", "--answer", "tools=codex", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	var names []string
	for _, s := range env.Data.Steps {
		names = append(names, s.Name)
	}
	if i, j := slices.Index(names, "import"), slices.Index(names, "codex_memories"); i < 0 || j != i+1 || slices.Index(names, "codex") != slices.Index(names, "retention")+1 {
		t.Errorf("step order %v", names)
	}
	by := stepsByName(env.Data.Steps)
	for _, n := range claudeSteps {
		if by[n].Status != "skipped" || !strings.Contains(by[n].Detail, "not among the tools") {
			t.Errorf("%s: %+v", n, by[n])
		}
	}
	if by["codex"].Status != "done" || by["codex_memories"].Status != "done" {
		t.Errorf("codex steps %+v %+v", by["codex"], by["codex_memories"])
	}
	for _, c := range env.Data.Preflight {
		if c.ID == "claude" {
			t.Error("the pre-flight checked Claude Code")
		}
	}
	if !slices.ContainsFunc(env.Data.Preflight, func(c setup.Check) bool { return c.ID == "codex" && c.Status == setup.OK }) {
		t.Errorf("no Codex check: %+v", env.Data.Preflight)
	}
	if !slices.ContainsFunc(env.Data.NextSteps, func(s string) bool { return strings.Contains(s, "/hooks") }) || !strings.Contains(env.Data.TryFirst, "Codex") {
		t.Errorf("next steps %v, try %q", env.Data.NextSteps, env.Data.TryFirst)
	}
	if cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml")); strings.Contains(string(cfg), "memories = true") {
		t.Errorf("memories still on:\n%s", cfg)
	}
	rec, _ := setup.Load(pathsOf(home).SetupFile())
	if rec == nil || !slices.Equal(rec.Undo.CodexHomes, []string{dir}) || len(rec.Undo.ConfigRuns) != 1 {
		t.Fatalf("record %+v", rec)
	}
	if slices.ContainsFunc(f.calls, func(c codexCall) bool { return strings.Contains(c.line, "claude") }) {
		t.Error("odd codex calls")
	}

	trustAll(t, dir) // the user trusts the hooks in Codex
	code, out, errOut = run(t, "setup", "--undo", "--json")
	if code != output.OK {
		t.Fatalf("undo: exit %d %s %s", code, out, errOut)
	}
	if cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml")); string(cfg) != codexStart+"\n[hooks.state]\n" {
		t.Errorf("config.toml after undo:\n%q", cfg)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "cache", "backsight")); !os.IsNotExist(err) {
		t.Error("the plugin cache is left")
	}
}

// Both tools: Claude Code's steps run, and Codex's; a user server named
// memory stops the Codex step, naming it.
func TestSetupWiresBothAndStopsOnAMemoryServer(t *testing.T) {
	home, steps := setupMachine(t)
	withLLMKey(t, home)
	f := withCodex(t, home)
	codexHomeWith(t, home, "[mcp_servers.memory]\ncommand = \"/usr/bin/mem\"\n")
	steps.real["client install"] = true
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.NeedsUser || env.Error == nil || !strings.Contains(env.Error.Message, "[mcp_servers.memory]") {
		t.Fatalf("exit %d %s", code, out)
	}
	if by := stepsByName(env.Data.Steps); by["client"].Status != "done" || by["codex"].Status != "failed" {
		t.Errorf("steps %+v", env.Data.Steps)
	}
	if len(f.mutating()) != 0 {
		t.Errorf("codex ran %v", f.mutating())
	}
}
