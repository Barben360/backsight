// Package config loads and validates config.toml (SPEC Appendix A.1).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/backsight/internal/paths"
)

// SchemaVersion this binary reads and writes.
const SchemaVersion = 1

type Config struct {
	SchemaVersion int        `toml:"schema_version" json:"schema_version"`
	Server        Server     `toml:"server" json:"server"`
	Projects      Projects   `toml:"projects" json:"projects"`
	Privacy       Privacy    `toml:"privacy" json:"privacy"`
	Components    Components `toml:"components" json:"components"`
	Recall        Recall     `toml:"recall" json:"recall"`
	LLM           LLM        `toml:"llm" json:"llm"`
	Embeddings    Embeddings `toml:"embeddings" json:"embeddings"`
	Network       Network    `toml:"network" json:"network"`
	Hooks         Hooks      `toml:"hooks" json:"hooks"`
	Backup        Backup     `toml:"backup" json:"backup"`
	Update        Update     `toml:"update" json:"update"`
}

type Server struct {
	URL        string `toml:"url" json:"url"`
	Bank       string `toml:"bank" json:"bank"`
	Mode       string `toml:"mode" json:"mode"`
	UIPort     int    `toml:"ui_port" json:"ui_port"`
	ComposeDir string `toml:"compose_dir" json:"compose_dir"`
	Digest     string `toml:"digest" json:"digest"` // empty: the binary's pin
}

type Projects struct {
	Transverse []string `toml:"transverse" json:"transverse"`
}

type Privacy struct {
	ExcludePaths []string `toml:"exclude_paths" json:"exclude_paths"`
}

// Components say which integration pieces this install owns, so the product
// can run next to reference pieces during the migration milestones.
type Components struct {
	Read  bool `toml:"read" json:"read"`
	Write bool `toml:"write" json:"write"`
}

type Recall struct {
	MinScore           float64 `toml:"min_score" json:"min_score"`
	GlobalMinScore     float64 `toml:"global_min_score" json:"global_min_score"` // per-prompt global hits (0046)
	PromptHits         int     `toml:"prompt_hits" json:"prompt_hits"`
	QueryChars         int     `toml:"query_chars" json:"query_chars"`
	PromptChars        int     `toml:"prompt_chars" json:"prompt_chars"`
	MinGap             int     `toml:"min_gap" json:"min_gap"`
	RecallMinChars     int     `toml:"recall_min_chars" json:"recall_min_chars"`
	ReminderMinChars   int     `toml:"reminder_min_chars" json:"reminder_min_chars"`
	QuietPrompts       int     `toml:"quiet_prompts" json:"quiet_prompts"`
	StickyMax          int     `toml:"sticky_max" json:"sticky_max"`
	StickyBypass       float64 `toml:"sticky_bypass" json:"sticky_bypass"`
	StickyWindowH      int     `toml:"sticky_window_h" json:"sticky_window_h"`
	ProjectBoost       float64 `toml:"project_boost" json:"project_boost"`
	SessionBudgetChars int     `toml:"session_budget_chars" json:"session_budget_chars"`
}

type LLM struct {
	Provider string `toml:"provider" json:"provider"`
	Model    string `toml:"model" json:"model"`
	BaseURL  string `toml:"base_url" json:"base_url"`
}

type Embeddings struct {
	Preset string `toml:"preset" json:"preset"`
}

type Network struct {
	Exposure        string `toml:"exposure" json:"exposure"`
	BindIP          string `toml:"bind_ip" json:"bind_ip"`
	BreakerFailures int    `toml:"breaker_failures" json:"breaker_failures"`
	BreakerPauseMin int    `toml:"breaker_pause_min" json:"breaker_pause_min"`
}

type Hooks struct {
	SubagentSkip []string `toml:"subagent_skip" json:"subagent_skip"`
	OutboxWarnMB int      `toml:"outbox_warn_mb" json:"outbox_warn_mb"`
	OutboxMaxMB  int      `toml:"outbox_max_mb" json:"outbox_max_mb"`
}

