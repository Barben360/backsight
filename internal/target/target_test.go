package target

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/reference"
)

func dirs(t *testing.T) paths.Dirs {
	root := t.TempDir()
	return paths.Dirs{Data: filepath.Join(root, "d"), Config: filepath.Join(root, "c"), State: filepath.Join(root, "s")}
}

func put(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(p, mode)
}

func TestFromConfigMissing(t *testing.T) {
	if _, err := FromConfig(dirs(t), "/home/u"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("want fs.ErrNotExist, got %v", err)
	}
}

func TestFromConfigWithSecrets(t *testing.T) {
	d := dirs(t)
	put(t, d.ConfigFile(), "[server]\nurl = \"http://10.0.0.2:8888\"\nbank = \"b\"\n", 0o600)
	put(t, d.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	tg, err := FromConfig(d, "/home/u")
	if err != nil || tg.URL != "http://10.0.0.2:8888" || tg.Bank != "b" || tg.Source != "config" || tg.Token != "fakevalue" {
		t.Fatalf("%+v %v", tg, err)
	}
	if out := fmt.Sprintf("%v %+v", tg, tg); strings.Contains(out, "fakevalue") {
		t.Errorf("token printed: %s", out)
	}
}

// A config without secrets.toml is valid: the server may run without a key.
func TestFromConfigWithoutSecrets(t *testing.T) {
	d := dirs(t)
	put(t, d.ConfigFile(), "", 0o600)
	tg, err := FromConfig(d, "/home/u")
	if err != nil || tg.Token != "" || tg.URL != "http://127.0.0.1:8888" {
		t.Errorf("%+v %v", tg, err)
	}
}

func TestFromConfigErrors(t *testing.T) {
	d := dirs(t)
	put(t, d.ConfigFile(), "[recall]\nmin_scor = 1\n", 0o600)
	var ce *ConfigError
	if _, err := FromConfig(d, "/home/u"); !errors.As(err, &ce) || ce.File != d.ConfigFile() {
		t.Errorf("want a ConfigError on config.toml, got %v", err)
	}
	put(t, d.ConfigFile(), "", 0o600)
	put(t, d.SecretsFile(), "api_key = \"fakevalue\"\n", 0o644)
	if _, err := FromConfig(d, "/home/u"); !errors.As(err, &ce) || ce.File != d.SecretsFile() || strings.Contains(err.Error(), "fakevalue") {
		t.Errorf("want a ConfigError on secrets.toml, got %v", err)
	}
}

func TestFromReference(t *testing.T) {
	if FromReference(nil) != nil || FromReference(&reference.Install{Bank: "b"}) != nil {
		t.Error("no URL, no target")
	}
	tg := FromReference(&reference.Install{URL: "http://127.0.0.1:8888", Bank: "b", Token: "fakevalue"})
	if tg == nil || tg.Source != "reference" || tg.Token != "fakevalue" {
		t.Errorf("%+v", tg)
	}
}

func TestDisplayAndCleartext(t *testing.T) {
	cases := []struct {
		url, display string
		cleartext    bool
	}{
		{"http://127.0.0.1:8888", "http://127.0.0.1:8888", false},
		{"http://localhost:8888", "http://localhost:8888", false},
		{"http://[::1]:8888", "http://[::1]:8888", false},
		{"https://mem.example:443", "https://mem.example:443", false},
		{"http://192.168.1.10:8888", "http://192.168.1.10:8888", true},
		{"http://u:fakevalue@10.0.0.2:8888/x", "http://u:xxxxx@10.0.0.2:8888/x", true},
	}
	for _, c := range cases {
		tg := Target{URL: c.url}
		if tg.Display() != c.display || tg.Cleartext() != c.cleartext {
			t.Errorf("%s: display %q cleartext %v", c.url, tg.Display(), tg.Cleartext())
		}
	}
}

// A tailnet host (Tailscale's ranges, or a MagicDNS name): WireGuard
// encrypts the link. A LAN address, a plain name or loopback is not one.
func TestTailnet(t *testing.T) {
	for url, want := range map[string]bool{
		"http://100.101.102.103:8888":   true,
		"http://[fd7a:115c:a1e0::1]:88": true,
		"http://host.tail1234.ts.net:8": true,
		"http://192.168.1.10:8888":      false,
		"http://100.128.0.1:8888":       false, // outside 100.64.0.0/10
		"http://nas.local:8888":         false,
		"http://127.0.0.1:8888":         false,
		"http://evil-ts.net.example:80": false,
	} {
		if got := (Target{URL: url}).Tailnet(); got != want {
			t.Errorf("%s: tailnet %v, want %v", url, got, want)
		}
	}
}
