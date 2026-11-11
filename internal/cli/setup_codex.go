package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fileimport"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/setup"
)

// Setup's Codex part (block 3, https://github.com/Barben360/backsight/issues/278;
// decision 0085 D1, D5, D10): it detects Codex, wires each coding tool
// the tools answer picks, and turns Codex's own memories off when
// answered, after the import step.

// claudeSteps are the steps that wire Claude Code alone: skipped when the
// tools answer leaves it out.
var claudeSteps = []string{"client", "retention", "verify", "reference", "backfill"}

// detectCodex finds Codex in the home setup acts on (--codex-home, the
// one recorded, the shell's): there, the tools question is asked. A
// missing codex, or a home that is not there, leaves Claude Code alone;
// a --codex-home that is not there is refused.
func (s *setupEnv) detectCodex(ctx context.Context) error {
	dir, err := codexHomeFor(s.dirs, s.home, s.f.codexHome)
	if err != nil {
		if s.f.codexHome != "" {
			return err
		}
		return nil
	}
	env := harnessEnv(codex, s.home, codexProfile(s.home, dir))
	if _, err := codex.Detect(ctx, env); err != nil {
		if s.f.codexHome != "" {
			return needsCodex("--codex-home names a Codex home, but the codex command is missing or failed: " + err.Error())
		}
		return nil
	}
	s.codexDir = dir
	_, err = claudeCode.Detect(ctx, harnessEnv(claudeCode, s.home, s.loc.Dir))
	s.claudeFound = err == nil
	s.codexMemories, _ = codex.MemoriesOn(ctx, env)
	return nil
}

// codexArgs are a Codex command's args, with --codex-home when setup was
// given one.
func (s *setupEnv) codexArgs(args ...string) []string {
	if s.f.codexHome != "" {
		args = append(args, "--codex-home", s.f.codexHome)
	}
	return args
}

// withTools fits steps to the tools answer: Claude Code's steps skipped
// when it is not picked, and with Codex, its step after Claude Code's
// retention and its memories step after the import.
func (s *setupEnv) withTools(steps []setupStep, answers map[string]string, withRuns func(*setup.Record, func() subResult) subResult) []setupStep {
	claude, codexOn := setup.Tools(answers)
	if !claude {
		for i, st := range steps {
			if slices.Contains(claudeSteps, st.name) {
				name := st.name
				steps[i].eta, steps[i].run = 0, func(context.Context, *setup.Record) (setup.Step, error) { return notPickedStep(name), nil }
			}
		}
	}
	if !codexOn {
		return steps
	}
	install := setupStep{"codex", 10, func(ctx context.Context, r *setup.Record) (setup.Step, error) {
		env := harnessEnv(codex, s.home, codexProfile(s.home, s.codexDir))
		// Undo removes the plugins only from a home they were not in before.
		if there, err := codex.Installed(ctx, env); err != nil || !there {
			r.Undo.CodexHomes = appendNew(r.Undo.CodexHomes, s.codexDir)
			s.persist(r)
		}
		out := subcommand(ctx, s.codexArgs("client", "install", "--codex")...)
		if out.Err != nil {
			return setup.Step{}, out.Err
		}
		var cr clientInstallResult
		_ = json.Unmarshal(out.Data, &cr)
		s.codexTrust = cr.Trust
		for _, st := range cr.Steps {
			s.codexAdded = s.codexAdded || (st.Name == "codex" && st.Status == "done" && strings.HasPrefix(st.Detail, "plugin "))
		}
		return setup.Step{Name: "codex", Status: "done", Detail: "plugins backsight@backsight and backsight-hooks@backsight in " + cr.Profile}, nil
	}}
	imp := setupStep{"codex_import", 2, func(ctx context.Context, r *setup.Record) (setup.Step, error) {
		if answers["codex_import"] != "yes" {
			return setup.Step{Name: "codex_import", Status: "skipped", Detail: codexImportSkipped(answers)}, nil
		}
		// An extra, as the folder import: what keeps it from being queued
		// is said, with the command, and setup goes on.
		cmd := "backsight import --from codex --codex-home " + s.codexDir
		args := []string{"import", "--from", "codex", "--codex-home", s.codexDir}
		if answers["provider"] == "claude-code" {
			args = append(args, "--allow-subscription") // answered yes past the caveat
		}
		out := subcommand(ctx, args...)
		if out.Err != nil {
			return setup.Step{Name: "codex_import", Status: "skipped", Detail: fmt.Sprintf("not queued: %s; run %s once that is fixed", strings.TrimSuffix(errMessage(out.Err), "."), cmd)}, nil
		}
		var ir filesImportResult
		_ = json.Unmarshal(out.Data, &ir)
		detail := fmt.Sprintf("%d file(s) of Codex's memories queued: jobs tick sends them in the background (backsight import list shows them)", ir.Plan.Files)
		if answers["scheduler"] == "off" {
			detail += "; with no scheduler, run backsight jobs tick to send them"
		}
		return setup.Step{Name: "codex_import", Status: "done", Detail: detail}, nil
	}}
	memories := setupStep{"codex_memories", 1, func(ctx context.Context, r *setup.Record) (setup.Step, error) {
		if answers["codex_memories"] != "off" {
			why := "Codex's own memories are off"
			if answers["codex_memories"] == "keep" {
				why = "kept on, as answered"
			}
			return setup.Step{Name: "codex_memories", Status: "skipped", Detail: why}, nil
		}
		out := withRuns(r, func() subResult { return subcommand(ctx, s.codexArgs("client", "codex-memories", "off")...) })
		if out.Err != nil {
			return setup.Step{}, out.Err
		}
		var cr clientConfigResult
		_ = json.Unmarshal(out.Data, &cr)
		detail := "turned off in Codex's config.toml (setup --undo turns them on again)"
		if len(cr.Notes) > 0 {
			detail += "; " + strings.Join(cr.Notes, "; ")
		}
		return setup.Step{Name: "codex_memories", Status: "done", Detail: detail}, nil
	}}
	return insertSteps(steps, install, imp, memories)
}

