package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
)

// Bank profile and the config keys backsight owns on it (decision 0049).
// Each owned key is replaced whole (a user's extra strategy or label under
// retain_strategies or entity_labels does not survive a send); every other
// key on the bank is left as it is.
const (
	bankName    = "backsight memory"
	bankMission = "Write every fact in English. Long-term memory of one developer's work with Claude Code across projects: " +
		"decisions and their reasons, corrections, conventions, pitfalls and fixes, and what was delivered."
)

const rules = `Write every fact in English, whatever the language of the source. Keep identifiers, file paths, commands, versions, URLs, issue and pull request numbers, and quoted user wording exactly as written.
Extract only durable knowledge a coding agent will need in later sessions: decisions with their reasons, corrections of earlier facts, conventions and preferences, pitfalls with their cause and fix, delivered states (what was shipped, merged or configured, and where), and lessons.
Do not extract plans, hypotheses, guesses, intermediate debugging steps, abandoned approaches, or anything the source later contradicts; keep only the final outcome.
One fact per statement, self-contained: name the project, component or tool it is about.
Use absolute dates (YYYY-MM-DD) taken from the source timestamps, never relative ones such as yesterday or last week.
Never record secrets, tokens, passwords or keys, even partially.
Label each fact with its kind.`

const conversationRules = rules + `
The source is a coding session between a developer and an AI coding agent: what the developer asked, approved or corrected carries more weight than what the agent proposed.`

const documentRules = rules + `
The source is a note someone chose to store: keep every durable statement in it, with its reason.`

// subagentRules: a report is not a note someone chose to keep, and a bare
// status ("monitoring CI") is no fact (decision 0071 C5, E30 V5).
const subagentRules = rules + `
The source is the final report of a sub-agent. A report that only says work is in progress, being monitored or waiting, with no finding, cause, decision or delivered result, holds no fact. Keep findings with their cause, decisions with their reason, and what was delivered and where.`

var kindValues = [][2]string{
	{"decision", "A choice that was made, with its reason."},
	{"correction", "A fact that replaces or invalidates an earlier one."},
	{"convention", "A rule, preference or standard to follow."},
	{"pitfall", "Something that broke or misled, with its cause and fix."},
	{"state", "What was delivered, configured or merged, and where."},
	{"lesson", "A generalised insight to reuse elsewhere."},
}

func bankTemplate() map[string]any {
	values := make([]map[string]string, len(kindValues))
	for i, v := range kindValues {
		values[i] = map[string]string{"value": v[0], "description": v[1]}
	}
	strategy := func(instructions string) map[string]any {
		return map[string]any{"retain_extraction_mode": "custom", "retain_custom_instructions": instructions, "retain_chunk_size": 30000}
	}
	return map[string]any{
		"retain_mission":          bankMission,
		"retain_default_strategy": "conversation",
		"retain_strategies": map[string]any{"conversation": strategy(conversationRules), "document": strategy(documentRules),
			"subagent": strategy(subagentRules)},
		"entity_labels": []map[string]any{{"key": "kind", "tag": true, "type": "multi-values", "optional": true,
			"description": "What kind of knowledge the fact is.", "values": values}},
		"entities_allow_free_form": true,
		"enable_observations":      true,
		"observations_mission": "Write every observation in English. Consolidate durable knowledge per project: decisions with their reasons, " +
			"conventions, pitfalls and fixes, and the current state of what was delivered. When a newer fact corrects or supersedes an older one, " +
			"rewrite the observation to the newest truth and keep the date of the change.",
		"default_observation_scopes": [][]string{{"project:*"}},
	}
}

// TemplateHash identifies the template this binary applies, bank profile
// included.
func TemplateHash() string { return Current().Hash() }

// Template is a bank template as backsight sends it: the bank's profile
// and the config keys it owns. Applied records the one last sent, so a
// later binary can tell its own older template from a change made on the
// server (M5 review rules, "Bank template follows the binary").
type Template struct {
	Name    string         `json:"name"`
	Mission string         `json:"mission"`
	Config  map[string]any `json:"config"`
}

