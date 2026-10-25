package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/claudecfg"
	"github.com/Barben360/backsight/internal/claudecode"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/reference"
	"github.com/Barben360/backsight/internal/setup"
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
	edits, err := retireEdits(c.loc)
	if err != nil {
		return res, editError(err)
	}
	res.Kept = []string{"the reference server and its data: backsight doctor says when it can be stopped",
		"files under ~/.claude/hooks and ~/.claude/skills, and your CLAUDE.md: personal files, never edited",
		"close or restart every Claude Code session: a running one may write the old MCP servers back (backsight doctor reports them)"}
	timerOn := retroTimerOwned(ctx, c.loc)
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

// retroTimerOwned reports whether retiring this profile stops the
// reference retro timer: it is machine-wide and reads the default
// profile's transcripts, so only a setup of ~/.claude may stop it.
func retroTimerOwned(ctx context.Context, loc claudecode.Location) bool {
	if schedulerGOOS != "linux" {
		return false
	}
	// The timer is this profile's only if its service runs this profile's
	// retro script: a HOME moved for a test install, or a copied profile,
	// is not where the real timer runs from (found by the agent-driven
	// install run, block 4 review).
	out, err := sysRunner.Run(ctx, "systemctl", "--user", "show", "-p", "ExecStart", strings.TrimSuffix(reference.RetroTimer, ".timer")+".service")
	if err != nil || !strings.Contains(string(out), " "+filepath.Join(filepath.Clean(loc.Dir), "hooks", "memory-retro.py")+" ") {
		return false
	}
	return retroTimerEnabled(ctx)
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

// retireEdits are the edits that unwire the reference client pieces from
// the profile at loc.
func retireEdits(loc claudecode.Location) ([]claudecfg.Edit, error) {
	names, err := referenceServers(loc.State)
	if err != nil {
		return nil, err
	}
	return []claudecfg.Edit{{Target: loc.Settings, Apply: retireSettings(names)}, {Target: loc.State, Apply: retireMCP}}, nil
}

// referenceServers names the reference MCP servers in ~/.claude.json, at
// the top level and in every project; their permission rules go with them.
func referenceServers(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		MCPServers map[string]any `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]any `json:"mcpServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	scan := func(m map[string]any) {
		for name, v := range m {
			if isReferenceServer(v) {
				seen[name] = true
			}
		}
	}
	scan(doc.MCPServers)
	for _, p := range doc.Projects {
		scan(p.MCPServers)
	}
	return setup.Keys(boolKeys(seen)), nil
}

func boolKeys(m map[string]bool) map[string]string {
	out := map[string]string{}
	for k := range m {
		out[k] = ""
	}
	return out
}

// toolRuleOf reports whether a permission rule names a tool of server:
// "mcp__<server>" or "mcp__<server>__<tool>".
func toolRuleOf(rule, server string) bool {
	return rule == "mcp__"+server || strings.HasPrefix(rule, "mcp__"+server+"__")
}

// retireSettings removes the reference hook entries, and the permission
// rules of the reference MCP servers being removed (servers). A hook group
// holding only reference commands goes whole (its bytes untouched around
// it); a group mixing them with others is rewritten without them; an event
// left empty goes.
func retireSettings(servers []string) func([]byte) ([]byte, []claudecfg.Change, error) {
	return func(doc []byte) ([]byte, []claudecfg.Change, error) {
		return retireSettingsDoc(doc, servers)
	}
}

