package config

import (
	"errors"
	"fmt"
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
		{"briefing source", func(c *Config) { c.Recall.Briefing = "both" }, "recall.briefing"},
		{"skip_entrypoints bad glob", func(c *Config) { c.Privacy.SkipEntrypoints = []string{"sdk-["} }, "privacy.skip_entrypoints"},
		{"skip_entrypoints empty pattern", func(c *Config) { c.Privacy.SkipEntrypoints = []string{""} }, "privacy.skip_entrypoints"},
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
		{"negative price", func(c *Config) { c.LLM.PriceOutput = -1 }, "llm.price_output"},
		{"no transverse alias", func(c *Config) { c.Projects.Transverse = nil }, "projects.transverse"},
		{"outbox order", func(c *Config) { c.Hooks.OutboxWarnMB = 600 }, "hooks.outbox_warn_mb"},
		{"backup keep", func(c *Config) { c.Backup.KeepDaily = -1 }, "backup.keep_daily"},
		{"retention", func(c *Config) { c.Client.RetentionDays = -1 }, "client.retention_days"},
		{"relative update source", func(c *Config) { c.Update.Source = "dist" }, "update.source"},
		{"relative compose dir", func(c *Config) { c.Server.ComposeDir = "server" }, "server.compose_dir"},
		{"empty compose dir", func(c *Config) { c.Server.ComposeDir = "" }, "server.compose_dir"},
		{"home-relative compose dir", func(c *Config) { c.Server.ComposeDir = "~/server" }, "server.compose_dir"},
		{"relative backup dir", func(c *Config) { c.Backup.Dir = "./backups" }, "backup.dir"},
		{"empty backup dir", func(c *Config) { c.Backup.Dir = "" }, "backup.dir"},
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

func TestUpdateKeys(t *testing.T) {
	cfg, err := Load(write(t, "[update]\nsource = \"/srv/releases\"\nprereleases = true\n"), dirs, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Update.Source != "/srv/releases" || !cfg.Update.Prereleases || cfg.Update.AutoApply {
		t.Errorf("%+v", cfg.Update)
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
		"backup.keep_daily":           {1, func(c *Config, v int) { c.Backup.KeepDaily = v }}, // 0 would prune the backup just written
		"client.retention_days":       {0, func(c *Config, v int) { c.Client.RetentionDays = v }},
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

func TestSetClientRetentionKeepsTheRestOfTheFile(t *testing.T) {
	cases := []struct {
		name, in, want string
		days           int
	}{
		{"no section", "schema_version = 1\n# mine\n[server]\nbank = \"b\"\n", "schema_version = 1\n# mine\n[server]\nbank = \"b\"\n\n[client]\nretention_days = 3650\n", 3650},
		{"section without key", "[client]\n# note\n[server]\nbank = \"b\"\n", "[client]\nretention_days = 90\n# note\n[server]\nbank = \"b\"\n", 90},
		{"replace keeps the comment", "[client]\nretention_days = 30 # legal hold\n\n[llm]\nmodel = \"m\"\n", "[client]\nretention_days = 3650 # legal hold\n\n[llm]\nmodel = \"m\"\n", 3650},
		{"remove", "[client]\nretention_days = 30\n[llm]\n", "[client]\n[llm]\n", 0},
		{"remove absent", "[llm]\n", "[llm]\n", 0},
		{"empty file", "", "[client]\nretention_days = 3650\n", 3650},
		{"no trailing newline", "[server]\nbank = \"b\"", "[server]\nbank = \"b\"\n\n[client]\nretention_days = 7\n", 7},
		{"other table's same key", "[other]\nretention_days = 1\n", "[other]\nretention_days = 1\n\n[client]\nretention_days = 5\n", 5},
		{"CRLF", "[server]\r\nbank = \"b\"\r\n", "[server]\r\nbank = \"b\"\r\n\r\n[client]\r\nretention_days = 5\r\n", 5},
		{"CRLF replace", "[client]\r\nretention_days = 1\r\n", "[client]\r\nretention_days = 5\r\n", 5},
	}
	for _, c := range cases {
		got, err := SetClientRetention([]byte(c.in), c.days)
		if err != nil || string(got) != c.want {
			t.Errorf("%s:\ngot  %q (%v)\nwant %q", c.name, got, err, c.want)
		}
	}
}

// Forms a line edit cannot change safely are refused, not mangled.
func TestSetClientRetentionRefusesOtherForms(t *testing.T) {
	for _, in := range []string{"client.retention_days = 30\n", "client = { retention_days = 30 }\n", "x = \"\"\"\n[client]\n\"\"\"\n"} {
		if _, err := SetClientRetention([]byte(in), 3650); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestSetExcludePaths(t *testing.T) {
	cases := []struct {
		name, in, want string
		paths          []string
	}{
		{"add section", "[server]\nbank = \"b\"\n", "[server]\nbank = \"b\"\n\n[privacy]\nexclude_paths = [\"/w/secret\"]\n", []string{"/w/secret"}},
		{"replace keeps comment", "[privacy]\nexclude_paths = [] # mine\n", "[privacy]\nexclude_paths = [\"/a\", \"/b c\"] # mine\n", []string{"/a", "/b c"}},
		{"key added to section", "[privacy]\n[llm]\n", "[privacy]\nexclude_paths = [\"/a\"]\n[llm]\n", []string{"/a"}},
		{"empty list", "[privacy]\nexclude_paths = [\"/a\"]\n", "[privacy]\nexclude_paths = []\n", nil},
		{"quotes escaped", "", "[privacy]\nexclude_paths = [\"/w/\\\"q\\\"\"]\n", []string{`/w/"q"`}},
	}
	for _, c := range cases {
		got, err := SetExcludePaths([]byte(c.in), c.paths)
		if err != nil || string(got) != c.want {
			t.Errorf("%s:\ngot  %q (%v)\nwant %q", c.name, got, err, c.want)
		}
	}
	for _, in := range []string{"[privacy]\nexclude_paths = [\n  \"/a\",\n]\n", "privacy.exclude_paths = []\n", "privacy = { exclude_paths = [] }\n"} {
		if _, err := SetExcludePaths([]byte(in), []string{"/x"}); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestSetExcludePathsWithHashesAndQuotesInValues(t *testing.T) {
	in := "[privacy]\nexclude_paths = [\"/a#b\", \"/c]d\"] # kept \"note\"\n"
	got, err := SetExcludePaths([]byte(in), []string{"/a#b", "/c]d", "/e"})
	if err != nil || string(got) != "[privacy]\nexclude_paths = [\"/a#b\", \"/c]d\", \"/e\"] # kept \"note\"\n" {
		t.Errorf("%q %v", got, err)
	}
}

func TestServerProjectIsValidated(t *testing.T) {
	for v, ok := range map[string]bool{"backsight": true, "bs-fresh_2": true, "": false, "Backsight": false, "-x": false, "a b": false} {
		cfg := Default(dirs, "/home/u")
		cfg.Server.Project = v
		if err := cfg.Validate(); (err == nil) != ok {
			t.Errorf("project %q: %v", v, err)
		}
	}
}

// Absolute directories are accepted, from the file as from the defaults.
func TestAbsoluteDirectoriesAccepted(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(write(t, fmt.Sprintf("[server]\ncompose_dir = %q\n\n[backup]\ndir = %q\n", filepath.Join(dir, "srv"), filepath.Join(dir, "bk"))), dirs, "/home/u")
	if err != nil || cfg.Server.ComposeDir != filepath.Join(dir, "srv") || cfg.Backup.Dir != filepath.Join(dir, "bk") {
		t.Errorf("%v %+v %+v", err, cfg.Server, cfg.Backup)
	}
	if _, err := Load(write(t, "[backup]\ndir = \"backups\"\n"), dirs, "/home/u"); err == nil || !strings.Contains(err.Error(), "backup.dir") {
		t.Errorf("a relative backup.dir in the file: %v", err)
	}
}

func TestSkipsEntrypoint(t *testing.T) {
	p := Default(dirs, "/home/u").Privacy
	for ep, want := range map[string]bool{"sdk-ts": true, "sdk-cli": true, "sdk-py": true, "cli": false, "claude-desktop": false, "": false} {
		if got := p.SkipsEntrypoint(ep); got != want {
			t.Errorf("default SkipsEntrypoint(%q) = %v, want %v", ep, got, want)
		}
	}
	p.SkipEntrypoints = []string{}
	if p.SkipsEntrypoint("sdk-ts") {
		t.Error("an empty list must keep every entrypoint")
	}
}
