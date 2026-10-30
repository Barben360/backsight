package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

// recall.failure_pitfalls is opt-in and migratable: an older binary refuses
// a config with a key it does not know, so the default must hold when the
// key is absent and no code may ever write it.
func TestFailurePitfallsDefaultsOffAndIsNeverWritten(t *testing.T) {
	cfg, err := Load(write(t, "[recall]\nmin_score = 0.6\n"), dirs, "/home/u")
	if err != nil || cfg.Recall.FailurePitfalls {
		t.Fatalf("absent key: %v, on = %v", err, cfg.Recall.FailurePitfalls)
	}
	cfg, err = Load(write(t, "[recall]\nfailure_pitfalls = true\n"), dirs, "/home/u")
	if err != nil || !cfg.Recall.FailurePitfalls {
		t.Fatalf("set key: %v, on = %v", err, cfg.Recall.FailurePitfalls)
	}
	err = filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, "config.go") {
			return err
		}
		if raw, _ := os.ReadFile(p); strings.Contains(string(raw), "failure_pitfalls") {
			t.Errorf("%s mentions failure_pitfalls: only the user may set it", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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

func TestServerRole(t *testing.T) {
	cfg, err := Load(write(t, "[server]\nbank = \"work\"\n"), dirs, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Role != RoleBoth || cfg.ServerOnly() {
		t.Errorf("without role: got %q, ServerOnly %v; want %q, false", cfg.Server.Role, cfg.ServerOnly(), RoleBoth)
	}
	cfg, err = Load(write(t, "[server]\nrole = \"server\"\n"), dirs, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Role != RoleServer || !cfg.ServerOnly() {
		t.Errorf("role = server: got %q, ServerOnly %v", cfg.Server.Role, cfg.ServerOnly())
	}
	if _, err := Load(write(t, "[server]\nrole = \"client\"\n"), dirs, "/home/u"); err == nil || !strings.Contains(err.Error(), "server.role") {
		t.Errorf("role = client: want an error naming server.role, got %v", err)
	}
	if _, err := Load(write(t, "[server]\nrole = \"server\"\nmode = \"remote\"\n"), dirs, "/home/u"); err == nil || !strings.Contains(err.Error(), "server.role") {
		t.Errorf("role = server with mode = remote: want an error naming server.role, got %v", err)
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
		{"own owner without its host", func(c *Config) { c.Projects.OwnOwners = []string{"acme"} }, "projects.own_owners"},
		{"own owner empty", func(c *Config) { c.Projects.OwnOwners = []string{" "} }, "projects.own_owners"},
		{"denied owner without its host", func(c *Config) { c.Projects.DenyOwners = []string{"acme"} }, "projects.deny_owners"},
		{"loopback bind for private", func(c *Config) { c.Network.Exposure, c.Network.BindIP = "private", "127.0.0.1" }, "network.bind_ip"},
		{"cgnat below", func(c *Config) { c.Network.Exposure, c.Network.BindIP = "private", "100.63.255.255" }, "network.bind_ip"},
		{"cgnat above", func(c *Config) { c.Network.Exposure, c.Network.BindIP = "private", "100.128.0.1" }, "network.bind_ip"},
		{"breaker failures", func(c *Config) { c.Network.BreakerFailures = 0 }, "network.breaker_failures"},
		{"breaker pause", func(c *Config) { c.Network.BreakerPauseMin = 0 }, "network.breaker_pause_min"},
		{"outbox warn zero", func(c *Config) { c.Hooks.OutboxWarnMB = 0 }, "hooks.outbox_warn_mb"},
		{"role unknown", func(c *Config) { c.Server.Role = "client" }, "server.role"},
		{"server-only host with a remote server", func(c *Config) { c.Server.Role, c.Server.Mode = RoleServer, "remote" }, "server.role"},
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
		{"NaN price", func(c *Config) { c.LLM.PriceInput = math.NaN() }, "llm.price_input"},
		{"infinite price", func(c *Config) { c.LLM.PriceCached = math.Inf(1) }, "llm.price_cached"},
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

// server expose sets the exposure and its address, keeping the rest of the
// file; loopback removes the address.
func TestSetNetwork(t *testing.T) {
	cases := []struct {
		name, in, exposure, ip, want string
	}{
		{"private, no section", "# mine\n[server]\nbank = \"b\"\n", "private", "100.101.1.2",
			"# mine\n[server]\nbank = \"b\"\n\n[network]\nexposure = \"private\"\nbind_ip = \"100.101.1.2\"\n"},
		{"private, replaced", "[network]\nexposure = \"private\" # LAN\nbind_ip = \"192.168.1.10\"\nbreaker_failures = 4\n", "private", "100.101.1.2",
			"[network]\nexposure = \"private\" # LAN\nbind_ip = \"100.101.1.2\"\nbreaker_failures = 4\n"},
		{"loopback removes bind_ip", "# mine\n[network]\nexposure = \"private\"\nbind_ip = \"192.168.1.10\"\nbreaker_failures = 4\n[llm]\nmodel = \"m\"\n", "loopback", "",
			"# mine\n[network]\nexposure = \"loopback\"\nbreaker_failures = 4\n[llm]\nmodel = \"m\"\n"},
	}
	for _, c := range cases {
		got, err := SetNetwork([]byte(c.in), c.exposure, c.ip)
		if err != nil || string(got) != c.want {
			t.Errorf("%s:\ngot  %q (%v)\nwant %q", c.name, got, err, c.want)
		}
	}
	if _, err := SetNetwork([]byte("network.exposure = \"private\"\n"), "loopback", ""); err == nil {
		t.Error("a dotted key was edited")
	}
}

// connect points a config at another PC's server: the user's comments and
// unrelated keys stay, and the result loads as a client of that server.
func TestSetRemoteKeepsTheFileAndLoadsRemote(t *testing.T) {
	in := "# mine\nschema_version = 1\n\n[server]\nurl = \"http://127.0.0.1:8888\" # was local\nui_port = 19999\n\n[recall]\nmin_score = 0.6\n"
	got, err := SetRemote([]byte(in), "http://100.101.1.2:8888", "backsight")
	want := "# mine\nschema_version = 1\n\n[server]\nmode = \"remote\"\nbank = \"backsight\"\nurl = \"http://100.101.1.2:8888\" # was local\nui_port = 19999\n\n[recall]\nmin_score = 0.6\n"
	if err != nil || string(got) != want {
		t.Fatalf("got  %q (%v)\nwant %q", got, err, want)
	}
	cfg, err := Parse(got, "config.toml", dirs, "/home/u")
	if err != nil || cfg.Server.Mode != "remote" || cfg.Server.URL != "http://100.101.1.2:8888" || cfg.Server.Bank != "backsight" || cfg.Recall.MinScore != 0.6 {
		t.Errorf("%+v %v", cfg.Server, err)
	}
	fresh, err := SetRemote([]byte("# Written by backsight connect.\nschema_version = 1\n"), "http://h.tailnet.ts.net:8888", "b")
	if cfg, perr := Parse(fresh, "config.toml", dirs, "/home/u"); err != nil || perr != nil || cfg.Server.Mode != "remote" || cfg.Server.URL != "http://h.tailnet.ts.net:8888" ||
		!strings.HasPrefix(string(fresh), "# Written by backsight connect.\n") {
		t.Errorf("new file: %q %v %v", fresh, err, perr)
	}
	if _, err := SetRemote([]byte("server.url = \"x\"\n"), "http://100.101.1.2:8888", "b"); err == nil {
		t.Error("a dotted key was edited")
	}
}

// The host's LLM on a client prices the backfill estimate: each value is
// set in place, and a zero price or an empty value removes its key.
func TestSetLLMEstimate(t *testing.T) {
	in := "# mine\n[llm]\nprovider = \"deepseek\" # host's\nprice_cached = 0.1\nbase_url = \"http://x\"\n[server]\nmode = \"remote\"\nurl = \"http://100.101.1.2:8888\"\n"
	got, err := SetLLMEstimate([]byte(in), "anthropic", "claude-haiku-4-5", "EUR", 1, 0, 5.5)
	want := "# mine\n[llm]\nmodel = \"claude-haiku-4-5\"\nprice_input = 1.0\nprice_output = 5.5\ncurrency = \"EUR\"\nprovider = \"anthropic\" # host's\nbase_url = \"http://x\"\n[server]\nmode = \"remote\"\nurl = \"http://100.101.1.2:8888\"\n"
	if err != nil || string(got) != want {
		t.Fatalf("got  %q (%v)\nwant %q", got, err, want)
	}
	cfg, err := Parse(got, "config.toml", dirs, "/home/u")
	if err != nil || cfg.LLM.Provider != "anthropic" || cfg.LLM.Model != "claude-haiku-4-5" || cfg.LLM.PriceInput != 1 || cfg.LLM.PriceCached != 0 ||
		cfg.LLM.PriceOutput != 5.5 || cfg.LLM.Currency != "EUR" || cfg.LLM.BaseURL != "http://x" {
		t.Errorf("%+v %v", cfg.LLM, err)
	}
	fresh, err := SetLLMEstimate([]byte("schema_version = 1\n"), "deepseek", "deepseek-flash", "", 0, 0, 0)
	if err != nil || string(fresh) != "schema_version = 1\n\n[llm]\nprovider = \"deepseek\"\nmodel = \"deepseek-flash\"\n" {
		t.Errorf("%q %v", fresh, err)
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, err := SetLLMEstimate([]byte(""), "deepseek", "m", "USD", bad, 0, 0); err == nil {
			t.Errorf("price %v accepted", bad)
		}
	}
}

func TestIsTailnet(t *testing.T) {
	for ip, want := range map[string]bool{"100.101.1.2": true, "100.64.0.1": true, "fd7a:115c:a1e0::5": true,
		"192.168.1.10": false, "10.0.0.2": false, "100.128.0.1": false, "fd00::1": false} {
		if got := IsTailnet(net.ParseIP(ip)); got != want {
			t.Errorf("IsTailnet(%s) = %v, want %v", ip, got, want)
		}
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

func TestOwnOwnersAreRead(t *testing.T) {
	cfg, err := Load(write(t, "[projects]\nown_owners = [\"github.com/acme\", \"https://gitlab.com/team/sub\"]\n"), dirs, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects.OwnOwners) != 2 || cfg.Projects.OwnOwners[0] != "github.com/acme" || len(cfg.Projects.Transverse) == 0 {
		t.Errorf("%+v", cfg.Projects)
	}
}

// [feedback] lives in Default only: setup never writes it, since an older
// binary refuses a config.toml with keys it does not know.
func TestFeedbackDefaultsAndValidation(t *testing.T) {
	got := Default(dirs, "/home/u").Feedback
	if want := (Feedback{Enabled: true, ConfidentRank: 2, MissHalfLifeDays: 90, CreditHalfLifeDays: 365, StopCheck: "always", MaxRetirements: 3, StaleShows: 10,
		BoostWeight: 0.1, PriorCredits: 3, PriorMisses: 9, ShareLedger: true}); got != want {
		t.Fatalf("defaults %+v, want %+v", got, want)
	}
	cfg, err := Load(write(t, "[feedback]\nenabled = false\nconfident_rank = 1\nmiss_half_life_days = 0\ncredit_half_life_days = 30\nstop_check = \"cited\"\n"), dirs, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Feedback{ConfidentRank: 1, CreditHalfLifeDays: 30, StopCheck: "cited", MaxRetirements: 3, StaleShows: 10,
		BoostWeight: 0.1, PriorCredits: 3, PriorMisses: 9, ShareLedger: true}); cfg.Feedback != want {
		t.Errorf("read %+v, want %+v", cfg.Feedback, want)
	}
	for body, key := range map[string]string{
		"confident_rank = 0":         "feedback.confident_rank",
		`stop_check = "sometimes"`:   "feedback.stop_check",
		"miss_half_life_days = -1":   "feedback.miss_half_life_days",
		"credit_half_life_days = -1": "feedback.credit_half_life_days",
	} {
		_, err := Load(write(t, "[feedback]\n"+body+"\n"), dirs, "/home/u")
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: got %v, want an error naming %s", body, err, key)
		}
	}
	for _, s := range []string{"always", "cited", "off"} {
		if _, err := Load(write(t, "[feedback]\nstop_check = \""+s+"\"\n"), dirs, "/home/u"); err != nil {
			t.Errorf("stop_check %q refused: %v", s, err)
		}
	}
}

// share_ledger: on by default; false is read as false, and a value that
// is not a boolean is refused with its key.
func TestShareLedgerKey(t *testing.T) {
	if !Default(dirs, "/home/u").Feedback.ShareLedger {
		t.Error("share_ledger off by default")
	}
	cfg, err := Load(write(t, "[feedback]\nshare_ledger = false\n"), dirs, "/home/u")
	if err != nil || cfg.Feedback.ShareLedger || !cfg.Feedback.Enabled {
		t.Errorf("false: %v %+v", err, cfg.Feedback)
	}
	if _, err := Load(write(t, "[feedback]\nshare_ledger = \"no\"\n"), dirs, "/home/u"); err == nil || !strings.Contains(err.Error(), "share_ledger") {
		t.Errorf("a string: %v", err)
	}
}

func TestMaxRetirementsKey(t *testing.T) {
	if got := Default(dirs, "/home/u").Feedback.MaxRetirements; got != 3 {
		t.Errorf("default %d", got)
	}
	cfg, err := Load(write(t, "[feedback]\nmax_retirements = 0\n"), dirs, "/home/u")
	if err != nil || cfg.Feedback.MaxRetirements != 0 {
		t.Errorf("0: %v %d", err, cfg.Feedback.MaxRetirements)
	}
	if _, err := Load(write(t, "[feedback]\nmax_retirements = -1\n"), dirs, "/home/u"); err == nil || !strings.Contains(err.Error(), "feedback.max_retirements") {
		t.Errorf("-1: %v", err)
	}
}

func TestStaleShowsKey(t *testing.T) {
	if got := Default(dirs, "/home/u").Feedback.StaleShows; got != 10 {
		t.Errorf("default %d", got)
	}
	cfg, err := Load(write(t, "[feedback]\nstale_shows = 1\n"), dirs, "/home/u")
	if err != nil || cfg.Feedback.StaleShows != 1 {
		t.Errorf("1: %v %d", err, cfg.Feedback.StaleShows)
	}
	if _, err := Load(write(t, "[feedback]\nstale_shows = 0\n"), dirs, "/home/u"); err == nil || !strings.Contains(err.Error(), "feedback.stale_shows") {
		t.Errorf("0: %v", err)
	}
}

// The boost's weight k and priors α, β: defaults 0.1, 3 and 9; k = 0 turns
// the boost off, a negative k or a prior of 0 is refused.
func TestBoostKeysValidate(t *testing.T) {
	f := Default(dirs, "/home/u").Feedback
	if f.BoostWeight != 0.1 || f.PriorCredits != 3 || f.PriorMisses != 9 {
		t.Errorf("defaults %v %v %v", f.BoostWeight, f.PriorCredits, f.PriorMisses)
	}
	cfg, err := Load(write(t, "[feedback]\nboost_weight = 0\nprior_credits = 0.5\nprior_misses = 20\n"), dirs, "/home/u")
	if err != nil || cfg.Feedback.BoostWeight != 0 || cfg.Feedback.PriorCredits != 0.5 || cfg.Feedback.PriorMisses != 20 {
		t.Errorf("read: %v %+v", err, cfg.Feedback)
	}
	for body, key := range map[string]string{
		"boost_weight = -0.1": "feedback.boost_weight",
		"prior_credits = 0":   "feedback.prior_credits",
		"prior_credits = -1":  "feedback.prior_credits",
		"prior_misses = 0":    "feedback.prior_misses",
	} {
		if _, err := Load(write(t, "[feedback]\n"+body+"\n"), dirs, "/home/u"); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: got %v, want an error naming %s", body, err, key)
		}
	}
}

// server.runtime: docker (the default, "" in the file) or native, which
// runs on linux-amd64 only and serves this PC only.
func TestRuntimeConfigKey(t *testing.T) {
	native := "[server]\nruntime = \"native\"\n"
	cfg, err := Parse([]byte(native), "config.toml", dirs, "/home/u")
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		if err != nil || cfg.RuntimeKind() != "native" {
			t.Fatalf("native on linux-amd64: %v %q", err, cfg.RuntimeKind())
		}
	} else if err == nil {
		t.Fatalf("native loaded on %s-%s", runtime.GOOS, runtime.GOARCH)
	}
	cfg = Default(dirs, "/home/u")
	cfg.Server.Runtime = "native"
	if err := cfg.ValidateOn("linux", "amd64"); err != nil {
		t.Errorf("native on linux-amd64: %v", err)
	}
	for _, p := range [][2]string{{"linux", "arm64"}, {"darwin", "arm64"}, {"windows", "amd64"}} {
		err := cfg.ValidateOn(p[0], p[1])
		if err == nil || !strings.Contains(err.Error(), "server.runtime: the native runtime runs on Linux x86_64 (linux-amd64) only in this release") {
			t.Errorf("native on %s-%s: %v", p[0], p[1], err)
		}
	}
	cfg.Network.Exposure, cfg.Network.BindIP = "private", "192.168.1.10"
	if err := cfg.ValidateOn("linux", "amd64"); err == nil || !strings.Contains(err.Error(),
		`server.runtime = "native" serves this PC only (loopback) in this release: private exposure needs the docker runtime`) {
		t.Errorf("native with private exposure: %v", err)
	}
	cfg = Default(dirs, "/home/u")
	cfg.Server.Runtime = "podman"
	if err := cfg.ValidateOn("linux", "amd64"); err == nil || !strings.Contains(err.Error(), "server.runtime") {
		t.Errorf("podman: %v", err)
	}
	for _, kind := range []string{"", "docker"} {
		cfg.Server.Runtime = kind
		if err := cfg.ValidateOn("darwin", "arm64"); err != nil || cfg.RuntimeKind() != "docker" {
			t.Errorf("runtime %q: %v %q", kind, err, cfg.RuntimeKind())
		}
	}
	raw, err := json.Marshal(Default(dirs, "/home/u"))
	if err != nil || !strings.Contains(string(raw), `"runtime":""`) {
		t.Errorf("defaults' JSON: %s %v", raw, err)
	}
}

// A server-only host serves other PCs, which native cannot (loopback
// only): the pair is refused with setup's message (ruling R38); native
// with the default role still validates.
func TestNativeServerOnlyConfigIsRefused(t *testing.T) {
	cfg := Default(dirs, "/home/u")
	cfg.Server.Runtime = "native"
	if err := cfg.ValidateOn("linux", "amd64"); err != nil {
		t.Fatalf("native, role %q: %v", cfg.Server.Role, err)
	}
	cfg.Server.Role = RoleServer
	err := cfg.ValidateOn("linux", "amd64")
	want := "server.role: a server-only host serves other PCs, which the native runtime cannot yet (loopback only): use the docker runtime there"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("native server-only: %v\nwant %q", err, want)
	}
	if _, err := Parse([]byte("[server]\nruntime = \"native\"\nrole = \"server\"\n"), "config.toml", dirs, "/home/u"); err == nil || !strings.Contains(err.Error(), NativeServerOnly) {
		t.Errorf("parsed: %v", err)
	}
	cfg.Server.Runtime = "docker"
	if err := cfg.ValidateOn("linux", "amd64"); err != nil {
		t.Errorf("docker server-only: %v", err)
	}
}

// SetRuntime writes native, and docker by removing the key: a Docker
// install's config.toml never holds one.
func TestSetRuntime(t *testing.T) {
	in := "# mine\n[server]\nbank = \"b\"\n"
	got, err := SetRuntime([]byte(in), "native")
	if want := "# mine\n[server]\nruntime = \"native\"\nbank = \"b\"\n"; err != nil || string(got) != want {
		t.Fatalf("native: %q %v, want %q", got, err, want)
	}
	back, err := SetRuntime(got, "docker")
	if err != nil || string(back) != in {
		t.Errorf("docker: %q %v, want %q", back, err, in)
	}
	if _, err := SetRuntime([]byte(in), "podman"); err == nil {
		t.Error("podman written")
	}
}