func retireSettingsDoc(doc []byte, servers []string) ([]byte, []claudecfg.Change, error) {
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
		if !ok {
			return false
		}
		for _, srv := range servers {
			if toolRuleOf(s, srv) {
				return true
			}
		}
		return false
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

func isReferenceServer(v any) bool {
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
}

// retireMCP removes the reference MCP servers from ~/.claude.json, at the
// top level and in each project.
func retireMCP(doc []byte) ([]byte, []claudecfg.Change, error) {
	out, changes, err := claudecfg.RemoveWhere(doc, []string{"mcpServers"}, func(_ string, v any) bool { return isReferenceServer(v) })
	if err != nil {
		return nil, nil, err
	}
	for _, p := range projectKeys(out) {
		var ch []claudecfg.Change
		if out, ch, err = claudecfg.RemoveWhere(out, []string{"projects", p, "mcpServers"}, func(_ string, v any) bool { return isReferenceServer(v) }); err != nil {
			return nil, nil, err
		}
		changes = append(changes, ch...)
	}
	return out, changes, nil
}

func projectKeys(doc []byte) []string {
	v, ok, err := claudecfg.Get(doc, []string{"projects"})
	m, isObj := v.(map[string]any)
	if err != nil || !ok || !isObj {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k, p := range m {
		if pm, ok := p.(map[string]any); ok && pm["mcpServers"] != nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// The inverse edits undo uses (decision 0064 amendment): Claude Code
// rewrites ~/.claude.json at every start and settings.json on many
// changes, so putting back a whole pre-image would be refused, or with
// --force would drop everything written since. Each inverse re-applies
// only what its run removed or added, read from the run's pre-image.

// unretireSettings puts back the reference hook entries pre held that doc
// lacks (each as a group of its own with its original matcher, appended to
// its event), and pre's permission rules for the servers retired.
func unretireSettings(pre []byte, servers []string) func([]byte) ([]byte, []claudecfg.Change, error) {
	return func(doc []byte) ([]byte, []claudecfg.Change, error) {
		var changes []claudecfg.Change
		v, _, err := claudecfg.Get(pre, []string{"hooks"})
		if err != nil {
			return nil, nil, err
		}
		hooks, _ := v.(map[string]any)
		events := make([]string, 0, len(hooks))
		for ev := range hooks {
			events = append(events, ev)
		}
		sort.Strings(events)
		for _, ev := range events {
			groups, _ := hooks[ev].([]any)
			for _, g := range groups {
				gm, _ := g.(map[string]any)
				list, _ := gm["hooks"].([]any)
				for _, h := range list {
					hm, _ := h.(map[string]any)
					cmd, _ := hm["command"].(string)
					if !reference.HookCommand(cmd) || hasHookCommand(doc, ev, cmd) {
						continue
					}
					group := map[string]any{"hooks": []any{h}}
					if m, ok := gm["matcher"]; ok {
						group["matcher"] = m
					}
					var ch claudecfg.Change
					if doc, ch, err = claudecfg.EnsureInArray(doc, []string{"hooks", ev}, group); err != nil {
						return nil, nil, err
					}
					changes = append(changes, ch)
				}
			}
		}
		v, _, err = claudecfg.Get(pre, []string{"permissions", "allow"})
		if err != nil {
			return nil, nil, err
		}
		allow, _ := v.([]any)
		for _, r := range allow {
			rule, _ := r.(string)
			ref := false
			for _, srv := range servers {
				ref = ref || toolRuleOf(rule, srv)
			}
			if !ref {
				continue
			}
			var ch claudecfg.Change
			if doc, ch, err = claudecfg.EnsureInArray(doc, []string{"permissions", "allow"}, rule); err != nil {
				return nil, nil, err
			}
			if ch.New != nil {
				changes = append(changes, ch)
			}
		}
		return doc, changes, nil
	}
}

func hasHookCommand(doc []byte, event, cmd string) bool {
	v, ok, err := claudecfg.Get(doc, []string{"hooks", event})
	groups, isArr := v.([]any)
	if err != nil || !ok || !isArr {
		return false
	}
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		list, _ := gm["hooks"].([]any)
		for _, h := range list {
			if hm, _ := h.(map[string]any); hm["command"] == cmd {
				return true
			}
		}
	}
	return false
}

// unretireMCP puts back the reference MCP servers pre held that doc
// lacks, at the top level and in projects doc still has.
func unretireMCP(pre []byte) func([]byte) ([]byte, []claudecfg.Change, error) {
	return func(doc []byte) ([]byte, []claudecfg.Change, error) {
		var changes []claudecfg.Change
		put := func(base []string) error {
			v, _, err := claudecfg.Get(pre, base)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			names := make([]string, 0, len(m))
			for n := range m {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				if !isReferenceServer(m[n]) {
					continue
				}
				if _, there, _ := claudecfg.Get(doc, append(append([]string{}, base...), n)); there {
					continue
				}
				var ch claudecfg.Change
				if doc, ch, err = claudecfg.Set(doc, append(append([]string{}, base...), n), m[n]); err != nil {
					return err
				}
				changes = append(changes, ch) // reported as paths only (changedPaths)
			}
			return nil
		}
		if err := put([]string{"mcpServers"}); err != nil {
			return nil, nil, err
		}
		for _, p := range projectKeys(pre) {
			if _, there, _ := claudecfg.Get(doc, []string{"projects", p}); !there {
				continue // a project Claude Code dropped is not recreated
			}
			if err := put([]string{"projects", p, "mcpServers"}); err != nil {
				return nil, nil, err
			}
		}
		return doc, changes, nil
	}
}

// removeAdded removes from settings.json's allow list the memory-tool
// rules pre lacked (the inverse of client install's one edit).
func removeAdded(pre []byte) func([]byte) ([]byte, []claudecfg.Change, error) {
	had := map[string]bool{}
	if v, _, _ := claudecfg.Get(pre, []string{"permissions", "allow"}); v != nil {
		for _, r := range v.([]any) {
			if s, ok := r.(string); ok {
				had[s] = true
			}
		}
	}
	return func(doc []byte) ([]byte, []claudecfg.Change, error) {
		return claudecfg.RemoveWhere(doc, []string{"permissions", "allow"}, func(_ string, v any) bool {
			s, _ := v.(string)
			return slices.Contains(plugin.ToolNames, s) && !had[s]
		})
	}
}

// restoreKey puts key back to its value in pre, or removes it when pre
// lacked it (the inverse of client retention's edit).
func restoreKey(pre []byte, key string) func([]byte) ([]byte, []claudecfg.Change, error) {
	v, had, _ := claudecfg.Get(pre, []string{key})
	return func(doc []byte) ([]byte, []claudecfg.Change, error) {
		if had {
			out, ch, err := claudecfg.Set(doc, []string{key}, v)
			return out, []claudecfg.Change{ch}, err
		}
		return claudecfg.RemoveWhere(doc, nil, func(k string, _ any) bool { return k == key })
	}
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
	undoTimers := ""
	if !r.Plan && len(r.Timers) > 0 {
		undoTimers = " and systemctl --user enable --now " + strings.Join(r.Timers, " ")
	}
	if n == 0 && len(r.Timers) == 0 {
		b.WriteString("no reference piece wired into Claude Code\n")
	}
	for _, k := range r.Kept {
		b.WriteString("kept: " + k + "\n")
	}
	if r.Run != "" {
		fmt.Fprintf(&b, "undo with backsight client config restore %s%s\n", r.Run, undoTimers)
	}
	return b.String()
}
