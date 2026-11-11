package cli

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/fileimport"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/setup"
)

// codexWithMemories is a machine with Codex, its memories on and holding
// a consolidated file and one conversation's summary.
func codexWithMemories(t *testing.T) (home, dir string, steps *fakeSteps, f *fakeCodexCLI) {
	t.Helper()
	home, steps = setupMachine(t)
	withLLMKey(t, home)
	f = withCodex(t, home)
	f.memories = true
	dir = codexHomeWith(t, home, codexStart)
	writeCodexFile(t, filepath.Join(dir, "memories_v2", "memory_summary.md"), "v1\n## User Profile\nprefers Go\n")
	writeCodexFile(t, filepath.Join(dir, "memories_v2", "rollout_summaries", "a.md"), codexSummary("/src/app", "2026-11-10T19:00:00+00:00", "the app's tests need Docker"))
	return home, dir, steps, f
}

// stepCallIndex is the position of the first subcommand whose line starts
// with prefix, -1 when none ran.
func stepCallIndex(f *fakeSteps, prefix string) int {
	return slices.IndexFunc(f.calls, func(c []string) bool { return strings.HasPrefix(strings.Join(c, " "), prefix) })
}

// Codex's memories hold files: setup asks whether to import them, with
// their count and cost, before it asks whether to turn them off.
func TestSetupAsksToImportCodexMemories(t *testing.T) {
	codexWithMemories(t)
	code, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--questions", "--json")
	env := parseSetup(t, out)
	ids := setupQuestionIDs(env.Data.Questions)
	i, j := slices.Index(ids, "codex_import"), slices.Index(ids, "codex_memories")
	if code != output.OK || i < 0 || j != i+1 {
		t.Fatalf("exit %d, questions %v", code, ids)
	}
	q := env.Data.Questions[i]
	if q.Default != "no" || q.Recommended != "yes" || !strings.Contains(q.Prompt, "2 files") || q.Options[0].Cost == "" {
		t.Errorf("codex_import = %+v", q)
	}
	// Claude Code alone: Codex's memories are not its business.
	_, out, _ = run(t, "setup", "--answer", "provider=deepseek", "--answer", "tools=claude-code", "--questions", "--json")
	if ids := setupQuestionIDs(parseSetup(t, out).Data.Questions); slices.Contains(ids, "codex_import") {
		t.Errorf("tools=claude-code asked %v", ids)
	}
}

func TestSetupDoesNotAskWithNothingToImport(t *testing.T) {
	t.Run("no store", func(t *testing.T) {
		home, _ := setupMachine(t)
		withCodex(t, home).memories = true
		codexHomeWith(t, home, codexStart)
		_, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--questions", "--json")
		if ids := setupQuestionIDs(parseSetup(t, out).Data.Questions); slices.Contains(ids, "codex_import") || !slices.Contains(ids, "codex_memories") {
			t.Errorf("questions %v", ids)
		}
	})
	t.Run("every file imported already", func(t *testing.T) {
		home, dir, _, _ := codexWithMemories(t)
		d := pathsOf(home)
		id, err := retain.MachineID(d.State)
		if err != nil {
			t.Fatal(err)
		}
		scan, err := fileimport.ScanCodex(dir, fileimport.CodexOptions{})
		if err != nil || len(scan.Files) != 2 {
			t.Fatalf("scan %+v %v", scan, err)
		}
		m := fileimport.Manifest{Schema: 1, Source: fileimport.PresetSourceID(id, fileimport.PresetCodex, scan.Root), Root: scan.Root, Project: "transverse", Preset: fileimport.PresetCodex}
		m.Merge(scan, nil)
		for i := range m.Entries {
			m.Entries[i].Sent = true
		}
		if err := fileimport.SaveManifest(d.ImportDir(), m); err != nil {
			t.Fatal(err)
		}
		_, out, _ := run(t, "setup", "--answer", "provider=deepseek", "--questions", "--json")
		if ids := setupQuestionIDs(parseSetup(t, out).Data.Questions); slices.Contains(ids, "codex_import") {
			t.Errorf("asked to import what is sent already: %v", ids)
		}
	})
}

