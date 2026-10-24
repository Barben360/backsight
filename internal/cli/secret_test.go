package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
)

func withStdin(t *testing.T, content string) {
	t.Helper()
	old, oldTTY := stdin, stdinIsTerminal
	stdin, stdinIsTerminal = strings.NewReader(content), func() bool { return false }
	t.Cleanup(func() { stdin, stdinIsTerminal = old, oldTTY })
}

func secretsFile(home string) string {
	return filepath.Join(home, "backsight", "config", "secrets.toml")
}

func TestSecretSetFromPipe(t *testing.T) {
	home := isolate(t)
	withStdin(t, "fakevalue1\nsecond line ignored\n")
	code, out, errOut := run(t, "secret", "set", "llm_api_key")
	if code != output.OK {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if strings.Contains(out+errOut, "fakevalue1") {
		t.Error("value echoed")
	}
	s, err := config.LoadSecrets(secretsFile(home))
	if err != nil || s.LLMAPIKey != "fakevalue1" {
		t.Errorf("%v %v", s.Names(), err)
	}
}

func TestSecretSetFromEnvAndFile(t *testing.T) {
	home := isolate(t)
	t.Setenv("MY_KEY", " fakevalue2\n")
	if code, out, errOut := run(t, "secret", "set", "llm_api_key", "--from-env", "MY_KEY"); code != output.OK {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	f := filepath.Join(home, "key.txt")
	_ = os.WriteFile(f, []byte("fakevalue3\n"), 0o600)
	if code, _, errOut := run(t, "secret", "set", "ui_key", "--from-file", f); code != output.OK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	s, _ := config.LoadSecrets(secretsFile(home))
	if s.LLMAPIKey != "fakevalue2" || s.UIKey != "fakevalue3" {
		t.Errorf("%v", s.Names())
	}
}

func TestSecretSetRefusals(t *testing.T) {
	home := isolate(t)
	keyFile := filepath.Join(home, "key.txt")
	_ = os.WriteFile(keyFile, []byte("fakevalue4\n"), 0o600)
	cases := map[string][]string{
		"unknown name":   {"secret", "set", "password", "--from-env", "HOME"},
		"unset env":      {"secret", "set", "llm_api_key", "--from-env", "BACKSIGHT_NO_SUCH_VAR"},
		"both sources":   {"secret", "set", "llm_api_key", "--from-env", "HOME", "--from-file", keyFile},
		"missing name":   {"secret", "set"},
		"value argument": {"secret", "set", "llm_api_key", "fakevalue4"},
	}
	for name, args := range cases {
		withStdin(t, "")
		code, out, errOut := run(t, args...)
		if code == output.OK || strings.Contains(out+errOut, "fakevalue4") {
			t.Errorf("%s: exit %d, out %q err %q", name, code, out, errOut)
		}
	}
	for _, args := range [][]string{{"secret", "set"}, {"secret", "set", "llm_api_key", "fakevalue4"}, {"secret", "set", "password", "--from-env", "HOME"}} {
		if code, _, _ := run(t, args...); code != output.Usage {
			t.Errorf("%v: exit %d, want %d", args, code, output.Usage)
		}
	}
	withStdin(t, "\n")
	if code, _, _ := run(t, "secret", "set", "llm_api_key"); code == output.OK {
		t.Error("an empty value from stdin was accepted")
	}
}

func TestSecretListNeverShowsValues(t *testing.T) {
	home := isolate(t)
	_ = config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error { return s.Set("api_key", "fakevalue5") })
	code, out, _ := run(t, "secret", "list", "--json")
	var env struct {
		Data map[string]bool `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d: %v\n%s", code, err, out)
	}
	if !env.Data["api_key"] || env.Data["llm_api_key"] || len(env.Data) != 5 || strings.Contains(out, "fakevalue5") {
		t.Errorf("%s", out)
	}
	_, text, _ := run(t, "secret", "list")
	if !strings.Contains(text, "api_key") || !strings.Contains(text, "set") || strings.Contains(text, "fakevalue5") {
		t.Errorf("%q", text)
	}
}
