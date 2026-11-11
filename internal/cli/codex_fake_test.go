package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/codexcfg"
	"github.com/Barben360/backsight/internal/harness"
)

// fakeCodexCLI is the codex command as Codex 0.167 answers it (recorded in
// internal/testdata/codex/0.167.0/config/ and by block 3's probes), on the
// Codex home of each call (CODEX_HOME, else ~/.codex): it writes
// config.toml's [marketplaces] and [plugins] tables as Codex does, copies
// a plugin into plugins/cache/, and removes both again, leaving the empty
// cache folder and the [hooks.state] tables (checks/6-removal).
type fakeCodexCLI struct {
	t        *testing.T
	home     string
	version  string
	mcp      string // codex mcp list --json
	memories bool   // codex features list
	missing  bool
	failOn   string
	failOnce bool // with failOn: fail once, then pass
	calls    []codexCall
}

type codexCall struct{ line, home string }

func withCodex(t *testing.T, home string) *fakeCodexCLI {
	t.Helper()
	f := &fakeCodexCLI{t: t, home: home, version: "0.167.0", mcp: "[]"}
	old := codexRunner
	codexRunner = f
	t.Cleanup(func() { codexRunner = old })
	return f
}

// mutating are the command lines that change Codex's install.
func (f *fakeCodexCLI) mutating() []string {
	var out []string
	for _, c := range f.calls {
		if c.line != "--version" && !strings.HasSuffix(c.line, "list --json") && c.line != "features list" {
			out = append(out, c.line)
		}
	}
	return out
}

func (f *fakeCodexCLI) codexHome(env []string) string {
	dir := filepath.Join(f.home, ".codex")
	for _, kv := range codexEnviron(env) {
		if v, ok := strings.CutPrefix(kv, "CODEX_HOME="); ok && v != "" {
			dir = v
		}
	}
	return dir
}

func (f *fakeCodexCLI) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	if f.missing {
		return nil, errCodexMissing
	}
	line := strings.Join(args, " ")
	dir := f.codexHome(env)
	f.calls = append(f.calls, codexCall{line, dir})
	if f.failOn != "" && strings.HasPrefix(line, f.failOn) {
		if f.failOnce {
			f.failOn = ""
		}
		return nil, errors.New("codex " + line + ": failed")
	}
	if dir != filepath.Join(f.home, ".codex") {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return nil, errors.New("CODEX_HOME points to " + dir + ", but that path does not exist")
		}
	}
	cfgPath := filepath.Join(dir, "config.toml")
	cfg, _ := os.ReadFile(cfgPath)
	write := func(b []byte) {
		_ = os.MkdirAll(dir, 0o700)
		if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	appendTable := func(header, body string) {
		if strings.Contains(string(cfg), header+"\n") {
			return
		}
		add := header + "\n" + body
		if len(strings.TrimSpace(string(cfg))) > 0 {
			add = "\n" + add
		}
		write(append(cfg, add...))
	}
	removeTable := func(path []string) {
		out, _, err := codexcfg.RemoveTables(cfg, func(p []string) bool { return slices.Equal(p, path) })
		if err != nil {
			f.t.Fatal(err)
		}
		write(out)
	}
	source := func() string {
		v, _, _ := codexcfg.Get(cfg, []string{"marketplaces", "backsight", "source"})
		var s string
		_ = json.Unmarshal([]byte(v), &s)
		return s
	}
	switch {
	case line == "--version":
		return []byte("codex-cli " + f.version + "\n"), nil
	case line == "mcp list --json":
		return []byte(f.mcp), nil
	case line == "features list":
		on := "false"
		if f.memories {
			on = "true"
		}
		return []byte("hooks                                    stable             true\nmemories                                 stable             " + on + "\n"), nil
	case line == "plugin marketplace list --json":
		if s := source(); s != "" {
			return json.Marshal(map[string]any{"marketplaces": []any{map[string]any{"name": "backsight", "root": s, "marketplaceSource": map[string]string{"sourceType": "local", "source": s}}}})
		}
		return []byte(`{"marketplaces": []}`), nil
	case line == "plugin list --json":
		var list []any
		for _, id := range harness.CodexPluginIDs() {
			if _, ok, _ := codexcfg.Get(cfg, []string{"plugins", id, "enabled"}); ok {
				name, _, _ := strings.Cut(id, "@")
				list = append(list, map[string]any{"pluginId": id, "version": f.version0(source(), name), "installed": true, "enabled": true,
					"source": map[string]string{"source": "local", "path": filepath.Join(source(), "plugins", name)}})
			}
		}
		return json.Marshal(map[string]any{"installed": list, "available": []any{}})
	case len(args) == 4 && args[0] == "plugin" && args[1] == "marketplace" && args[2] == "add":
		if s := source(); s != "" && s != args[3] {
			return nil, errors.New("marketplace 'backsight' is already added from a different source")
		}
		appendTable("[marketplaces.backsight]", "source_type = \"local\"\nsource = \""+args[3]+"\"\n")
	case line == "plugin marketplace remove backsight":
		if source() == "" {
			return nil, errors.New("marketplace `backsight` is not configured or installed")
		}
		removeTable([]string{"marketplaces", "backsight"})
	case len(args) == 3 && args[0] == "plugin" && args[1] == "add":
		name, _, _ := strings.Cut(args[2], "@")
		_ = os.MkdirAll(filepath.Join(dir, "plugins", "cache", "backsight", name, f.version0(source(), name)), 0o755)
		appendTable(`[plugins."`+args[2]+`"]`, "enabled = true\n")
	case len(args) == 3 && args[0] == "plugin" && args[1] == "remove":
		name, _, _ := strings.Cut(args[2], "@")
		_ = os.RemoveAll(filepath.Join(dir, "plugins", "cache", "backsight", name))
		removeTable([]string{"plugins", args[2]})
	default:
		return nil, errors.New("unexpected codex " + line)
	}
	return []byte("ok\n"), nil
}

// version0 is the version of a plugin in the marketplace at root.
func (f *fakeCodexCLI) version0(root, name string) string {
	var m struct{ Version string }
	raw, _ := os.ReadFile(filepath.Join(root, "plugins", name, ".claude-plugin", "plugin.json"))
	_ = json.Unmarshal(raw, &m)
	return m.Version
}

// trustAll writes the [hooks.state] tables Codex writes once the user
// trusted every backsight hook (checks/3-two-plugins/config-trusted.toml).
func trustAll(t *testing.T, codexHome string) {
	t.Helper()
	cfgPath := filepath.Join(codexHome, "config.toml")
	cfg, _ := os.ReadFile(cfgPath)
	var b strings.Builder
	b.Write(cfg)
	if !strings.Contains(string(cfg), "[hooks.state]\n") {
		b.WriteString("\n[hooks.state]\n")
	}
	for _, key := range harness.CodexTrustKeys() {
		b.WriteString("\n[hooks.state.\"" + key + "\"]\ntrusted_hash = \"sha256:aa\"\ntrusted_content_hash = \"sha256:bb\"\n")
	}
	if err := os.WriteFile(cfgPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}
