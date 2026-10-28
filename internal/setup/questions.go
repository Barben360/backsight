// Package setup holds the onboarding wizard's deterministic parts: its
// questions and how answers resolve them (decision 0064), its pre-flight
// checks (SPEC §11.3 step 1) and its run record. The command that drives
// the steps lives in package cli.
package setup

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/config"
)

// Option is one value a question accepts.
type Option struct {
	Value  string `json:"value"`
	Label  string `json:"label"`
	Cost   string `json:"cost,omitempty"`
	Caveat string `json:"caveat,omitempty"`
}

// Question is one choice setup needs (SPEC §11.2 question schema).
type Question struct {
	ID          string   `json:"id"`
	Prompt      string   `json:"prompt"`
	Options     []Option `json:"options,omitempty"`
	Free        bool     `json:"free_text,omitempty"` // values outside Options are accepted
	Default     string   `json:"default,omitempty"`
	Recommended string   `json:"recommended,omitempty"`
	// Required: --non-interactive does not take the default; the user
	// must answer (the provider decides which key is needed, consent is
	// the go).
	Required bool `json:"required,omitempty"`
	// Value is the resolved answer, "" while unanswered; Source says where
	// it came from: answer | saved | config | default.
	Value  string `json:"value,omitempty"`
	Source string `json:"source,omitempty"`
}

// Asked reports whether the user still has to answer q.
func (q Question) Asked() bool { return q.Value == "" && q.Source == "" }

// MarshalJSON adds "asked" to the schema.
func (q Question) MarshalJSON() ([]byte, error) {
	type plain Question
	return json.Marshal(struct {
		plain
		Asked bool `json:"asked"`
	}{plain(q), q.Asked()})
}

// Preset is a provider's model and what it costs and risks (SPEC §12).
type Preset struct {
	Provider, Model, BaseURL string
	Label, Cost, Caveat      string
}

// Presets are the providers setup offers, recommended first. Any other
// Hindsight provider id is accepted as free text (listed, untested).
var Presets = []Preset{
	{Provider: "deepseek", Model: "deepseek-flash", Label: "DeepSeek API", Cost: "the cheapest provider tested: $0.15 per million input tokens off-peak, twice that at peak hours",
		Caveat: "DeepSeek processes and stores data in the People's Republic of China (https://cdn.deepseek.com/policies/en-US/deepseek-privacy-policy.html)"},
	{Provider: "anthropic", Model: "claude-haiku-5-5", Label: "Anthropic API key, or an Anthropic-compatible URL"},
	{Provider: "openai", Model: "gpt-6-luna", Label: "OpenAI API key, or an OpenAI-compatible URL"},
	{Provider: "ollama", Model: "qwen3:8b", BaseURL: "http://host.docker.internal:11434", Label: "Ollama on this machine (private, lower quality)",
		Caveat: "an 8B model writes wordier facts, some only true for a while, and takes about 2 min per part of a session on CPU; 3B and smaller models fail"},
	{Provider: "claude-code", Model: "claude-haiku-5-5", Label: "Your Claude subscription (claude setup-token)",
		Caveat: "Anthropic's terms say developers \"may not collect, store, or intermediate Claude.ai credentials or session tokens\" " +
			"(https://code.claude.com/docs/en/legal-and-compliance); this lane stores your token for the server, and Anthropic may restrict an account without notice. " +
			"An Anthropic API key (provider anthropic) is recommended instead. Usage counts against your plan"},
}

// PresetFor returns provider's preset, false for a provider setup does not list.
func PresetFor(provider string) (Preset, bool) {
	for _, p := range Presets {
		if p.Provider == provider {
			return p, true
		}
	}
	return Preset{}, false
}

// RecommendedRetention keeps transcripts ten years (decision 0041).
const RecommendedRetention = "3650"

