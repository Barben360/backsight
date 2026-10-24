package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/backsight/internal/paths"
)

var dirs = paths.Dirs{Data: "/d", Config: "/c", State: "/s"}

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultMatchesSpecAppendixA(t *testing.T) {
	raw, err := os.ReadFile("testdata/appendix-a1.toml")
	if err != nil {
		t.Fatal(err)
	}
	var want Config
	dec := toml.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&want); err != nil {
		t.Fatalf("golden file does not fit the Config struct: %v", err)
	}
	if got := Default(dirs, "/home/u"); !reflect.DeepEqual(got, want) {
		t.Errorf("defaults drifted from Appendix A.1\ngot  %+v\nwant %+v", got, want)
	}
	if err := Default(dirs, "/home/u").Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.toml"), dirs, "/home/u")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("want ErrNotExist, got %v", err)
	}
}

func TestLoadPartialFileKeepsDefaults(t *testing.T) {
	cfg, err := Load(write(t, "[server]\nbank = \"work\"\n[recall]\nmin_score = 0.6\n"), dirs, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	want := Default(dirs, "/home/u")
	want.Server.Bank, want.Recall.MinScore = "work", 0.6
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadRefusals(t *testing.T) {
	cases := []struct{ name, content, wantInErr string }{
		{"unknown key", "[recall]\nmin_scor = 0.5\n", "recall.min_scor"},
		{"unknown table", "[gpu]\nenabled = true\n", "gpu"},
		{"type mismatch", "[recall]\nmin_gap = \"three\"\n", "min_gap"},
		{"newer schema", "schema_version = 2\n", "newer backsight"},
		{"zero schema", "schema_version = 0\n", "schema_version"},
		{"invalid value", "[server]\nmode = \"cloud\"\n", "server.mode"},
		// Removed by spec v8 (decision 0039): facts are always English.
		{"language key", "language = \"fr\"\n", "language"},
		{"not toml", "this is = = not toml", "config.toml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, c.content), dirs, "/home/u")
			if err == nil || !strings.Contains(err.Error(), c.wantInErr) {
				t.Errorf("want an error naming %q, got %v", c.wantInErr, err)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*Config)
		wantInErr string
	}{
		{"mode", func(c *Config) { c.Server.Mode = "cloud" }, "server.mode"},
		{"url scheme", func(c *Config) { c.Server.URL = "ftp://x" }, "server.url"},
		{"url empty", func(c *Config) { c.Server.URL = "" }, "server.url"},
		{"url without host", func(c *Config) { c.Server.URL = "http://" }, "server.url"},
		{"url with credentials", func(c *Config) { c.Server.URL = "http://u:pw@127.0.0.1:8888" }, "server.url"},
		{"bank dot", func(c *Config) { c.Server.Bank = "." }, "server.bank"},
		{"ui port zero", func(c *Config) { c.Server.UIPort = 0 }, "server.ui_port"},
		{"ui port too big", func(c *Config) { c.Server.UIPort = 70000 }, "server.ui_port"},
		{"ui port equals api port", func(c *Config) { c.Server.UIPort = 8888 }, "server.ui_port"},
		{"local without explicit port", func(c *Config) { c.Server.URL = "http://127.0.0.1" }, "server.url"},
		{"digest not sha256", func(c *Config) { c.Server.Digest = "latest" }, "server.digest"},
		{"digest short", func(c *Config) { c.Server.Digest = "sha256:abc" }, "server.digest"},
		{"bank dotdot", func(c *Config) { c.Server.Bank = ".." }, "server.bank"},
		{"global_min_score range", func(c *Config) { c.Recall.GlobalMinScore = 1.2 }, "recall.global_min_score"},
		{"negative min_score", func(c *Config) { c.Recall.MinScore = -0.1 }, "recall.min_score"},
		{"sticky_bypass range", func(c *Config) { c.Recall.StickyBypass = 2 }, "recall.sticky_bypass"},
		{"project_boost range", func(c *Config) { c.Recall.ProjectBoost = -1 }, "recall.project_boost"},
		{"empty canonical alias", func(c *Config) { c.Projects.Transverse = []string{""} }, "projects.transverse"},
		{"loopback bind for private", func(c *Config) { c.Network.Exposure, c.Network.BindIP = "private", "127.0.0.1" }, "network.bind_ip"},
		{"cgnat below", func(c *Config) { c.Network.Exposure, c.Network.BindIP = "private", "100.63.255.255" }, "network.bind_ip"},
		{"cgnat above", func(c *Config) { c.Network.Exposure, c.Network.BindIP = "private", "100.128.0.1" }, "network.bind_ip"},
		{"breaker failures", func(c *Config) { c.Network.BreakerFailures = 0 }, "network.breaker_failures"},
		{"breaker pause", func(c *Config) { c.Network.BreakerPauseMin = 0 }, "network.breaker_pause_min"},
		{"outbox warn zero", func(c *Config) { c.Hooks.OutboxWarnMB = 0 }, "hooks.outbox_warn_mb"},
		{"bank empty", func(c *Config) { c.Server.Bank = "" }, "server.bank"},
		{"exposure", func(c *Config) { c.Network.Exposure = "public" }, "network.exposure"},
		{"private needs ip", func(c *Config) { c.Network.Exposure = "private" }, "network.bind_ip"},
		{"public ip refused", func(c *Config) { c.Network.Exposure, c.Network.BindIP = "private", "8.8.8.8" }, "network.bind_ip"},
		{"min_score range", func(c *Config) { c.Recall.MinScore = 1.5 }, "recall.min_score"},
		{"negative count", func(c *Config) { c.Recall.MinGap = -1 }, "recall.min_gap"},
		{"zero hits", func(c *Config) { c.Recall.PromptHits = 0 }, "recall.prompt_hits"},
		{"preset", func(c *Config) { c.Embeddings.Preset = "huge" }, "embeddings.preset"},
		{"provider empty", func(c *Config) { c.LLM.Provider = "" }, "llm.provider"},
		{"no transverse alias", func(c *Config) { c.Projects.Transverse = nil }, "projects.transverse"},
		{"outbox order", func(c *Config) { c.Hooks.OutboxWarnMB = 600 }, "hooks.outbox_warn_mb"},
		{"backup keep", func(c *Config) { c.Backup.KeepDaily = -1 }, "backup.keep_daily"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default(dirs, "/home/u")
			c.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), c.wantInErr) {
				t.Errorf("want an error naming %q, got %v", c.wantInErr, err)
			}
		})
	}
}