type Backup struct {
	Enabled    bool   `toml:"enabled" json:"enabled"`
	Dir        string `toml:"dir" json:"dir"`
	Recipient  string `toml:"recipient" json:"recipient"`
	KeepDaily  int    `toml:"keep_daily" json:"keep_daily"`
	KeepWeekly int    `toml:"keep_weekly" json:"keep_weekly"`
}

type Update struct {
	AutoApply bool `toml:"auto_apply" json:"auto_apply"`
}

// Default is the Appendix A.1 configuration for these directories and home.
func Default(d paths.Dirs, home string) Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Server: Server{URL: "http://127.0.0.1:8888", Bank: "claude-memory", Mode: "local",
			UIPort: 9999, ComposeDir: filepath.Join(d.Data, "server")},
		Projects:   Projects{Transverse: []string{".claude", filepath.Base(home)}},
		Privacy:    Privacy{ExcludePaths: []string{}},
		Components: Components{Read: true, Write: true},
		Recall: Recall{MinScore: 0.5, GlobalMinScore: 0.6, PromptHits: 3, QueryChars: 300, PromptChars: 800, MinGap: 3,
			RecallMinChars: 60, ReminderMinChars: 40, QuietPrompts: 3, StickyMax: 3, StickyBypass: 0.97,
			StickyWindowH: 24, ProjectBoost: 0.05, SessionBudgetChars: 6000},
		LLM:        LLM{Provider: "deepseek", Model: "deepseek-v4-flash"},
		Embeddings: Embeddings{Preset: "multilingual"},
		Network:    Network{Exposure: "loopback", BreakerFailures: 3, BreakerPauseMin: 5},
		Hooks:      Hooks{SubagentSkip: []string{"Explore", "statusline-setup", "claude-code-guide"}, OutboxWarnMB: 50, OutboxMaxMB: 500},
		Backup:     Backup{Enabled: true, Dir: filepath.Join(d.Data, "backups"), KeepDaily: 7, KeepWeekly: 4},
	}
}

