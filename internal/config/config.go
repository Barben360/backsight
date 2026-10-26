// Package config loads and validates config.toml (SPEC Appendix A.1).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/project"
)

// SchemaVersion this binary reads and writes.
const SchemaVersion = 1

type Config struct {
	SchemaVersion int        `toml:"schema_version" json:"schema_version"`
	Server        Server     `toml:"server" json:"server"`
	Client        Client     `toml:"client" json:"client"`
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
	// Project is the Compose project name, and the prefix of its volume:
	// another name lets a second install run beside the first (tests).
	Project string `toml:"project" json:"project"`
	Digest  string `toml:"digest" json:"digest"` // empty: the binary's pin
}

// Client is what backsight keeps in Claude Code's own settings (0052).
type Client struct {
	RetentionDays int `toml:"retention_days" json:"retention_days"` // cleanupPeriodDays floor; 0 = not managed
}

type Projects struct {
	Transverse []string `toml:"transverse" json:"transverse"`
	// OwnOwners: remote owners (host/owner, such as github.com/acme) whose
	// repositories are the user's own, besides the owner of the repository
	// a session starts in. A subagent's report goes to the own repository
	// its work is in (decision 0072); other repositories never count.
	OwnOwners []string `toml:"own_owners" json:"own_owners"`
}

type Privacy struct {
	ExcludePaths []string `toml:"exclude_paths" json:"exclude_paths"`
	// SkipEntrypoints: transcript entrypoints (globs) whose records are never
	// retained, headless SDK runs by default (decision 0071 C1).
	SkipEntrypoints []string `toml:"skip_entrypoints" json:"skip_entrypoints"`
}

// SkipsEntrypoint reports whether records written under entrypoint ep stay
// out of memory. Records with no entrypoint (older transcripts) are kept.
func (p Privacy) SkipsEntrypoint(ep string) bool {
	if ep == "" {
		return false
	}
	for _, pat := range p.SkipEntrypoints {
		if ok, _ := path.Match(pat, ep); ok {
			return true
		}
	}
	return false
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
	// Briefing: "model" (a per-project mental model the server keeps,
	// decision 0060) or "recent" (the 5 latest observations, E8).
	Briefing string `toml:"briefing" json:"briefing"`
}

type LLM struct {
	Provider string `toml:"provider" json:"provider"`
	Model    string `toml:"model" json:"model"`
	BaseURL  string `toml:"base_url" json:"base_url"`
	// Prices per million tokens; 0 uses the binary's table (decision 0051).
	PriceInput  float64 `toml:"price_input" json:"price_input"`
	PriceCached float64 `toml:"price_cached" json:"price_cached"`
	PriceOutput float64 `toml:"price_output" json:"price_output"`
	Currency    string  `toml:"currency" json:"currency"`
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

// Update: where releases come from and which ones count (SPEC §11.7,
// decision 0067).
type Update struct {
	AutoApply bool `toml:"auto_apply" json:"auto_apply"`
	// Source: "" = the GitHub releases of Barben360/backsight; else an
	// absolute path to a local release directory (internal/release.Dir).
	Source string `toml:"source" json:"source"`
	// Prereleases: offer pre-releases even when the installed version is a
	// stable one.
	Prereleases bool `toml:"prereleases" json:"prereleases"`
}

// Default is the Appendix A.1 configuration for these directories and home.
func Default(d paths.Dirs, home string) Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Server: Server{URL: "http://127.0.0.1:8888", Bank: "claude-memory", Mode: "local",
			UIPort: 9999, ComposeDir: filepath.Join(d.Data, "server"), Project: "backsight"},
		Projects:   Projects{Transverse: []string{".claude", filepath.Base(home)}},
		Privacy:    Privacy{ExcludePaths: []string{}, SkipEntrypoints: []string{"sdk-*"}},
		Components: Components{Read: true, Write: true},
		Recall: Recall{MinScore: 0.5, GlobalMinScore: 0.6, PromptHits: 3, QueryChars: 300, PromptChars: 800, MinGap: 3,
			RecallMinChars: 60, ReminderMinChars: 40, QuietPrompts: 3, StickyMax: 3, StickyBypass: 0.97,
			StickyWindowH: 24, ProjectBoost: 0.05, SessionBudgetChars: 6000, Briefing: "model"},
		LLM:        LLM{Provider: "deepseek", Model: "deepseek-flash", Currency: "USD"},
		Embeddings: Embeddings{Preset: "multilingual"},
		Network:    Network{Exposure: "loopback", BreakerFailures: 3, BreakerPauseMin: 5},
		Hooks:      Hooks{SubagentSkip: []string{"Explore", "statusline-setup", "claude-code-guide"}, OutboxWarnMB: 50, OutboxMaxMB: 500},
		Backup:     Backup{Enabled: true, Dir: filepath.Join(d.Data, "backups"), KeepDaily: 7, KeepWeekly: 4},
	}
}

// Load reads path over the defaults. Unknown keys and invalid values are
// refused with their key path; a missing file returns an fs.ErrNotExist error.
func Load(path string, d paths.Dirs, home string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Default(d, home), err
	}
	return Parse(raw, path, d, home)
}

// Parse is Load on bytes already read; path names them in errors.
func Parse(raw []byte, path string, d paths.Dirs, home string) (Config, error) {
	cfg := Default(d, home)
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
	if !projectRE.MatchString(c.Server.Project) {
		bad("server.project", "%q: lowercase letters, digits, - and _, starting with a letter or digit", c.Server.Project)
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
	for _, o := range c.Projects.OwnOwners {
		if n := project.NormaliseRemote(o); n == "" || !strings.Contains(strings.Trim(n, "/"), "/") {
			bad("projects.own_owners", "%q is not a host and owner, such as github.com/acme", o)
		}
	}
	for _, pat := range c.Privacy.SkipEntrypoints {
		if _, err := path.Match(pat, ""); err != nil || pat == "" {
			bad("privacy.skip_entrypoints", "%q is not a valid pattern", pat)
		}
	}
	r := c.Recall
	unit("recall.min_score", r.MinScore)
	unit("recall.global_min_score", r.GlobalMinScore)
	unit("recall.sticky_bypass", r.StickyBypass)
	unit("recall.project_boost", r.ProjectBoost)
	if r.Briefing != "model" && r.Briefing != "recent" {
		bad("recall.briefing", "%q: \"model\" or \"recent\"", r.Briefing)
	}
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
	for key, v := range map[string]float64{"llm.price_input": c.LLM.PriceInput, "llm.price_cached": c.LLM.PriceCached, "llm.price_output": c.LLM.PriceOutput} {
		if v < 0 {
			bad(key, "must be ≥ 0, got %v", v)
		}
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
	atLeast("client.retention_days", c.Client.RetentionDays, 0)
	atLeast("backup.keep_daily", c.Backup.KeepDaily, 1) // 0 would prune the backup just written
	atLeast("backup.keep_weekly", c.Backup.KeepWeekly, 0)
	// A relative directory would follow whatever directory a command runs
	// from (a hook, a timer, the shell): never the same server or backups.
	for key, v := range map[string]string{"server.compose_dir": c.Server.ComposeDir, "backup.dir": c.Backup.Dir} {
		if !filepath.IsAbs(v) {
			bad(key, "%q must be an absolute directory path", v)
		}
	}
	if c.Update.Source != "" && !filepath.IsAbs(c.Update.Source) {
		bad("update.source", "%q must be empty (GitHub releases) or an absolute directory path", c.Update.Source)
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errors.Join(errs...)
}

var (
	digestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	projectRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

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
