// Package reference discovers the reference install (the author's Python
// hooks, the Hindsight npm plugin and their config) that backsight replaces
// milestone by milestone (SPEC §11.5, research C13). It only reads files.
package reference

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Barben360/backsight/internal/claudecode"
)

// Secret is a string that never prints.
type Secret string

func (Secret) Format(f fmt.State, _ rune)   { _, _ = fmt.Fprint(f, "[redacted]") }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Piece is one reference component wired into Claude Code.
type Piece struct {
	ID         string `json:"id"`
	Where      string `json:"where"`
	ReplacedIn string `json:"replaced_in"` // roadmap milestone
}

// Install is what discovery found. URL, Bank and Token come from the npm
// plugin config and are empty when it is missing.
type Install struct {
	ConfigPath        string  `json:"config_path,omitempty"`
	URL               string  `json:"url,omitempty"`
	Bank              string  `json:"bank,omitempty"`
	Token             Secret  `json:"token,omitempty"`
	ObservationScopes string  `json:"observation_scopes,omitempty"`
	Pieces            []Piece `json:"pieces"`
	// Leftovers are reference files nothing runs any more (the retro
	// script and timer once the cut-over disabled the timer).
	Leftovers []Piece `json:"leftovers,omitempty"`
}

// hookRules classify hook commands; the first match wins per rule, all
// rules are tried so one command can be two pieces (the Python hook wraps
// the plugin's SessionStart script).
var hookRules = []struct{ needle, id string }{
	{"coding-agents/dist/", "npm-plugin-hook"},
	{"memory-search.py", "python-search-hook"},
	{"hindsight-multibank", "legacy-multibank-hook"},
	{"memory-retro.py", "python-retro"},
}

var mcpRules = []struct{ needle, id string }{
	{"coding-agents/dist/mcp-server.js", "npm-plugin-mcp"},
	{"hindsight-memory-mcp.py", "python-memory-mcp"},
}

// cutOver is the roadmap milestone that retires every reference piece:
// until then the reference stack stays the daily memory (ROADMAP v7).
const cutOver = "M4"

// Discover inspects home and Claude Code's files at loc. It returns nil
// when no reference piece exists.
func Discover(home string, loc claudecode.Location) (*Install, error) {
	in := &Install{}
	found := false

	cfgPath := filepath.Join(home, ".hindsight", "coding-agent.json")
	var cfg struct {
		APIURL            string `json:"apiUrl"`
		APIToken          string `json:"apiToken"`
		BankID            string `json:"bankId"`
		ObservationScopes any    `json:"observationScopes"`
	}
	if ok, err := readJSON(cfgPath, &cfg); err != nil {
		return nil, err
	} else if ok {
		found = true
		in.ConfigPath, in.URL, in.Bank, in.Token = cfgPath, cfg.APIURL, cfg.BankID, Secret(cfg.APIToken)
		if cfg.ObservationScopes != nil {
			in.ObservationScopes = fmt.Sprint(cfg.ObservationScopes)
		}
	}

	seen := map[string]bool{}
	add := func(p Piece) {
		if k := p.ID + "|" + p.Where; !seen[k] {
			seen[k] = true
			in.Pieces = append(in.Pieces, p)
		}
	}

	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if _, err := readJSON(loc.Settings, &settings); err != nil {
		return nil, err
	}
	for event, groups := range settings.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				for _, r := range hookRules {
					if strings.Contains(h.Command, r.needle) {
						add(Piece{r.id, "settings.json " + event, cutOver})
					}
				}
			}
		}
	}

	type servers map[string]struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	var claudeJSON struct {
		MCPServers servers `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers servers `json:"mcpServers"`
		} `json:"projects"`
	}
	if _, err := readJSON(loc.State, &claudeJSON); err != nil {
		return nil, err
	}
	scan := func(where string, m servers) {
		for name, s := range m {
			line := s.Command + " " + strings.Join(s.Args, " ")
			for _, r := range mcpRules {
				if strings.Contains(line, r.needle) {
					add(Piece{r.id, where + name, cutOver})
				}
			}
		}
	}
	// client retire removes them from every project too.
	scan(".claude.json mcpServers.", claudeJSON.MCPServers)
	for p, proj := range claudeJSON.Projects {
		scan(".claude.json projects."+p+".mcpServers.", proj.MCPServers)
	}

	// The retro script and timer are wired only while the timer is enabled
	// (a *.wants link) or a hook entry runs the script. The timer is this
	// profile's only when its service runs this profile's script, as client
	// retire decides; an enable link left to a deleted unit is a leftover.
	userUnits := filepath.Join(home, ".config", "systemd", "user")
	_, timerErr := os.Stat(filepath.Join(userUnits, RetroTimer))
	wants, _ := filepath.Glob(filepath.Join(userUnits, "*.wants", RetroTimer))
	ours := retroServiceRuns(home, filepath.Join(userUnits, strings.TrimSuffix(RetroTimer, ".timer")+".service"),
		filepath.Join(loc.Dir, "hooks", "memory-retro.py"))
	timerOn := timerErr == nil && len(wants) > 0 && ours
	hookRuns := false
	for _, p := range in.Pieces {
		hookRuns = hookRuns || p.ID == "python-retro"
	}
	if _, err := os.Stat(filepath.Join(loc.Dir, "hooks", "memory-retro.py")); err == nil {
		p := Piece{"python-retro", ".claude/hooks/memory-retro.py", cutOver}
		if timerOn || hookRuns {
			add(p)
		} else {
			p.Where += ", not run"
			in.Leftovers = append(in.Leftovers, p)
		}
	}
	switch {
	case timerOn:
		add(Piece{"python-retro-timer", "systemd " + RetroTimer, cutOver})
	case timerErr == nil && ours:
		in.Leftovers = append(in.Leftovers, Piece{"python-retro-timer", "systemd " + RetroTimer + ", disabled", cutOver})
	case errors.Is(timerErr, fs.ErrNotExist) && len(wants) > 0:
		in.Leftovers = append(in.Leftovers, Piece{"python-retro-timer", "systemd " + RetroTimer + ", enable link to a deleted unit: " + wants[0], cutOver})
	}

	if !found && len(in.Pieces) == 0 && len(in.Leftovers) == 0 {
		return nil, nil
	}
	sort.Slice(in.Pieces, func(i, j int) bool {
		a, b := in.Pieces[i], in.Pieces[j]
		return a.ID+a.Where < b.ID+b.Where
	})
	return in, nil
}

// retroServiceRuns reports whether the unit file at path starts script
// (%h stands for home, as systemd expands it).
func retroServiceRuns(home, path, script string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(raw), "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(l), "ExecStart=")
		if !ok {
			continue
		}
		for _, arg := range strings.Fields(strings.ReplaceAll(v, "%h", home)) {
			if filepath.Clean(arg) == filepath.Clean(script) {
				return true
			}
		}
	}
	return false
}

// readJSON decodes path into out; a missing file is (false, nil).
func readJSON(path string, out any) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

// HookCommand reports whether a settings.json hook command belongs to the
// reference install (SPEC §11.5).
func HookCommand(cmd string) bool {
	for _, r := range hookRules {
		if strings.Contains(cmd, r.needle) {
			return true
		}
	}
	return false
}

// MCPServer reports whether an mcpServers entry (command and arguments,
// space-joined) is a reference MCP server.
func MCPServer(line string) bool {
	for _, r := range mcpRules {
		if strings.Contains(line, r.needle) {
			return true
		}
	}
	return false
}

// RetroTimer is the reference retro's systemd user timer.
const RetroTimer = "memory-retro.timer"