// insertSteps puts install after the retention step, then the import of
// Codex's memories and the memories step after the import step (the
// imports come first, so Codex's memory files are queued before they are
// turned off).
func insertSteps(steps []setupStep, install, imp, memories setupStep) []setupStep {
	at := func(name string) int {
		return slices.IndexFunc(steps, func(st setupStep) bool { return st.name == name }) + 1
	}
	steps = slices.Insert(steps, at("retention"), install)
	return slices.Insert(steps, at("import"), imp, memories)
}

// codexImportSkipped says why the import of Codex's memories is skipped.
func codexImportSkipped(answers map[string]string) string {
	if answers["codex_import"] == "no" {
		return "not asked for"
	}
	return "no Codex memory file to import"
}

// codexImportFor estimates the import of Codex's own memories in the home
// setup acts on, for a model; nil without Codex.
func (s *setupEnv) codexImportFor() func(model string) (int, string) {
	if s.codexDir == "" {
		return nil
	}
	return func(model string) (int, string) {
		if e, ok := s.codexImportEsts[model]; ok {
			return e.files, e.cost
		}
		e := s.scanCodexMemories(model)
		if s.codexImportEsts == nil {
			s.codexImportEsts = map[string]importEst{}
		}
		s.codexImportEsts[model] = e
		return e.files, e.cost
	}
}

// scanCodexMemories reads Codex's memory stores as `import --from codex`
// would, and prices what it would send: the files not sent already when
// this PC imported them before. It reads the files only, never the server.
func (s *setupEnv) scanCodexMemories(model string) importEst {
	cfg := config.Default(s.dirs, s.home)
	if s.cfg != nil {
		cfg = *s.cfg
	}
	if model != cfg.LLM.Model { // the configured prices are another model's
		cfg.LLM.Model, cfg.LLM.PriceInput, cfg.LLM.PriceCached, cfg.LLM.PriceOutput = model, 0, 0, 0
	}
	scan, err := scanCodexImport(s.home, s.codexDir, cfg.Projects.Transverse, cfg.Privacy.ExcludePaths, nil)
	if err != nil {
		return importEst{err: err}
	}
	var toSend func(fileimport.File) bool
	if id := retain.ReadMachineID(s.dirs.State); id != "" {
		if m, found, _ := fileimport.LoadManifest(s.dirs.ImportDir(), fileimport.PresetSourceID(id, fileimport.PresetCodex, scan.Root)); found {
			m.Entries = slices.Clone(m.Entries)
			m.Merge(scan, nil)
			unsent := map[string]bool{}
			for _, e := range m.Unsent() {
				unsent[e.Rel] = true
			}
			toSend = func(f fileimport.File) bool { return unsent[f.Rel] }
		}
	}
	return importEstOf(scan, toSend, cfg, model)
}

func notPickedStep(name string) setup.Step {
	return setup.Step{Name: name, Status: "skipped", Detail: "Claude Code is not among the tools wired (tools answer)"}
}

// planTools fits the planned steps to the tools answer, as withTools does.
func (s *setupEnv) planTools(steps []setup.Step, a map[string]string) []setup.Step {
	claude, codexOn := setup.Tools(a)
	if !claude {
		for i, st := range steps {
			if slices.Contains(claudeSteps, st.Name) {
				steps[i] = notPickedStep(st.Name)
			}
		}
	}
	if !codexOn {
		return steps
	}
	imp := setup.Step{Name: "codex_import", Status: "skipped", Detail: codexImportSkipped(a)}
	if a["codex_import"] == "yes" {
		imp = setup.Step{Name: "codex_import", Status: "planned", Detail: "backsight import --from codex --codex-home " + s.codexDir + ": queue Codex's memory files for jobs tick"}
		if n, cost := s.codexImportFor()(a["model"]); n > 0 {
			imp.Detail = fmt.Sprintf("backsight import --from codex --codex-home %s: queue %d file(s) for jobs tick (%s)", s.codexDir, n, cost)
		}
	}
	memories := setup.Step{Name: "codex_memories", Status: "skipped", Detail: "Codex's own memories are off"}
	switch a["codex_memories"] {
	case "off":
		memories = setup.Step{Name: "codex_memories", Status: "planned", Detail: "backsight client codex-memories off: memories = true removed from Codex's config.toml (a copy kept first)"}
	case "keep":
		memories.Detail = "kept on, as answered"
	}
	install := setup.Step{Name: "codex", Status: "planned", Detail: "backsight client install --codex: plugins backsight@backsight and backsight-hooks@backsight in " + s.codexDir + "; then trust the hooks in Codex"}
	at := func(name string) int {
		return slices.IndexFunc(steps, func(st setup.Step) bool { return st.Name == name }) + 1
	}
	steps = slices.Insert(steps, at("retention"), install)
	return slices.Insert(steps, at("import"), imp, memories)
}

// codexNextSteps are what a setup that wired Codex leaves the user to do.
func (s *setupEnv) codexNextSteps() []string {
	var out []string
	if s.codexAdded {
		out = append(out, "restart Codex so it loads backsight's plugins")
	}
	if s.codexTrust != "" {
		out = append(out, s.codexTrust)
	}
	return out
}