// Inputs are what resolves the questions, strongest first: an existing
// config (adoption, decision 0064), then answers, then answers saved by an
// earlier run, then defaults.
type Inputs struct {
	Config  *config.Config // nil: no config.toml yet
	Answers map[string]string
	Saved   map[string]string
	// ReferencePieces counts the reference pieces wired into Claude Code.
	ReferencePieces int
	// Backfill estimates the sessions backfill would send for a model and
	// the cost to show; 0 sessions: no backfill question.
	Backfill func(model string) (sessions int, cost string)
}

// Hidden answers have defaults and are never asked.
// role: "server" for a host that only serves the memory (setup --role).
var hidden = map[string]string{"api_port": "8888", "ui_port": "9999", "project": "backsight", "scheduler": "on", "role": config.RoleBoth}

// known lists every answer id setup accepts.
var known = []string{"provider", "model", "base_url", "ack_subscription", "retention", "backups", "reference", "backfill", "consent", "api_port", "ui_port", "project", "scheduler", "role"}

// UsageError is an answer setup cannot take; the command exits 2.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Resolve builds the questions that apply and resolves each from in. The
// hidden answers come back in the map.
func Resolve(in Inputs) ([]Question, map[string]string, error) {
	for k := range in.Answers {
		if !slices.Contains(known, k) {
			return nil, nil, &UsageError{fmt.Sprintf("unknown answer %q: setup knows %s", k, strings.Join(known, ", "))}
		}
	}
	fromConfig := configAnswers(in.Config)
	extra := map[string]string{}
	for k, def := range hidden {
		v := def
		if c, ok := fromConfig[k]; ok {
			if a, given := in.Answers[k]; given && a != c {
				return nil, nil, &UsageError{fmt.Sprintf("config.toml already sets %s to %q (answer %q): edit %s in config.toml, or drop the answer", k, c, a, configKey[k])}
			}
			v = c
		} else if a, ok := in.Answers[k]; ok {
			v = a
		} else if s := in.Saved[k]; s != "" {
			v = s
		}
		if k == "scheduler" {
			if v != "on" && v != "off" {
				return nil, nil, &UsageError{fmt.Sprintf("scheduler=%s: on or off", v)}
			}
		} else if k == "role" {
			if v != config.RoleBoth && v != config.RoleServer {
				return nil, nil, &UsageError{fmt.Sprintf("role=%s: %s or %s", v, config.RoleBoth, config.RoleServer)}
			}
		} else if k == "project" {
			if !projectRE.MatchString(v) {
				return nil, nil, &UsageError{fmt.Sprintf("project=%s: lowercase letters, digits, - and _", v)}
			}
		} else if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 65535 {
			return nil, nil, &UsageError{fmt.Sprintf("%s=%s is not a port", k, v)}
		}
		extra[k] = v
	}
	if extra["api_port"] == extra["ui_port"] {
		return nil, nil, &UsageError{"api_port and ui_port must differ"}
	}
	// A server-only host runs no Claude Code: nothing of it to ask about.
	serverOnly := extra["role"] == config.RoleServer
	// A client extracts nothing and backs nothing up: its [llm] only
	// prices estimates, its provider and model come from config.toml
	// (connect writes the host's), and backups are the host's.
	remote := in.Config != nil && in.Config.Server.Mode == "remote"
	pick := func(q *Question) error {
		ans, given := in.Answers[q.ID]
		if given && ans == "" && q.ID != "base_url" { // empty is no answer (only a base URL may be empty)
			given = false
		}
		if c, ok := fromConfig[q.ID]; ok {
			if given && ans != c {
				return &UsageError{fmt.Sprintf("config.toml already sets %s to %q (answer %q): edit %s in config.toml, or drop the answer", q.ID, c, ans, configKey[q.ID])}
			}
			q.Value, q.Source = c, "config"
			return nil
		}
		switch {
		case given:
			q.Value, q.Source = ans, "answer"
		case in.Saved[q.ID] != "":
			q.Value, q.Source = in.Saved[q.ID], "saved"
		}
		return nil
	}
	var qs []Question
	add := func(q Question) error {
		if err := pick(&q); err != nil {
			return err
		}
		if q.Value != "" && !q.Free && !q.accepts(q.Value) {
			return &UsageError{fmt.Sprintf("%s=%s is not one of %s", q.ID, q.Value, strings.Join(q.values(), ", "))}
		}
		qs = append(qs, q)
		return nil
	}

	pq := Question{ID: "provider", Prompt: "Which LLM should the memory server use to extract facts from your sessions?",
		Free: true, Default: "deepseek", Recommended: "deepseek", Required: true}
	for _, p := range Presets {
		pq.Options = append(pq.Options, Option{Value: p.Provider, Label: p.Label, Cost: p.Cost, Caveat: p.Caveat})
	}
	if err := add(pq); err != nil {
		return nil, nil, err
	}
	provider := qs[0].Value
	if saved := in.Saved["provider"]; saved != "" && saved != orDefault(provider, "deepseek") {
		// The provider changed: the earlier model, URL and acknowledgement
		// belonged to the old one.
		in.Saved = maps.Clone(in.Saved)
		for _, k := range []string{"model", "base_url", "ack_subscription"} {
			delete(in.Saved, k)
		}
	}
	preset, listed := PresetFor(provider)
	provider = orDefault(provider, "deepseek")
	if qs[0].Value == "" {
		preset, listed = Presets[0], true
	}

	mq := Question{ID: "model", Prompt: "Which model?", Free: true}
	if listed {
		mq.Default, mq.Recommended = preset.Model, preset.Model
		mq.Options = []Option{{Value: preset.Model, Label: "the " + provider + " preset"}}
	} else {
		mq.Required = true // an unlisted provider has no preset
	}
	if err := add(mq); err != nil {
		return nil, nil, err
	}
	model := orDefault(qs[len(qs)-1].Value, mq.Default)

	switch {
	case remote, provider == "deepseek", provider == "claude-code":
	default:
		bq := Question{ID: "base_url", Prompt: "Base URL of the provider's API (empty: the provider's own)", Free: true, Default: preset.BaseURL}
		if provider == "ollama" {
			bq.Prompt = "Ollama's address as the server container sees it. On Linux, Ollama must listen on an address the container reaches (OLLAMA_HOST): backsight guide install#local-models-with-ollama"
		}
		if err := add(bq); err != nil {
			return nil, nil, err
		}
	}
	if provider == "claude-code" && !remote {
		if err := add(Question{ID: "ack_subscription", Prompt: "Extracting with your Claude subscription: " + preset.Caveat + ". Backfill of past sessions stays off unless you also answer backfill=yes. Accept?",
			Options: []Option{{Value: "yes", Label: "I accept"}}, Required: true}); err != nil {
			return nil, nil, err
		}
	}

	if !serverOnly {
		rq := Question{ID: "retention", Prompt: "How long should Claude Code keep its transcripts? Backfill and memory rebuilds read them.",
			Free: true, Default: RecommendedRetention, Recommended: RecommendedRetention,
			Options: []Option{{Value: RecommendedRetention, Label: "ten years"}, {Value: "keep", Label: "leave Claude Code's setting as it is", Caveat: "Claude Code deletes transcripts after 30 days by default"}}}
		if err := add(rq); err != nil {
			return nil, nil, err
		}
		if v := qs[len(qs)-1].Value; v != "" && v != "keep" {
			if n, err := strconv.Atoi(v); err != nil || n < 1 {
				return nil, nil, &UsageError{fmt.Sprintf("retention=%s: a number of days ≥ 1, or keep", v)}
			}
		}
	}

	if !remote { // a client's backups are its host's
		if err := add(Question{ID: "backups", Prompt: "Back up the memory every day (encrypted, 7 daily + 4 weekly kept)?", Default: "on", Recommended: "on",
			Options: []Option{{Value: "on", Label: "daily encrypted backups"}, {Value: "off", Label: "no backups", Caveat: "a lost disk loses the memory"}}}); err != nil {
			return nil, nil, err
		}
	}
	if in.ReferencePieces > 0 && !serverOnly {
		if err := add(Question{ID: "reference", Prompt: fmt.Sprintf("%d pieces of an older Hindsight install are wired into Claude Code. Retire them once backsight is installed (undoable; the old server is left untouched)?", in.ReferencePieces),
			Default: "retire", Recommended: "retire",
			Options: []Option{{Value: "retire", Label: "retire them (backup copies kept, setup --undo restores them)"}, {Value: "keep", Label: "leave them", Caveat: "both memories then inject into every prompt"}}}); err != nil {
			return nil, nil, err
		}
	}
	if in.Backfill != nil && !serverOnly {
		if n, cost := in.Backfill(model); n > 0 {
			sessions := "sessions"
			if n == 1 {
				sessions = "session"
			}
			bq := Question{ID: "backfill", Prompt: fmt.Sprintf("Send your %d past %s to the memory, in the background?", n, sessions), Default: "yes", Recommended: "yes",
				Options: []Option{{Value: "yes", Label: "backfill in the background (jobs tick), within a spending cap", Cost: cost}, {Value: "no", Label: "start empty"}}}
			if provider == "claude-code" {
				bq.Default, bq.Recommended = "no", "no"
				bq.Options[0].Caveat = "months of sessions replayed unattended on your subscription"
			}
			if err := add(bq); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := add(Question{ID: "consent", Prompt: "Go ahead with this plan?", Required: true,
		Options: []Option{{Value: "yes", Label: "set up backsight"}, {Value: "no", Label: "stop here, change nothing"}}}); err != nil {
		return nil, nil, err
	}

	return qs, extra, nil
}

// Defaults fills every unanswered question that has a default and is not
// Required (--non-interactive), and returns the ids still unanswered.
func Defaults(qs []Question) (missing []string) {
	for i := range qs {
		q := &qs[i]
		if !q.Asked() {
			continue
		}
		if q.Default != "" && !q.Required {
			q.Value, q.Source = q.Default, "default"
			continue
		}
		if q.ID == "base_url" { // empty is a valid base URL: the provider's own
			q.Source = "default"
			continue
		}
		missing = append(missing, q.ID)
	}
	return missing
}

// Values maps each question to its resolved value.
func Values(qs []Question) map[string]string {
	out := map[string]string{}
	for _, q := range qs {
		if q.Value != "" {
			out[q.ID] = q.Value
		}
	}
	return out
}

func (q Question) accepts(v string) bool { return slices.Contains(q.values(), v) }

func (q Question) values() []string {
	out := make([]string, 0, len(q.Options))
	for _, o := range q.Options {
		out = append(out, o.Value)
	}
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// configKey names the config.toml key behind an answer.
var configKey = map[string]string{"provider": "llm.provider", "model": "llm.model", "base_url": "llm.base_url",
	"retention": "client.retention_days", "backups": "backup.enabled", "api_port": "server.url", "ui_port": "server.ui_port", "project": "server.project", "role": "server.role"}

// configAnswers are the answers an existing config already gives.
func configAnswers(c *config.Config) map[string]string {
	out := map[string]string{}
	if c == nil {
		return out
	}
	out["provider"], out["model"] = c.LLM.Provider, c.LLM.Model
	if c.LLM.Provider != "deepseek" && c.LLM.Provider != "claude-code" {
		out["base_url"] = c.LLM.BaseURL
	}
	if c.Client.RetentionDays > 0 { // 0: not managed yet, still a question
		out["retention"] = strconv.Itoa(c.Client.RetentionDays)
	}
	out["backups"] = map[bool]string{true: "on", false: "off"}[c.Backup.Enabled]
	if p, err := c.APIPort(); err == nil {
		out["api_port"] = strconv.Itoa(p)
	}
	out["ui_port"] = strconv.Itoa(c.Server.UIPort)
	out["project"] = c.Server.Project
	out["role"] = c.Server.Role
	for k, v := range out {
		if v == "" && k != "base_url" {
			delete(out, k)
		}
	}
	return out
}

var projectRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Keys returns the sorted ids of m.
func Keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
