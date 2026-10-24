package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Low-entropy fake values: secret scanners must not flag this file.
const secretsTOML = `api_key = "fakevalue1"
ui_key = "fakevalue2"
db_password = "fakevalue3"
llm_api_key = "fakevalue4"
backup_identity = "fakevalue5"
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
	if s.APIKey != "fakevalue1" || s.UIKey != "fakevalue2" || s.DBPassword != "fakevalue3" || s.LLMAPIKey != "fakevalue4" || s.BackupIdentity != "fakevalue5" {
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
		if strings.Contains(out, "fakevalue") {
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

func TestLoadSecretsMissingIsNotExist(t *testing.T) {
	if _, err := LoadSecrets(filepath.Join(t.TempDir(), "secrets.toml")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("want fs.ErrNotExist, got %v", err)
	}
}

// Decode errors can quote the offending value; none may reach the message.
func TestLoadSecretsErrorsNeverEchoValues(t *testing.T) {
	for name, content := range map[string]string{
		"unterminated": "api_key = \"fakevalue7\n",
		"type":         "api_key = [\"fakevalue7\"]\n",
		"bare value":   "api_key = fakevalue7\n",
	} {
		_, err := LoadSecrets(writeSecrets(t, content, 0o600))
		if err == nil || strings.Contains(err.Error(), "fakevalue7") || !strings.Contains(err.Error(), "secrets.toml") {
			t.Errorf("%s: want an error naming the file without the value, got %v", name, err)
		}
	}
}
