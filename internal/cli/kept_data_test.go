package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/testtool"
)

// uninstall --stop --purge-local keeps the server and its .env; server
// install then adopts the three server keys from it, so the kept
// database and the server's API key work again (M5 review rules,
// "Setting up again over kept data").
func TestServerInstallAfterPurgeLocalAdoptsTheKeptKeys(t *testing.T) {
	testtool.SkipWindows(t, "Task 4: the shim and the PATH link")
	m := installMachine(t)
	d := pathsOf(m.home)
	before, err := config.LoadSecrets(d.SecretsFile())
	if err != nil {
		t.Fatal(err)
	}
	if code, _, out := uninstall(t, "--stop", "--purge-local"); code != output.OK {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	if onDisk(d.SecretsFile()) || onDisk(d.ConfigFile()) || !onDisk(filepath.Join(d.Data, "server", ".env")) {
		t.Fatal("the fixture is not a purge-local over a kept server")
	}
	m.dk.volumeExists = true
	port := fakeHindsight(t)
	requireKey.Store(true) // the kept server runs on the kept key only
	acceptedKey.Store(before.APIKey)
	t.Setenv("LLM", "fakellmkey")
	if code, _, errOut := run(t, "secret", "set", "llm_api_key", "--from-env", "LLM"); code != output.OK {
		t.Fatal(errOut)
	}

	code, out, errOut := run(t, "server", "install", "--plan", "--api-port", strconv.Itoa(port), "--ui-port", "19999")
	if code != output.OK || !strings.Contains(out, "would adopt api_key, db_password, ui_key from the kept "+filepath.Join(d.Data, "server", ".env")) {
		t.Fatalf("plan: exit %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = run(t, "server", "install", "--api-port", strconv.Itoa(port), "--ui-port", "19999")
	if code != output.OK || !strings.Contains(out, "adopted api_key, db_password, ui_key from the kept") || strings.Contains(out, "generated") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	after, err := config.LoadSecrets(d.SecretsFile())
	if err != nil || after.APIKey != before.APIKey || after.DBPassword != before.DBPassword || after.UIKey != before.UIKey || after.LLMAPIKey != "fakellmkey" {
		t.Errorf("secrets.toml %v %v", after, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(d.SecretsFile()); fi.Mode().Perm() != 0o600 {
			t.Errorf("secrets.toml mode %o", fi.Mode().Perm())
		}
	}
	if strings.Contains(out+errOut, before.DBPassword) || strings.Contains(out+errOut, before.APIKey) {
		t.Error("a kept key reached the output")
	}
}

// A secrets.toml value always wins over the kept .env's; only the keys
// it lacks are adopted.
func TestServerInstallNeverOverwritesSecretsTomlFromTheEnv(t *testing.T) {
	home := devInstall(t, true)
	withCompose(t).volumeExists = true
	pointAt(t, home, fakeHindsight(t))
	d := pathsOf(home)
	cfg := config.Default(d, home)
	kept := config.Secrets{APIKey: "keptapikey", UIKey: "keptuikey", DBPassword: "keptdbpassword123456", LLMAPIKey: "fakellmkey"}
	files, err := server.Render(cfg, kept, "box")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Write(cfg.Server.ComposeDir, files); err != nil {
		t.Fatal(err)
	}
	mine, _ := config.LoadSecrets(d.SecretsFile())
	if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error { s.UIKey = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "server", "install")
	if code != output.OK || !strings.Contains(out, "adopted ui_key from the kept") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	got, _ := config.LoadSecrets(d.SecretsFile())
	if got.APIKey != mine.APIKey || got.DBPassword != mine.DBPassword || got.UIKey != "keptuikey" {
		t.Errorf("secrets.toml %+v", got.Names())
	}
}

// Without a db_password anywhere, the refusal offers getting it back
// first, and deleting the memories last.
func TestServerInstallOffersDeletingTheMemoriesLast(t *testing.T) {
	isolate(t)
	f := withCompose(t)
	f.volumeExists = true
	t.Setenv("LLM", "fakellmkey")
	run(t, "secret", "set", "llm_api_key", "--from-env", "LLM")
	code, _, errOut := run(t, "server", "install", "--api-port", strconv.Itoa(closedPort(t)), "--ui-port", "19999")
	restore, set, del := strings.Index(errOut, "restore the secrets.toml"), strings.Index(errOut, "secret set db_password"), strings.Index(errOut, "docker volume rm")
	if code != output.Blocked || restore < 0 || set < 0 || del < 0 || del < restore || del < set || !strings.Contains(errOut, ".env") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

// setup over a server uninstall --purge-local kept (its volume and .env,
// no secrets.toml): its server step reaches the kept data on the kept key.
func TestSetupAgainOverKeptDataReachesIt(t *testing.T) {
	home, f := setupMachine(t)
	f.real["server install"] = true
	d := pathsOf(home)
	kept := config.Secrets{APIKey: "keptapikey", UIKey: "keptuikey", DBPassword: "keptdbpassword123456", LLMAPIKey: "oldllmkey"}
	files, err := server.Render(config.Default(d, home), kept, "box")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Write(filepath.Join(d.Data, "server"), files); err != nil {
		t.Fatal(err)
	}
	withCompose(t).volumeExists = true
	port := fakeHindsight(t)
	requireKey.Store(true)
	acceptedKey.Store(kept.APIKey)
	withLLMKey(t, home)
	code, out, errOut := run(t, "setup", "--answer", "provider=deepseek", "--answer", "consent=yes", "--answer", "backups=off",
		"--answer", "api_port="+strconv.Itoa(port), "--answer", "ui_port=19999", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.OK || !hasStep(env.Data.Steps, "server", "done", "Hindsight 1.1.0") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	got, _ := config.LoadSecrets(d.SecretsFile())
	if got.APIKey != kept.APIKey || got.DBPassword != kept.DBPassword || got.UIKey != kept.UIKey || got.LLMAPIKey != "fakellmkey" {
		t.Errorf("secrets.toml %+v", got.Names())
	}
}