// Load reads path over the defaults. Unknown keys and invalid values are
// refused with their key path; a missing file returns an fs.ErrNotExist error.
func Load(path string, d paths.Dirs, home string) (Config, error) {
	cfg := Default(d, home)
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	dec := toml.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			var keys []string
			for _, e := range strict.Errors {
				row, _ := e.Position()
				keys = append(keys, fmt.Sprintf("%s (line %d)", strings.Join(e.Key(), "."), row))
			}
			return cfg, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			key := strings.Join(de.Key(), ".")
			if key != "" {
				key += ": "
			}
			return cfg, fmt.Errorf("%s:%d:%d: %s%s", path, row, col, key, de.Error())
		}
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.SchemaVersion < 1 {
		return cfg, fmt.Errorf("%s: schema_version must be ≥ 1, got %d", path, cfg.SchemaVersion)
	}
	if cfg.SchemaVersion > SchemaVersion {
		return cfg, fmt.Errorf("%s: schema_version %d was written by a newer backsight (this one reads %d)", path, cfg.SchemaVersion, SchemaVersion)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate reports every invalid value, each with its key path.
func (c Config) Validate() error {
	var errs []error
	bad := func(key, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{key}, args...)...))
	}
	oneOf := func(key, v string, allowed ...string) {
		if !slices.Contains(allowed, v) {
			bad(key, "%q is not one of %v", v, allowed)
		}
	}
	atLeast := func(key string, v, minimum int) {
		if v < minimum {
			bad(key, "must be ≥ %d, got %d", minimum, v)
		}
	}
	unit := func(key string, v float64) {
		if v < 0 || v > 1 {
			bad(key, "must be within [0, 1], got %v", v)
		}
	}

	oneOf("server.mode", c.Server.Mode, "local", "remote")
	switch u, err := url.Parse(c.Server.URL); {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		bad("server.url", "is not an http(s) URL with a host")
	case u.User != nil:
		bad("server.url", "must not carry credentials (the API key lives in secrets.toml)")
	}
	if c.Server.Mode == "local" {
		if u, err := url.Parse(c.Server.URL); err == nil && u.Host != "" && u.Port() == "" {
			bad("server.url", "local mode needs an explicit port (the API port to publish)")
		}
	}
	if c.Server.UIPort < 1 || c.Server.UIPort > 65535 {
		bad("server.ui_port", "must be within [1, 65535], got %d", c.Server.UIPort)
	} else if p, err := c.APIPort(); err == nil && p == c.Server.UIPort {
		bad("server.ui_port", "must differ from the API port %d", p)
	}
	if d := c.Server.Digest; d != "" && !digestRE.MatchString(d) {
		bad("server.digest", "%q is not empty or sha256:<64 hex>", d)
	}
	switch c.Server.Bank {
	case "":
		bad("server.bank", "must not be empty")
	case ".", "..":
		bad("server.bank", "%q is not a bank id", c.Server.Bank)
	}
	if len(c.Projects.Transverse) == 0 || c.Projects.Transverse[0] == "" {
		bad("projects.transverse", "needs at least one alias (the first is canonical)")
	}
	r := c.Recall
	unit("recall.min_score", r.MinScore)
	unit("recall.global_min_score", r.GlobalMinScore)
	unit("recall.sticky_bypass", r.StickyBypass)
	unit("recall.project_boost", r.ProjectBoost)
	for key, v := range map[string]int{"recall.prompt_hits": r.PromptHits, "recall.query_chars": r.QueryChars,
		"recall.prompt_chars": r.PromptChars, "recall.sticky_max": r.StickyMax, "recall.sticky_window_h": r.StickyWindowH,
		"recall.session_budget_chars": r.SessionBudgetChars} {
		atLeast(key, v, 1)
	}
	for key, v := range map[string]int{"recall.min_gap": r.MinGap, "recall.recall_min_chars": r.RecallMinChars,
		"recall.reminder_min_chars": r.ReminderMinChars, "recall.quiet_prompts": r.QuietPrompts} {
		atLeast(key, v, 0)
	}
	if c.LLM.Provider == "" {
		bad("llm.provider", "must not be empty")
	}
	oneOf("embeddings.preset", c.Embeddings.Preset, "multilingual", "english")
	oneOf("network.exposure", c.Network.Exposure, "loopback", "private")
	if c.Network.Exposure == "private" {
		if ip := net.ParseIP(c.Network.BindIP); ip == nil || !isPrivate(ip) {
			bad("network.bind_ip", "%q must be a private (LAN or tailnet) address for private exposure", c.Network.BindIP)
		}
	}
	atLeast("network.breaker_failures", c.Network.BreakerFailures, 1)
	atLeast("network.breaker_pause_min", c.Network.BreakerPauseMin, 1)
	atLeast("hooks.outbox_warn_mb", c.Hooks.OutboxWarnMB, 1)
	if c.Hooks.OutboxWarnMB > c.Hooks.OutboxMaxMB {
		bad("hooks.outbox_warn_mb", "(%d) must not exceed hooks.outbox_max_mb (%d)", c.Hooks.OutboxWarnMB, c.Hooks.OutboxMaxMB)
	}
	atLeast("backup.keep_daily", c.Backup.KeepDaily, 0)
	atLeast("backup.keep_weekly", c.Backup.KeepWeekly, 0)
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errors.Join(errs...)
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// APIPort is the port of server.url, explicit or the scheme's default.
func (c Config) APIPort() (int, error) {
	u, err := url.Parse(c.Server.URL)
	if err != nil {
		return 0, err
	}
	p := u.Port()
	switch {
	case p != "":
	case u.Scheme == "https":
		p = "443"
	default:
		p = "80"
	}
	return strconv.Atoi(p)
}

// cgnat is 100.64.0.0/10, the range tailnets such as Tailscale use.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func isPrivate(ip net.IP) bool { return ip.IsPrivate() || cgnat.Contains(ip) }
