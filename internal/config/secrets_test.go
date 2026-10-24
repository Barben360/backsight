package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const secretsTOML = `api_key = "hs-api-SECRET1"
ui_key = "hs-ui-SECRET2"
db_password = "db-SECRET3"
llm_api_key = "sk-SECRET4"
backup_identity = "AGE-SECRET-KEY-SECRET5"
`

func writeSecrets(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secrets.toml")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask may have narrowed it
		t.Fatal(err)
	}
	return p
}

func TestLoadSecrets(t *testing.T) {
	s, err := LoadSecrets(writeSecrets(t, secretsTOML, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if s.APIKey != "hs-api-SECRET1" || s.UIKey != "hs-ui-SECRET2" || s.DBPassword != "db-SECRET3" || s.LLMAPIKey != "sk-SECRET4" || s.BackupIdentity != "AGE-SECRET-KEY-SECRET5" {
		t.Errorf("fields not loaded: %v", s.Names())
	}
}

func TestLoadSecretsRefusesReadableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes only")
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		_, err := LoadSecrets(writeSecrets(t, secretsTOML, mode))
		if err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %o: want a refusal with the fix, got %v", mode, err)
		}
	}
}

func TestLoadSecretsRefusesUnknownKey(t *testing.T) {
	_, err := LoadSecrets(writeSecrets(t, "api_kee = \"x\"\n", 0o600))
	if err == nil || !strings.Contains(err.Error(), "api_kee") {
		t.Errorf("got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "\"x\"") {
		t.Errorf("a decode error must not echo values: %v", err)
	}
}

// Secrets must never reach a log, a terminal or a JSON report.
func TestSecretsNeverPrinted(t *testing.T) {
	s, err := LoadSecrets(writeSecrets(t, secretsTOML, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(s)
	jsPtr, _ := json.Marshal(&s)
	outs := []string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s", s, s, s, s), fmt.Sprintf("%+v", &s), string(js), string(jsPtr)}
	for _, out := range outs {
		if strings.Contains(out, "SECRET") {
			t.Errorf("secret leaked: %s", out)
		}
	}
	if !strings.Contains(string(js), "api_key") {
		t.Errorf("JSON should still list which secrets are set: %s", js)
	}
}

func TestSecretsNames(t *testing.T) {
	s := Secrets{APIKey: "a", LLMAPIKey: "b"}
	got := s.Names()
	if got["api_key"] != true || got["llm_api_key"] != true || got["ui_key"] != false || len(got) != 5 {
		t.Errorf("%v", got)
	}
}
