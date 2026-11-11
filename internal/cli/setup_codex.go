package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

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
	return insertSteps(steps, install, memories)
}

// insertSteps puts install after the retention step and memories after
// the import step (the import comes first, so Codex's memory files can be
// imported before they are turned off).
func insertSteps(steps []setupStep, install, memories setupStep) []setupStep {
	at := func(name string) int {
		return slices.IndexFunc(steps, func(st setupStep) bool { return st.name == name }) + 1
	}
	steps = slices.Insert(steps, at("retention"), install)
	return slices.Insert(steps, at("import"), memories)
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
	return slices.Insert(steps, at("import"), memories)
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
