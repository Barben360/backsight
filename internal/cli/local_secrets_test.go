package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/testtool"
)

// plantLocalSecrets puts values no redaction rule knows into secrets.toml
// and the server's .env, beside a quoted .env value that is no secret.
func plantLocalSecrets(t *testing.T, home string) (tomlValue, envValue, baseURL string) {
	t.Helper()
	tomlValue = "correct" + "horsebatterystaple" // letters only: no rule's shape
	envValue = "lemon" + "meringuepie"
	baseURL = "https://llm.example.org/v1"
	if err := config.UpdateSecrets(secretsFile(home), func(s *config.Secrets) error { return s.Set("llm_api_key", tomlValue) }); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(composeDir(home), 0o700)
	env := "BACKSIGHT_EXTRA_TOKEN=" + envValue + "\nHINDSIGHT_API_LLM_BASE_URL='" + baseURL + "'\n"
	if err := os.WriteFile(filepath.Join(composeDir(home), ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	return tomlValue, envValue, baseURL
}

// The exact values of secrets.toml and the server's .env never reach a
// retained segment, whatever their shape; other .env values are kept.
func TestRetainRedactsLocalSecretValues(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	tomlValue, envValue, baseURL := plantLocalSecrets(t, home)
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	withClock(t, at)
	p := filepath.Join(home, ".claude", "projects", "-home-u-billing", "sec.jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	text := "my llm key is " + tomlValue + ", the server token " + envValue + ", base " + baseURL
	line := `{"type":"user","uuid":"sec-u1","sessionId":"sec","cwd":"` + testtool.JSONPath(filepath.Join(home, "billing")) +
		`","timestamp":"` + at.Format(time.RFC3339) + `","message":{"role":"user","content":"` + text + `"}}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "retain", "flush", "--session", "sec")
	if code != output.OK || len(retainedContents) != 1 {
		t.Fatalf("exit %d, %d items\n%s\n%s", code, len(retainedContents), out, errOut)
	}
	got := retainedContents[0]
	if strings.Contains(got, tomlValue) || strings.Contains(got, envValue) || strings.Count(got, "[redacted:local_secret]") != 2 {
		t.Errorf("a local secret reached the segment: %s", got)
	}
	if !strings.Contains(got, baseURL) || !strings.Contains(got, "my llm key is") {
		t.Errorf("the text around the secrets was lost: %s", got)
	}
}

// store and memory_store refuse a note holding a local secret value:
// nothing is sent or queued.
func TestStoreRefusesLocalSecretValues(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	tomlValue, envValue, _ := plantLocalSecrets(t, home)
	for _, v := range []string{tomlValue, envValue} {
		code, out, errOut := run(t, "store", "--kind", "lesson", "--project", "p", "The", "key", "is", v)
		if code != output.Usage || strings.Contains(out+errOut, v) || !strings.Contains(out+errOut, "local_secret") {
			t.Errorf("exit %d %q %q", code, out, errOut)
		}
	}
	if retained.Load() != 0 || len(retainedContents) != 0 {
		t.Errorf("a note was sent: %q", retainedContents)
	}
	if n, _ := notesOutbox(pathsOf(home)).Pending(); n != 0 {
		t.Errorf("%d notes queued", n)
	}
}

// memory scan finds the backup key and the local secret values in stored
// memories, with the same rules as retain.
func TestMemoryScanFindsLocalSecretsAndTheBackupKey(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	tomlValue, _, _ := plantLocalSecrets(t, home)
	age := "AGE-" + "SECRET-KEY-1" + strings.Repeat("QZ7X9KMPL2", 5) + "W3RTY4UV"
	listAnswer["world"] = `[{"id":"k1","text":"restore with ` + age + `","fact_type":"world","tags":["project:infra"]},
		{"id":"k2","text":"the llm key is ` + tomlValue + `","fact_type":"world","tags":["project:infra"]},
		{"id":"k3","text":"commit 3f9a1c7e5b2d4f603f9a1c7e5b2d4f603f9a1c7e fixed the backup","fact_type":"world","tags":["project:infra"]}]`
	t.Cleanup(func() { listAnswer["world"] = "" })
	code, out, _ := run(t, "memory", "scan")
	if code != output.OK || !strings.Contains(out, "backsight forget k1 --purge") || !strings.Contains(out, "backsight forget k2 --purge") ||
		strings.Contains(out, "k3") || strings.Contains(out, age) || strings.Contains(out, tomlValue) {
		t.Errorf("exit %d\n%s", code, out)
	}
}