func TestValidateAcceptsPrivateAddresses(t *testing.T) {
	for _, ip := range []string{"192.168.1.10", "10.0.0.2", "100.101.102.103", "fd00::1"} {
		cfg := Default(dirs, "/home/u")
		cfg.Network.Exposure, cfg.Network.BindIP = "private", ip
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: %v", ip, err)
		}
	}
}

// Every integer key sits at its documented minimum: one below is refused,
// the minimum itself is accepted.
func TestValidateIntegerBoundaries(t *testing.T) {
	keys := map[string]struct {
		min int
		set func(*Config, int)
	}{
		"recall.prompt_hits":          {1, func(c *Config, v int) { c.Recall.PromptHits = v }},
		"recall.query_chars":          {1, func(c *Config, v int) { c.Recall.QueryChars = v }},
		"recall.prompt_chars":         {1, func(c *Config, v int) { c.Recall.PromptChars = v }},
		"recall.sticky_max":           {1, func(c *Config, v int) { c.Recall.StickyMax = v }},
		"recall.sticky_window_h":      {1, func(c *Config, v int) { c.Recall.StickyWindowH = v }},
		"recall.session_budget_chars": {1, func(c *Config, v int) { c.Recall.SessionBudgetChars = v }},
		"recall.min_gap":              {0, func(c *Config, v int) { c.Recall.MinGap = v }},
		"recall.recall_min_chars":     {0, func(c *Config, v int) { c.Recall.RecallMinChars = v }},
		"recall.reminder_min_chars":   {0, func(c *Config, v int) { c.Recall.ReminderMinChars = v }},
		"recall.quiet_prompts":        {0, func(c *Config, v int) { c.Recall.QuietPrompts = v }},
		"backup.keep_daily":           {0, func(c *Config, v int) { c.Backup.KeepDaily = v }},
		"backup.keep_weekly":          {0, func(c *Config, v int) { c.Backup.KeepWeekly = v }},
	}
	for key, k := range keys {
		cfg := Default(dirs, "/home/u")
		k.set(&cfg, k.min)
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s = %d must be accepted: %v", key, k.min, err)
		}
		k.set(&cfg, k.min-1)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), key+":") {
			t.Errorf("%s = %d must be refused with its key, got %v", key, k.min-1, err)
		}
	}
}

func TestValidateReportsEveryErrorSorted(t *testing.T) {
	cfg := Default(dirs, "/home/u")
	cfg.Server.Mode, cfg.LLM.Provider, cfg.Embeddings.Preset = "cloud", "", "huge"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("want errors")
	}
	lines := strings.Split(err.Error(), "\n")
	want := []string{"embeddings.preset", "llm.provider", "server.mode"}
	if len(lines) != len(want) {
		t.Fatalf("want %d errors, got %q", len(want), lines)
	}
	for i, key := range want {
		if !strings.HasPrefix(lines[i], key+":") {
			t.Errorf("line %d = %q, want it to start with %s", i, lines[i], key)
		}
	}
}

func TestValidateDoesNotEchoURLCredentials(t *testing.T) {
	cfg := Default(dirs, "/home/u")
	cfg.Server.URL = "http://u:fakevalue@127.0.0.1:8888"
	if err := cfg.Validate(); err == nil || strings.Contains(err.Error(), "fakevalue") {
		t.Errorf("want a refusal without the password, got %v", err)
	}
}

func TestAPIPortAndDigestAccepted(t *testing.T) {
	cfg := Default(dirs, "/home/u")
	cfg.Server.URL, cfg.Server.UIPort = "http://127.0.0.1:18888", 19999
	cfg.Server.Digest = "sha256:" + strings.Repeat("a", 64)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if p, err := cfg.APIPort(); err != nil || p != 18888 {
		t.Errorf("%d %v", p, err)
	}
	// Remote mode reaches a server elsewhere: the default https port is fine.
	cfg.Server.Mode, cfg.Server.URL = "remote", "https://mem.example"
	if err := cfg.Validate(); err != nil {
		t.Errorf("remote without explicit port: %v", err)
	}
}
