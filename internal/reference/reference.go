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

func replacedIn(id, event string) string {
	switch {
	case id == "python-retro":
		return "M3"
	case id == "npm-plugin-hook" && (event == "Stop" || event == "SessionEnd" || event == "SubagentStop"):
		return "M2"
	}
	return "M1"
}

// Discover inspects home. It returns nil when no reference piece exists.
func Discover(home string) (*Install, error) {
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
	if _, err := readJSON(filepath.Join(home, ".claude", "settings.json"), &settings); err != nil {
		return nil, err
	}
	for event, groups := range settings.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				for _, r := range hookRules {
					if strings.Contains(h.Command, r.needle) {
						add(Piece{r.id, "settings.json " + event, replacedIn(r.id, event)})
					}
				}
			}
		}
	}

	var claudeJSON struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if _, err := readJSON(filepath.Join(home, ".claude.json"), &claudeJSON); err != nil {
		return nil, err
	}
	for name, s := range claudeJSON.MCPServers {
		line := s.Command + " " + strings.Join(s.Args, " ")
		for _, r := range mcpRules {
			if strings.Contains(line, r.needle) {
				add(Piece{r.id, ".claude.json mcpServers." + name, "M1"})
			}
		}
	}

	if _, err := os.Stat(filepath.Join(home, ".claude", "hooks", "memory-retro.py")); err == nil {
		add(Piece{"python-retro", ".claude/hooks/memory-retro.py", "M3"})
	}

	if !found && len(in.Pieces) == 0 {
		return nil, nil
	}
	sort.Slice(in.Pieces, func(i, j int) bool {
		a, b := in.Pieces[i], in.Pieces[j]
		return a.ReplacedIn+a.ID+a.Where < b.ReplacedIn+b.ID+b.Where
	})
	return in, nil
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
