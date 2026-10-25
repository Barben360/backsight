package cli

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/reference"
)

type retireResult struct {
	Files  []claudecfg.FileResult `json:"files"`
	Run    string                 `json:"run,omitempty"`
	Timers []string               `json:"timers,omitempty"` // reference timers stopped
	Kept   []string               `json:"kept,omitempty"`   // reference pieces left in place, and why
	Plan   bool                   `json:"plan"`
}

// newClientRetireCmd unwires the reference install's client pieces
// (SPEC §11.5, decision 0036): hook entries and permission rules in
// settings.json, MCP servers in ~/.claude.json, the retro timer. The old
// server and its data are not touched.
func newClientRetireCmd(e *env, configDir *string) *cobra.Command {
	var plan bool
	cmd := &cobra.Command{
		Use:   "retire",
		Short: "Unwire the reference Hindsight install from Claude Code (pre-images kept; client config restore undoes it)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := clientRetire(cmd.Context(), *configDir, plan)
			return e.report("client retire", res, func() string { return retireText(res) }, err)
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "show what would be removed without writing")
	return cmd
}

func clientRetire(ctx context.Context, configDir string, plan bool) (retireResult, error) {
	res := retireResult{Plan: plan, Files: []claudecfg.FileResult{}}
	c, err := loadClientEnv(configDir)
	if err != nil {
		return res, err
	}
	edits := []claudecfg.Edit{{Target: c.loc.Settings, Apply: retireSettings}, {Target: c.loc.State, Apply: retireMCP}}
	res.Kept = []string{"the reference server and its data: backsight doctor says when it can be stopped",
		"files under ~/.claude/hooks and ~/.claude/skills, and your CLAUDE.md: personal files, never edited"}
	timerOn := runtime.GOOS == "linux" && retroTimerEnabled(ctx)
	if plan {
		if res.Files, err = claudecfg.Plan(edits); err != nil {
			return res, editError(err)
		}
		res.Files = onlyChanged(res.Files)
		if timerOn {
			res.Timers = []string{reference.RetroTimer}
		}
		return res, nil
	}
	if res.Run, res.Files, err = c.runner("client retire").Apply(edits); err != nil {
		return res, editError(err)
	}
	res.Files = onlyChanged(res.Files)
	if timerOn {
		if out, err := sysRunner.Run(ctx, "systemctl", "--user", "disable", "--now", reference.RetroTimer); err != nil {
			return res, partialError(fmt.Errorf("systemctl --user disable --now %s: %w %s", reference.RetroTimer, err, strings.TrimSpace(string(out))))
		}
		res.Timers = []string{reference.RetroTimer}
	}
	return res, nil
}

func retroTimerEnabled(ctx context.Context) bool {
	out, err := sysRunner.Run(ctx, "systemctl", "--user", "is-enabled", reference.RetroTimer)
	return err == nil && strings.TrimSpace(string(out)) == "enabled"
}

// onlyChanged keeps the files that change, each change as its path only:
// a retired MCP entry may carry a token in a form redaction does not know.
func onlyChanged(fs []claudecfg.FileResult) []claudecfg.FileResult {
	out := []claudecfg.FileResult{}
	for _, f := range fs {
		if len(f.Changes) == 0 {
			continue
		}
		for i := range f.Changes {
			f.Changes[i].Old, f.Changes[i].New = nil, nil
		}
		out = append(out, f)
	}
	return out
}

// retireSettings removes the reference hook entries and permission rules.
// A hook group holding only reference commands goes whole (its bytes
// untouched around it); a group mixing them with others is rewritten
// without them; an event left empty goes.
func retireSettings(doc []byte) ([]byte, []claudecfg.Change, error) {
	var changes []claudecfg.Change
	v, ok, err := claudecfg.Get(doc, []string{"hooks"})
	if err != nil {
		return nil, nil, err
	}
	hooks, _ := v.(map[string]any)
	if ok && hooks != nil {
		events := make([]string, 0, len(hooks))
		for ev := range hooks {
			events = append(events, ev)
		}
		sort.Strings(events)
		for _, ev := range events {
			groups, _ := hooks[ev].([]any)
			var kept []any
			mixed := false
			for _, g := range groups {
				all, some, rest := splitGroup(g)
				switch {
				case all:
				case some:
					mixed = true
					kept = append(kept, rest)
				default:
					kept = append(kept, g)
				}
			}
			var ch []claudecfg.Change
			switch {
			case len(kept) == len(groups) && !mixed:
				continue
			case len(kept) == 0:
				doc, ch, err = claudecfg.RemoveWhere(doc, []string{"hooks"}, func(k string, _ any) bool { return k == ev })
			case mixed:
				var one claudecfg.Change
				doc, one, err = claudecfg.Set(doc, []string{"hooks", ev}, kept)
				ch = []claudecfg.Change{one}
			default:
				doc, ch, err = claudecfg.RemoveWhere(doc, []string{"hooks", ev}, func(_ string, g any) bool { all, _, _ := splitGroup(g); return all })
			}
			if err != nil {
				return nil, nil, err
			}
			changes = append(changes, ch...)
		}
	}
	doc, ch, err := claudecfg.RemoveWhere(doc, []string{"permissions", "allow"}, func(_ string, v any) bool {
		s, ok := v.(string)
		return ok && reference.Permission(s)
	})
	if err != nil {
		return nil, nil, err
	}
	return doc, append(changes, ch...), nil
}

// splitGroup classifies one hook group: every command is the reference's
// (all), some are (some, with rest the group without them), or none.
func splitGroup(g any) (all, some bool, rest map[string]any) {
	m, ok := g.(map[string]any)
	if !ok {
		return false, false, nil
	}
	list, _ := m["hooks"].([]any)
	var keep []any
	for _, h := range list {
		hm, _ := h.(map[string]any)
		if cmd, _ := hm["command"].(string); reference.HookCommand(cmd) {
			continue
		}
		keep = append(keep, h)
	}
	switch {
	case len(list) == 0 || len(keep) == len(list):
		return false, false, nil
	case len(keep) == 0:
		return true, false, nil
	}
	rest = map[string]any{}
	for k, v := range m {
		rest[k] = v
	}
	rest["hooks"] = keep
	return false, true, rest
}

// retireMCP removes the reference MCP servers from ~/.claude.json.
func retireMCP(doc []byte) ([]byte, []claudecfg.Change, error) {
	return claudecfg.RemoveWhere(doc, []string{"mcpServers"}, func(_ string, v any) bool {
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		line := fmt.Sprint(m["command"])
		if args, ok := m["args"].([]any); ok {
			for _, a := range args {
				line += " " + fmt.Sprint(a)
			}
		}
		return reference.MCPServer(line)
	})
}

func retireText(r retireResult) string {
	var b strings.Builder
	verb := map[bool]string{true: "would remove", false: "removed"}[r.Plan]
	n := 0
	for _, f := range r.Files {
		for _, ch := range f.Changes {
			fmt.Fprintf(&b, "%s %s: %s\n", verb, f.Target, ch.Path)
			n++
		}
	}
	for _, t := range r.Timers {
		fmt.Fprintf(&b, "%s systemd user timer %s\n", map[bool]string{true: "would stop", false: "stopped"}[r.Plan], t)
	}
	if n == 0 && len(r.Timers) == 0 {
		b.WriteString("no reference piece wired into Claude Code\n")
	}
	for _, k := range r.Kept {
		b.WriteString("kept: " + k + "\n")
	}
	if r.Run != "" {
		fmt.Fprintf(&b, "undo with backsight client config restore %s\n", r.Run)
	}
	return b.String()
}
