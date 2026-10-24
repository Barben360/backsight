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
	cfg, err := Load(write(t, "language = \"fr\"\n[recall]\nmin_score = 0.6\n"), dirs, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	want := Default(dirs, "/home/u")
	want.Language, want.Recall.MinScore = "fr", 0.6
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
		{"invalid value", "language = \"de\"\n", "language"},
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
		{"language", func(c *Config) { c.Language = "de" }, "language"},
		{"mode", func(c *Config) { c.Server.Mode = "cloud" }, "server.mode"},
		{"url scheme", func(c *Config) { c.Server.URL = "ftp://x" }, "server.url"},
		{"url empty", func(c *Config) { c.Server.URL = "" }, "server.url"},
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