// Current is the template this binary applies.
func Current() Template {
	return Template{Name: bankName, Mission: bankMission, Config: bankTemplate()}
}

// Hash identifies t; a template read back from its record hashes the same
// as when it was sent (JSON keys sorted, numbers kept as written).
func (t Template) Hash() string {
	raw, _ := json.Marshal(map[string]any{"config": t.Config, "name": t.Name, "mission": t.Mission})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SaveApplied records this binary's template at path (0600), as the one
// the bank was given.
func SaveApplied(path string) error {
	raw, err := json.MarshalIndent(Current(), "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(raw, '\n'), 0o600)
}

// LoadApplied reads the template SaveApplied recorded; its Hash says which
// one it is.
func LoadApplied(path string) (Template, error) {
	var t Template
	raw, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return t, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// BankResult says what ApplyBank did.
type BankResult struct {
	Bank    string `json:"bank"`
	Created bool   `json:"created"`
	Sent    bool   `json:"sent"`
	Hash    string `json:"template_hash"`
	// Renamed: the legacy bank renamed first (bank rename), as "old → new";
	// RenameWarning: why it was not, the template then going to the old.
	Renamed       string `json:"renamed,omitempty"`
	RenameWarning string `json:"rename_warning,omitempty"`
}

// ApplyBank creates bank if missing and sets backsight's keys on it, unless
// appliedHash says this template is already there (force re-sends).
func ApplyBank(ctx context.Context, c *hindsight.Client, bank, appliedHash string, force bool) (BankResult, error) {
	res := BankResult{Bank: bank, Hash: TemplateHash()}
	banks, err := c.Banks(ctx)
	if err != nil {
		return res, err
	}
	exists := false
	for _, b := range banks {
		exists = exists || b.ID == bank
	}
	if exists && !force && appliedHash == res.Hash {
		return res, nil
	}
	// PUT creates the bank or refreshes its profile (name, mission).
	if err := c.CreateBank(ctx, bank, bankName, bankMission); err != nil {
		return res, err
	}
	res.Created = !exists
	tmpl := bankTemplate()
	if err := c.PatchBankConfig(ctx, bank, tmpl); err != nil {
		return res, err
	}
	res.Sent = true
	over, err := c.BankConfigOverrides(ctx, bank)
	if err != nil {
		return res, err
	}
	var missing []string
	for k := range tmpl {
		if _, ok := over[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return res, fmt.Errorf("bank %s did not keep %s: check the server's bank config API (HINDSIGHT_API_ENABLE_BANK_CONFIG_API)", bank, strings.Join(missing, ", "))
	}
	return res, nil
}

// BankDrift lists the template keys bank no longer holds as backsight set
// them (a hand edit in the UI, a server reset). A key matches when every
// value the template sets is there: the server may add its own defaults.
func BankDrift(ctx context.Context, c *hindsight.Client, bank string) ([]string, error) {
	return DriftFrom(ctx, c, bank, bankTemplate())
}

// DriftFrom is BankDrift against another template's config keys (an
// older one, from its record).
func DriftFrom(ctx context.Context, c *hindsight.Client, bank string, tmpl map[string]any) ([]string, error) {
	over, err := c.BankConfigOverrides(ctx, bank)
	if err != nil {
		return nil, err
	}
	var drift []string
	for k, want := range tmpl {
		raw, ok := over[k]
		var got any
		if !ok || json.Unmarshal(raw, &got) != nil || !holds(normal(want), got) {
			drift = append(drift, k)
		}
	}
	sort.Strings(drift)
	return drift, nil
}

// normal gives v the shape JSON decoding gives (maps, []any, float64).
func normal(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

// holds reports whether got has every value of want; objects may carry
// more keys, lists must match item by item.
func holds(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !holds(v, g[k]) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !holds(w[i], g[i]) {
				return false
			}
		}
		return true
	default:
		return want == got
	}
}