func TestSetupPlansTheCodexImportBeforeTurningMemoriesOff(t *testing.T) {
	_, dir, steps, f := codexWithMemories(t)
	code, out, _ := run(t, "setup", "--plan", "--answer", "provider=deepseek", "--answer", "tools=codex", "--answer", "codex_import=yes", "--json")
	env := parseSetup(t, out)
	var names []string
	for _, s := range env.Data.Steps {
		names = append(names, s.Name)
	}
	i := slices.Index(names, "codex_import")
	by := stepsByName(env.Data.Steps)
	if code != output.OK || i < 0 || names[i-1] != "import" || names[i+1] != "codex_memories" {
		t.Fatalf("exit %d, steps %v", code, names)
	}
	if st := by["codex_import"]; st.Status != "planned" || !strings.Contains(st.Detail, "--from codex --codex-home "+dir) || !strings.Contains(st.Detail, "2 file(s)") {
		t.Errorf("codex_import = %+v", st)
	}
	if stepCallIndex(steps, "import") >= 0 || len(f.mutating()) != 0 {
		t.Errorf("a plan ran %v, codex %v", steps.calls, f.mutating())
	}
	// Not answered yes: skipped, said why.
	_, out, _ = run(t, "setup", "--plan", "--answer", "provider=deepseek", "--answer", "tools=codex", "--answer", "codex_import=no", "--json")
	if st := stepsByName(parseSetup(t, out).Data.Steps)["codex_import"]; st.Status != "skipped" || st.Detail != "not asked for" {
		t.Errorf("codex_import=no: %+v", st)
	}
}

func TestSetupImportsCodexMemoriesBeforeTurningThemOff(t *testing.T) {
	home, dir, steps, _ := codexWithMemories(t)
	steps.real["client install"], steps.real["client codex-memories"] = true, true
	code, out, errOut := run(t, "setup", "--answer", "provider=deepseek", "--answer", "tools=codex", "--answer", "codex_import=yes", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	imp, off := stepCallIndex(steps, "import --from codex --codex-home "+dir), stepCallIndex(steps, "client codex-memories off")
	if imp < 0 || off < 0 || imp > off {
		t.Errorf("import at %d, memories off at %d: %v", imp, off, steps.calls)
	}
	by := stepsByName(env.Data.Steps)
	if by["codex_import"].Status != "done" || by["codex_memories"].Status != "done" {
		t.Errorf("steps %+v %+v", by["codex_import"], by["codex_memories"])
	}
	if !slices.ContainsFunc(env.Data.NextSteps, func(s string) bool { return strings.Contains(s, "Codex's own memories are being imported") }) {
		t.Errorf("next steps %v", env.Data.NextSteps)
	}
	_ = home
}

// --non-interactive with no answer takes the default: nothing imported,
// nothing spent.
func TestSetupNonInteractiveImportsNothingUnasked(t *testing.T) {
	_, _, steps, _ := codexWithMemories(t)
	steps.real["client install"], steps.real["client codex-memories"] = true, true
	code, out, errOut := run(t, "setup", "--answer", "provider=deepseek", "--answer", "tools=codex", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if stepCallIndex(steps, "import") >= 0 {
		t.Errorf("imported unasked: %v", steps.calls)
	}
	if st := stepsByName(env.Data.Steps)["codex_import"]; st.Status != "skipped" {
		t.Errorf("codex_import = %+v", st)
	}
	if !slices.ContainsFunc(env.Data.Questions, func(q setup.Question) bool { return q.ID == "codex_import" && q.Value == "no" }) {
		t.Errorf("questions %+v", env.Data.Questions)
	}
}
